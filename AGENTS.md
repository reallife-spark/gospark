# AGENTS.md — goSpark

This file is for an agent (or a person) starting work in this repository. It
says what goSpark is, how to run it, and which rules are not negotiable. It is
deliberately short; the detail lives in the files it points to.

## What this is

goSpark is the Go **control and service plane** for SparkNet: identity and
capability verification (`internal/authz`, `internal/identity`), signed task
dispatch and telemetry-aware placement (`internal/placement`), routing
(`internal/router`), the API gateway (`internal/gateway`), the NATS/JetStream
bus (`internal/bus`), the per-vault typed memory and delegate service
(`internal/memory`, `internal/delegates`), and the NVFP4 KV-block data plane
(`internal/kvtransfer`, `internal/transport`).

It is a **sibling to the `sparknet` fabric, not a replacement.** The split
follows one rule, recorded in ADR 0001: **Go runs the control/service plane;
Python/CUDA runs the compute plane.** The peer guard, the anchor and the
compute plane stay where they are.

`USER_MANUAL.md` is the developer-facing manual for this repository: the
boundary, the authority model, the packages, the binaries, the seams, the
contract snapshots, the gate, and how to make a change correctly. The **master
manual** for the whole system lives in the `sparknet` repository as
`USER_MANUAL.md`.

## The boundary (do not cross it)

- The pure-Go core builds with `CGO_ENABLED=0` and must never require a GPU, a
  model, or the compute plane to build or to make an authorization decision.
- The only cgo binary is `cmd/sparknet-gpu` (ADR 0004). It *builds* on a
  driverless host and *degrades at runtime* to `available=false` with a reason.
- Anything that would pull NVML/DCGM/CUDA into the core belongs behind a seam:
  `internal/gputel` (telemetry), `internal/compute`, `internal/embed`,
  `internal/kvrouter`.

## Authority model (fail closed)

The signer signs; the verifier verifies. An agent verifies a task envelope's
**signature → liveness → required capability → replay nonce, in that order**,
and refuses on the first failure. Holding a capability name without the key
grants nothing; the trust anchor is the pinned `-master-pub`, not a key
embedded in a document you were handed. The admin surface grants full registry
authority, so it binds loopback-only and is off unless `admin.listen` is set.
Telemetry is never authority: a health report carries no capabilities.

## Commands

```sh
make build        # pure-Go binaries into bin/ (CGO_ENABLED=0)
make build-gpu    # the cgo GPU sidecar (links NVML/DCGM)
make test         # all tests
make test-race    # tests under the race detector
make test-nats    # NATS/JetStream integration tests (start a server first)
make nats-dev     # throwaway JetStream on 127.0.0.1:4222
make lint         # build + vet + fmt-check + golangci-lint
make ci           # fmt-check + vet + golangci-lint + test-race  ← the gate
make eval         # lm-eval against the stub tier (EVAL_BASE_URL for a real model)
make keys-check   # refuse if a key-shaped file is tracked
make clean
```

Requires Go 1.27+. `make ci` is the gate; the local gate is authoritative and
the GitHub Actions in `.github/workflows/` (SHA-pinned) only mirror it.
`make test-nats` skips itself when `nats-server` is absent — that skip is
visible on purpose, as is the `golangci-lint` SKIP when the binary is not
installed.

## Layout

```
cmd/            binaries: sparknet-control (Master), sparknet-agent (node),
                sparknetctl (operator CLI), sparknet-gpu (cgo sidecar),
                sparknet-ingest, sparknet-backup, sparknet-mcp, sparknet-memory,
                stub-vllm
internal/       apiv1 authz backup bus compute config embed gateway gputel
                identity ingest kvrouter kvtransfer mcp mesh metrics placement
                registryfile router server telemetry testutil transport
proto/          sparknet/v1 — the on-wire contract, one .proto per family
spec/           sparknet — read-only contract snapshots from the fabric
                (see spec/sparknet/PROVENANCE.md)
compute/        the reference Python/CUDA worker and its .example env files
docs/           architecture.md, transport.md, ROADMAP.md, adr/
configs/        *.example.yaml — copy and edit, never commit the copy
```

## Conventions

- **Style**: the fabric's `STYLE_GUIDE.md` (sibling `sparknet` repository) is
  the standing convention page for both repositories — Python, shell,
  Makefiles, commits, secrets, CI, docs, release. Go adds `gofmt` + `go vet`
  (both run by `make ci`).
- **Commits**: conventional commits, one logical change each.
- **Decisions**: a non-obvious design decision gets an ADR in `docs/adr/`,
  written as a decision with its consequences. `docs/ROADMAP.md` tracks what is
  next.
- **Secrets**: `.state/` is gitignored; keys are 0600 there and never tracked.
  `make keys-check` enforces the tracked-file side. Never write a real-ish
  placeholder. See `SECURITY.md`.
- **Resource-first**: before building a container, model or service of our own,
  check NVIDIA's catalog first (in the fabric, `make nv-search q="…"`) and
  record the verdict.
- **No comments unless they earn their place.** The code in this repository
  documents *why* in prose where a reader would otherwise have to guess, and is
  otherwise quiet.

## Where the fabric is

The contract this repository speaks is the sibling `sparknet` fabric:
`contracts/` is the SSOT, `shared/sparknet-authz.py` the reference
implementation, and `spec/sparknet/` here is the read-only snapshot of it.
When the fabric's contract changes, the snapshot is refreshed deliberately and
the diff reviewed — never silently.
