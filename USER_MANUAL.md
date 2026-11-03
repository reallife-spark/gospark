---
title: goSpark — user manual
updated: 2026-10-30
---

# goSpark — user manual

> The manual for **goSpark**: the Go control and service plane that sits
> beside the sparknet fabric. It decides where a prompt is resolved, routes
> signed cluster jobs, serves the node's API, streams GPU telemetry, moves KV
> blocks between nodes, and exposes the read-only observability surface.
>
> It does **not** run a model. That is the compute plane's job, and the
> boundary is a decision, not an accident — see [§2](#2-the-boundary).

---

## Table of contents

1. [What goSpark is](#1-what-gospark-is)
2. [The boundary](#2-the-boundary)
3. [Orientation](#3-orientation)
4. [The authority model](#4-the-authority-model)
5. [The packages](#5-the-packages)
6. [The binaries](#6-the-binaries)
7. [The seams](#7-the-seams)
8. [The contract snapshots](#8-the-contract-snapshots)
9. [Build, run, test](#9-build-run-test)
10. [The gate](#10-the-gate)
11. [How to make a change correctly](#11-how-to-make-a-change-correctly)
12. [Configuration reference](#12-configuration-reference)
13. [Troubleshooting](#13-troubleshooting)
14. [Spec vs implemented](#14-spec-vs-implemented)
15. [Glossary](#15-glossary)

---

## 1. What goSpark is

goSpark is the **control and service plane** of a sparknet node, written in
Go. It runs beside the fabric's Python services in the same node container and
shares the NATS bus with them — not a process.

What it does:

- **verifies** the fabric's signed envelopes, in Go, byte-for-byte identically
  to the fabric's Python implementation;
- **dispatches** signed cluster jobs to the nodes that can serve them, ranked
  by live GPU telemetry;
- **serves** the node's OpenAI-compatible API through a Go gateway in front of
  the local engine;
- **collects** GPU telemetry (memory, utilisation, health) through a cgo
  sidecar;
- **moves** NVFP4 KV blocks between nodes over HTTP/3 (QUIC);
- **snapshots** storage and knowledge into signed, content-addressed backups;
- **exposes** a read-only observability surface over the admin routes — as
  Prometheus metrics, and as an MCP server.

What it deliberately does not do: run a model, hold a tensor, tokenize, or
touch CUDA in the control plane.

**Who this manual is for:** a developer changing goSpark — a package, a
binary, a seam, a config file or the contract snapshot — who needs to know
where the boundary is and what the gate checks.

---

## 2. The boundary

The governing decision is [ADR 0001](docs/adr/0001-go-control-plane.md):

> Go runs the control and service plane; Python/CUDA runs the compute plane.

The reason is not aesthetic. The control plane needs concurrency, cheap
fan-out, long-lived connections and a small resident footprint; the compute
plane needs a CUDA context, a model resident in VRAM, and a Python ecosystem
of ML libraries. One binary cannot be good at both without being bad at both.

So the split is:

| Concern | Where it lives |
|---|---|
| Identity, verification, authorisation | Go — `internal/identity`, `internal/authz` |
| Dispatch, routing, admission | Go — `internal/router`, `internal/placement`, `internal/gateway` |
| The message fabric | Go — `internal/bus` (NATS/JetStream) |
| Telemetry collection | Go — `internal/gputel` (cgo sidecar) |
| KV block movement | Go — `internal/kvtransfer`, `internal/transport` |
| Model execution, CUDA, tokenisation | Python — the compute plane, over the bridge |
| The fabric's guard, anchor, vault, provenance | Python — the parent fabric (`sparknet/`) |

The two planes meet at exactly two places: the **task bridge**
(`internal/compute`, [ADR 0006](docs/adr/0006-compute-plane-task-bridge.md))
and the **embedding seam** (`internal/embed`, [ADR 0008](docs/adr/0008-ingest-embedding-seam.md)).
Both are interfaces, not imports: goSpark defines the contract, and the
compute plane implements it.

The consequence you will feel as a developer: **goSpark never imports a
model library, and the compute plane never imports a goSpark package.** If
you find yourself wanting to, you are about to cross the boundary — write the
interface instead.

---

## 3. Orientation

Read [AGENTS.md](AGENTS.md) first: it states the boundary, the commands, the
layout, the conventions and where the fabric is. This manual explains the
code.

### Where this checkout lives

goSpark is still its own repository, but it is now **hosted inside the
fabric** at `gospark/`:

```
sparknet/                 ← the fabric root (this checkout's parent)
  contracts/              the contract SSOT
  gospark/                ← this repository (its own .git)
  peer-node/  master-node/
```

The fabric's `.gitignore` excludes `/gospark/`, so the nested checkout is
never tracked twice. The node image consumes it through the **BuildKit named
context** `gospark` — the image build context is the fabric root, so the
nested `.git` and its build output are excluded by `.dockerignore` and only
`gospark/bin` and `gospark/compute` are copied into the image. The setup
script and both compose files default to `<fabric-root>/gospark`;
`GOSPARK_DIR` overrides it.

Everything in this manual is relative to `gospark/`. Run the commands from
there.

| Path | What it is |
|---|---|
| `cmd/` | The nine binaries. |
| `internal/` | The 25 packages. Nothing outside `cmd/` may import them by path. |
| `proto/sparknet/v1/` | The on-wire contract: one `.proto` per family. |
| `spec/sparknet/` | Read-only snapshots of the fabric's contract. |
| `compute/` | The reference Python/CUDA worker and its `.example` env files. |
| `docs/architecture.md` | The architecture, section by section. |
| `docs/adr/` | The 14 architectural decision records. |
| `docs/ROADMAP.md` | What is next. |
| `docs/transport.md` | The data plane's transport. |
| `configs/` | `*.example.yaml` — copy and edit; never commit the copy. |
| `eval/` | The reference eval and its corpus. |
| `deploy/` | The systemd units and the deployment notes. |
| `Makefile` | Every command. |
| `SECURITY.md`, `CONTRIBUTING.md`, `CHANGELOG.md` | The project's own rules and history. |

---

## 4. The authority model

goSpark **mirrors** the fabric's rule rather than inventing one:

> Authority comes from key possession, not from a capability grant.

`internal/authz` decides whether a signed envelope is authentic, live, and
carries the capability it claims. It fails closed: an unparseable envelope, an
expired one, a revoked one, or one whose signature does not verify, is
**refused** — never degraded into a warning.

The cross-language property that makes this work: the Go verifier and the
fabric's Python implementation must produce **the same bytes for the same
message**, and accept **the same messages**. Two details carry that:

- **Canonical JSON** (`internal/apiv1`): object keys sorted, insignificant
  whitespace removed — the bytes are deliberately identical to Python's
  `json.dumps(..., sort_keys=True, separators=(",", ":"))`.
- **Per-entry signing** ([ADR 0002](docs/adr/0002-registry-per-entry-signing.md)):
  the registry *document* is unsigned; each *entry* is signed. That is what
  lets a revoked flag live inside the signature, and it is why only the
  Master can set one.
- **The attested `department`** is part of the mirror. The fabric's
  `cross-department-share` capability is compared against the `department` in
  each **signed** registry entry, never a node's self-description, so goSpark
  carries the field through `internal/identity` and `internal/authz` as well.

The fabric's suite asserts the agreement from the Go side: it verifies the
Python authz and the Go authz against the same vectors and fails if they
disagree. **The two implementations are not allowed to drift.**

### 4.1 The admin surface

[ADR 0003](docs/adr/0003-live-admin-surface.md) records the live admin
surface, and the constraint is absolute:

> It refuses any non-loopback admin bind.

The admin surface grants full registry authority, so it binds
`127.0.0.1` only, and it is **disabled unless `admin.listen` is set**. To
reach it, bind loopback and use an SSH tunnel. The reason is worth stating:
an admin surface that can rewrite the registry is, functionally, the Master.
Making it loopback-only means the only ways in are the ones an operator
already has.

---

## 5. The packages

25 packages under `internal/`. The ones worth knowing first:

### The trust and identity core

| Package | What it is |
|---|---|
| `internal/identity` | The node identity as the fabric defines it: a projection of five fields whose `sha256` fingerprint names the node, plus an Ed25519 keypair for signing and verifying envelopes. |
| `internal/authz` | The verifier: is this envelope authentic, live, and carrying the capability it claims? Mirrors the fabric's `shared/sparknet-authz.py`. |
| `internal/apiv1` | Canonical JSON — the byte-level agreement with Python. |
| `internal/registryfile` | The on-disk registry shape, in one place, so the control plane, the agent and `sparknetctl` agree on how a registry file is read. |
| `internal/config` | YAML with environment overrides. One file, explicit defaults, no hidden global state. |

### The message fabric and the servers

| Package | What it is |
|---|---|
| `internal/bus` | The message fabric: typed, gRPC-shaped payloads over a resilient store-and-forward backbone (NATS with JetStream). |
| `internal/server` | The assembly point — the only package that knows about all of them. |
| `internal/telemetry` | The logging and metrics surface: a `slog` logger and a handful of atomic counters, with no vendor dependency, so a node's operational story is readable from its own process. |
| `internal/mesh` | The WireGuard overlay the control plane runs over. A deliberately small scaffold: it reports peer state by parsing `wg show` and reroutes by rewriting peer endpoints. |
| `internal/registryfile`, `internal/metrics`, `internal/mcp` | The disk shape, the Prometheus surface, the MCP surface. |

### The decision-making packages

| Package | What it is |
|---|---|
| `internal/router` | Decides **where a prompt is resolved**: on the edge, on a single remote teacher, or as a fan-out cluster job. It returns a *decision*, never a result — the caller proceeds. |
| `internal/placement` | Turns GPU telemetry into a dispatch decision: ranks the nodes the caller may dispatch to, against a requested floor. A **pure** engine — no I/O, so it is directly testable. See [ADR 0005](docs/adr/0005-telemetry-aware-dispatch.md). |
| `internal/gateway` | The Go API gateway in front of the compute plane: proxies inference, streams Server-Sent Events without buffering, and exposes the router's escalation decision so a client can decide whether to escalate. |
| `internal/gputel` | The GPU telemetry collector. The core is deliberately dependency-free: a `Collector` interface, a `Null` collector and a registry — so the whole package is testable with no GPU. |

### The seams

| Package | What it is |
|---|---|
| `internal/compute` | The task bridge to the Python/CUDA compute plane. |
| `internal/embed` | The embedding seam: Go's ingest pipeline talks to the compute plane's model through an interface, so ingest stays free of any model dependency. |
| `internal/ingest` | Crawl → chunk → embed. |
| `internal/kvrouter` | The KV-routing seam: the prefix tree and the prefetch policy are compute-plane; only the routing decision is here. |
| `internal/kvtransfer` | The NVFP4 KV block codec — the canonical byte form and its inverse. |
| `internal/transport` | The data plane: NVFP4 KV blocks streamed over HTTP/3 (QUIC), kept strictly separate from the control-plane bus. |
| `internal/backup` | Signed, content-addressed snapshots of a directory tree, provable and restorable without shelling out to `rsync`. |
| `internal/memory` | Per-vault typed memory: episodic, semantic, procedural, short-term and shared records, lexical retrieval, redaction, retention and export. |
| `internal/delegates` | Validated delegate plans, durable runs/events, budgets, cancellation and handoff records. |

### The rest

`internal/mcp` (a minimal MCP server over stdio exposing the read-only
admin routes), `internal/metrics` (Prometheus text exposition, version
0.0.4), `internal/testutil` (shared integration-test helpers, never imported
by production code).

---

## 6. The binaries

Nine, in `cmd/`:

| Binary | What it does |
|---|---|
| `sparknet-control` | The control plane. Signs, dispatches, serves the admin surface, ranks nodes. |
| `sparknet-agent` | The node agent. Accepts signed tasks, verifies them, and delegates to the compute plane. |
| `sparknetctl` | The operator CLI. |
| `sparknet-gpu` | The cgo GPU-telemetry sidecar (NVML + DCGM). |
| `sparknet-ingest` | The ingest driver. |
| `sparknet-backup` | The backup daemon. |
| `sparknet-mcp` | The read-only MCP server. |
| `sparknet-memory` | The per-vault memory and delegate service (loopback 4110). |
| `stub-vllm` | The CPU stand-in for the engine. |

The split between `sparknet-control` and `sparknet-agent` mirrors the
fabric's Master/peer asymmetry, but the two are **not** the same thing: the
control plane is where authority lives in the Go plane, while the agent is a
node that verifies what it is given.

`sparknet-gpu` is separate because of [ADR 0004](docs/adr/0004-gpu-telemetry-sidecar.md):
the telemetry collector needs cgo, NVML and DCGM, and linking that into the
control plane would make every binary in the repository need a CUDA-capable
build. Isolating it means the control plane stays a pure-Go binary that
builds and tests anywhere.

---

## 7. The seams

The seams are where goSpark meets the rest of the system. Each has an ADR
recording the decision and its consequences.

| Seam | ADR | Contract |
|---|---|---|
| Compute-plane task bridge | [0006](docs/adr/0006-compute-plane-task-bridge.md) | A signed task the agent delegates to the compute worker. |
| Gateway serving seam | [0007](docs/adr/0007-gateway-serving-seam.md) | Readiness, model discovery, streaming. |
| Knowledge ingest seam | [0008](docs/adr/0008-ingest-embedding-seam.md) | Crawl → chunk → embed. |
| KV-routing seam | [0009](docs/adr/0009-kv-routing-seam.md) | RadixAttention residency as a routing input. |
| Backup seam | [0010](docs/adr/0010-backup-seam.md) | Signed, content-addressed snapshots. |
| Metrics surface | [0011](docs/adr/0011-metrics-surface.md) | Prometheus text exposition. |
| Gateway admission control | [0012](docs/adr/0012-gateway-admission-control.md) | Queueing, not batching. |
| Graceful drain and systemd | [0013](docs/adr/0013-graceful-drain-and-systemd.md) | Draining a node without dropping in-flight work. |
| Read-only MCP server | [0014](docs/adr/0014-read-only-mcp-server.md) | The observability surface over MCP. |

Two of these deserve a note, because they are the ones a newcomer gets
wrong:

**Admission control is queueing, not batching.** The gateway holds a request
until capacity is available rather than grouping requests into a batch. The
distinction matters for latency: batching adds a scheduling delay to every
request in the batch, queueing adds it only to the ones that wait.

**The KV-routing seam keeps the policy compute-plane.** The prefix tree and
the prefetch policy belong to the engine, because only the engine knows what
is resident. goSpark holds the *routing decision*; it asks the engine and
acts on the answer. Putting the policy in goSpark would mean two
implementations of RadixAttention residency, and they would disagree.

---

## 8. The contract snapshots

`spec/sparknet/` holds **read-only verbatim copies** of the fabric's
`contracts/` files: `capabilities.json`, `ports.json`,
`identity-schema.json`, `overlay.json`.

The rule, stated in [spec/sparknet/PROVENANCE.md](spec/sparknet/PROVENANCE.md):

> If a snapshot drifts from the fabric's contract, **the fabric is the
> authority and the snapshot is wrong.**

The snapshots exist so goSpark builds and tests standalone, without a
checkout of the fabric alongside it. What goSpark takes from each:

| Snapshot | Consumed by |
|---|---|
| `capabilities.json` | `internal/authz` — the closed vocabulary and the holder-bears-risk set; the Master's private-key-implied rights are mirrored as `authz.MasterImpliedCapabilities`. |
| `identity-schema.json` | `internal/identity` and `internal/authz` — the projected identity, the `sha256` fingerprint over canonical JSON, and the per-entry signing fields. |
| `ports.json` | The default listen addresses in `configs/*.example.yaml` — chosen so the control-plane gateway and the data plane do not collide with the reserved peer ports. |
| `overlay.json` | The WireGuard-overlay assumption behind `internal/mesh`. |

**When a snapshot moves, the Go code that encodes it moves in the same
change.** `internal/authz/capabilities.go` mirrors `capabilities.json`;
`internal/identity` mirrors `identity-schema.json`. Never silently.

---

## 9. Build, run, test

```sh
make build        # pure-Go binaries into bin/ (CGO_ENABLED=0)
make build-gpu    # the cgo GPU sidecar (links NVML/DCGM)
make test         # all tests
make test-race    # tests under the race detector
make test-nats    # NATS/JetStream integration tests (start a server first)
make nats-dev     # a throwaway JetStream on 127.0.0.1:4222
make vet          # go vet
make fmt          # gofmt
make fmt-check    # gofmt -l, the failing form
make golangci-lint
make lint         # build + vet + fmt-check + golangci-lint
make ci           # fmt-check + vet + golangci-lint + test-race   ← the gate
make eval         # lm-eval against the stub tier
make keys-check   # refuse if a key-shaped file is tracked
make clean
```

Requires **Go 1.27+**.

Two skips are **visible on purpose**, and you should not paper over them:
`make test-nats` skips itself when `nats-server` is absent, and the
`golangci-lint` step reports a SKIP when the binary is not installed. A skip
is information; hiding it would make the gate lie.

### 9.1 A two-process demo

The fastest way to see the whole thing work:

```sh
make build
# mint the control-plane key and a node key
./bin/sparknetctl keygen --out control.key
./bin/sparknetctl keygen --out node-a.key
# start the agent (accepts tasks signed by the control-plane key)
./bin/sparknet-agent --config configs/agent.example.yaml
# the control plane dispatches one arithmetic task to node-a and keeps serving
./bin/sparknet-control --config configs/control.example.yaml
```

### 9.2 Authorising nodes

```sh
# sign a registry authorising node-a for compute; keep control.key 0600
./bin/sparknetctl registry sign --node node-a --cap compute --out registry.json
# revoke (and reinstate) a node: only the Master can, since the flag is signed
./bin/sparknetctl registry revoke --node node-a --out registry.json
# the control plane signs and serves the registry; the agent adopts and verifies it
./bin/sparknet-control --config configs/control.example.yaml
./bin/sparknet-agent --config configs/agent.example.yaml
```

### 9.3 The live admin surface

The admin surface grants full registry authority, so it is **loopback-only**
and **disabled unless `admin.listen` is set**. Changing the registry of a
running control plane means reaching it over an SSH tunnel:

```sh
ssh -L 8899:127.0.0.1:8899 node-a
./bin/sparknetctl admin set-registry --file registry.json
```

### 9.4 GPU telemetry

```sh
# print one snapshot (works even with no GPU: reports available=false + reason)
./bin/sparknet-gpu --once
# publish to the control plane every 30s over the same bus as everything else
./bin/sparknet-gpu --interval 30s
# read the last snapshot per node back from the control plane
./bin/sparknetctl telemetry nodes
```

The `available=false + reason` path is a **first-class** result, not an error
case: the telemetry sidecar is expected to run on hosts without a GPU, and a
host with no GPU must be able to say so honestly.

### 9.5 Telemetry-aware dispatch

```sh
# dispatch to the best node with at least 32 GiB free, then keep serving
./bin/sparknetctl dispatch --prompt "…" --min-free-gib 32
# ask a RUNNING control plane which node it would pick, without dispatching
./bin/sparknetctl plan --prompt "…" --min-free-gib 32
```

`plan` is the read-only one: it returns the reason and the full ranking, not
just the winner. It is the debugging tool for a placement decision.

### 9.6 The remaining seams

Each of these is exercised in the README with a worked example: the
**compute-plane bridge** (the agent delegates to the worker on the GPU host),
the **serving seam**, the **knowledge ingest seam** (crawl and chunk, with or
without an embedding endpoint), the **KV-routing seam** (the control plane
reads a node's KV-residency map), and the **backup seam**:

```sh
./bin/sparknet-backup snapshot --dir /srv/sparknet --out backup.tar
./bin/sparknet-backup verify --file backup.tar
./bin/sparknet-backup restore --file backup.tar --to /srv/sparknet
```

`restore` verifies first and **refuses a tampered backup**.

### 9.7 The eval gate

```sh
make eval         # lm-eval (a pinned clone) against the stub tier
```

`EVAL_BASE_URL` points the same eval at a real model. The corpus is in
`eval/spark_smoke.jsonl` with its config in `eval/spark_smoke.yaml`.

---

## 10. The gate

```sh
make ci           # fmt-check + vet + golangci-lint + test-race
```

**`make ci` is the gate, and the local gate is authoritative.** The GitHub
Actions in `.github/workflows/` are SHA-pinned and only *mirror* it. A node
has no forge and no hosted runner, so a green badge is never what authorises
a merge.

The gate steps and what each enforces:

| Step | What it enforces |
|---|---|
| `fmt-check` | `gofmt -l` is empty. |
| `vet` | `go vet` is clean. |
| `golangci-lint` | The configured linter set. A SKIP is reported, not hidden. |
| `test-race` | The suite under the race detector. |

Plus `make keys-check`, which refuses if a key-shaped file is tracked.

The conventions the gate does not check for you:

- **Conventional commits**, one logical change each.
- **A non-obvious design decision gets an ADR** in `docs/adr/`, written as a
  decision with its consequences. `docs/ROADMAP.md` tracks what is next.
- **Secrets**: `.state/` is gitignored, keys are `0600` there and never
  tracked. Never write a real-ish placeholder. See [SECURITY.md](SECURITY.md).
- **Resource-first**: before building a container, model or service of our own,
  check NVIDIA's catalog first — in the fabric, `make nv-search q="…"` — and
  record the verdict.
- **No comments unless they earn their place.** The code documents *why* in
  prose where a reader would otherwise have to guess, and is otherwise quiet.
- **One page per file** in the docs; cite sources.

---

## 11. How to make a change correctly

### 11.1 Adding a package

1. Put it under `internal/`, and keep it importable only from `cmd/` and from
   other `internal/` packages.
2. Keep `internal/testutil` out of production code.
3. If it needs a model, an embedding endpoint or a CUDA call, it is on the
   wrong side of the boundary — write the interface in Go and implement it in
   the compute plane.
4. If it encodes a contract value, it moves with the snapshot, in the same
   change.

### 11.2 Changing a seam

- Update the ADR in the same commit, if the decision changed. If only the
  implementation changed, the ADR should still be read to check whether the
  decision moved.
- Keep the seam an **interface**, not an import.
- Add the case to the demo in the README, if it is a new user-visible path.

### 11.3 Changing the contract snapshot

1. Re-copy the file from the fabric's `contracts/`.
2. Update the source commit in
   [spec/sparknet/PROVENANCE.md](spec/sparknet/PROVENANCE.md).
3. Update the Go code that mirrors it **in the same change** —
   `internal/authz/capabilities.go` and `internal/identity` are the usual
   pair.
4. Review the diff: the fabric is the authority, so a snapshot that moved
   without the fabric moving is a bug in the snapshot.

### 11.4 Adding a config key

- Add it to the relevant `configs/*.example.yaml`, with an explicit default.
- Keep the environment override consistent with the existing ones.
- Remember `ports.json`'s role: the default listen addresses must not collide
  with the reserved peer ports.

### 11.5 Changing a binary

- `sparknet-control` and `sparknet-agent` stay **pure Go** so they build
  anywhere. Anything needing cgo belongs in `sparknet-gpu`.
- The admin surface stays **loopback-only**, and stays off unless
  `admin.listen` is set.
- Keep the `--help` surface complete.

### 11.6 Writing documentation

- One page per file, in its category directory. Frontmatter `title` +
  `updated`.
- Cite sources. A distilled page without provenance is a guess — mark it as
  one.
- A decision without an ADR is a decision nobody will be able to reconstruct.

---

## 12. Configuration reference

| Thing | Where | Notes |
|---|---|---|
| Runtime config | `configs/*.example.yaml` | YAML with `GOSPARK_*` environment overrides. Copy and edit; never commit the copy. |
| Control plane | `configs/control.example.yaml` | The admin surface, the bus, the gateway. |
| Node agent | `configs/agent.example.yaml` | The compute-plane bridge, the bus. |
| GPU sidecar | `configs/gpu-sidecar.example.yaml` | The telemetry collector and its publish interval. |
| Backup daemon | `configs/backup.example.yaml` | The snapshot interval and destination. |
| Contract snapshots | `spec/sparknet/` | Read-only. The fabric is the authority. |
| Registry file | a JSON file on disk | Its shape lives in `internal/registryfile`. |
| Keys | `.state/` | Gitignored, `0600`, never tracked. `make keys-check` enforces the tracked-file side. |
| On-wire contract | `proto/sparknet/v1/` | One `.proto` per family. |
| Eval corpus | `eval/spark_smoke.jsonl` | With `eval/spark_smoke.yaml`. |
| Units | `deploy/*.service` | The systemd units. |

**Secrets** live in `.state/` and are referenced by path only. `make
keys-check` refuses a tracked key-shaped file. Never write a real-ish
placeholder.

---

## 13. Troubleshooting

| Symptom | Likely cause | Where to look |
|---|---|---|
| The admin surface will not bind | It refuses any non-loopback bind, and it is disabled unless `admin.listen` is set. | `configs/control.example.yaml`; an SSH tunnel. |
| The Go verifier and the fabric disagree about a signature | The canonical bytes or the per-entry fields drifted. | `internal/apiv1`; the fabric's cross-language test. |
| A snapshot is out of date | The fabric's contract moved and the snapshot did not. | `spec/sparknet/PROVENANCE.md` — the fabric is the authority. |
| `make ci` reports a golangci-lint SKIP | The binary is not installed. The skip is visible on purpose. | Install it; do not silence the skip. |
| `make test-nats` skips | `nats-server` is absent. | `make nats-dev` for a throwaway JetStream. |
| `make build-gpu` fails | The cgo sidecar needs the NVML and DCGM headers, which need a CUDA-capable host. | `make build` still produces every other binary — the sidecar is separate by design. |
| The GPU snapshot says `available=false` | There is no GPU, or NVML is not present. | The snapshot's `reason` field says which. This is a normal result, not a failure. |
| Dispatch picks the wrong node | The placement ranking is against the last snapshot, not the live state. | `sparknetctl plan` returns the reason and the full ranking. |
| A request hangs at the gateway | Admission control is **queueing**, not batching — the request waits for capacity. | [ADR 0012](docs/adr/0012-gateway-admission-control.md). |
| The KV transfer stalls on a dropped packet | HTTP/3 over QUIC is used precisely because a TCP stall would block every later block. | `docs/transport.md`; `internal/transport`. |
| The restore refuses | The backup is not authentic or not intact. That is the check working. | `sparknet-backup verify`. |
| The MCP server exposes nothing | The admin routes are the read-only surface, and the control plane is not running or not reachable. | `internal/mcp`; the control plane's `admin.listen`. |
| `make keys-check` fails | A key-shaped file is tracked. | Delete it; rotate anything real. |

---

## 14. Spec vs implemented

Broadly, in this repository:

- **Implemented and tested** — the control/data-plane split, the signed
  envelope verification and its byte-for-byte agreement with the fabric
  (including the attested `department` field and the closed capability
  vocabulary), the per-entry registry signing and revocation, the NVFP4
  codec, the QUIC transport, the live admin surface, the GPU telemetry
  (through the cgo sidecar), telemetry-aware dispatch, the compute-plane task
  bridge, the serving, ingest, KV-routing and backup seams, the Prometheus
  metrics surface, the read-only MCP server.
- **Deliberately absent** — model inference. That is the compute plane, and
  it is reached over the bridge.
- **Scaffold** — `internal/mesh` is explicitly a small scaffold: it reports
  peer state and reroutes endpoints, and does not manage the overlay.

The honest test: **if you cannot point at a command that exercises it, it is
specified.** The README's "Status" section and the ADRs are where that line
is kept. `spec/sparknet/` is a snapshot, not the authority: when the fabric's
contract moves, the snapshot and the mirrored Go move in the same change.

---

## 15. Glossary

| Term | Meaning |
|---|---|
| **Control plane** | The Go side: identity, verification, dispatch, routing, gateway, telemetry, transport. |
| **Compute plane** | The Python/CUDA side: model execution, tokenisation, embeddings. |
| **Seam** | A contract between the two planes, defined in Go and implemented by the compute worker. |
| **Bus** | The NATS/JetStream message fabric the control plane and the agents share. |
| **Data plane** | The KV-block transport over QUIC, kept separate from the bus. |
| **Envelope** | A signed bundle of identity, capabilities and validity timestamps. |
| **Canonical JSON** | Sorted keys, no insignificant whitespace — the bytes Python and Go agree on. |
| **Per-entry signing** | The registry document is unsigned; each entry is signed, so a revoked flag lives inside a signature. |
| **Admin surface** | The control plane's live registry-control plane. Loopback-only, disabled by default. |
| **Placement** | The pure function from telemetry to a ranked list of dispatch candidates. |
| **Admission control** | Queueing at the gateway. Not batching. |
| **RadixAttention residency** | The engine's map of what is already in the KV cache — a compute-plane fact, and a routing input. |
| **NVFP4** | The 4-bit float format the KV blocks are encoded in. |
| **Snapshot** | A signed, content-addressed archive of a directory tree. |
| **MCP** | The Model Context Protocol; the read-only observability surface, served over stdio. |
| **Spec-only** | Designed and contracted, no code, by deliberate role boundary. |
