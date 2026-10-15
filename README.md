# goSpark

**A Go control/service plane for SparkNet.** goSpark implements the fabric's
*control* and *service* plane — identity, capability verification, task
dispatch, routing, the API gateway, and the NVFP4 KV-block data plane — in Go.
It is a sibling to the `sparknet` fabric, not a replacement for it: the peer
guard, the anchor and the Python/CUDA compute plane stay where they are.

The split follows one rule from the SparkNet design: **Go runs the control and
service plane; Python/CUDA runs the compute plane.** Go is where latency,
concurrency and network plumbing matter; the GPU runtime is where the model
lives. Keeping them in separate processes is what lets a node keep answering
while a GPU job is in flight.

## What is here

| Path | What it is |
|---|---|
| `cmd/sparknet-control` | the master role: holds the signing key, dispatches signed tasks, verifies attested results |
| `cmd/sparknet-agent` | a node: reports health, verifies signed tasks, executes a closed operation whitelist, returns attested results |
| `cmd/sparknetctl` | operator CLI: keygen, fingerprint, capability list, route, registry sign/verify/revoke, live admin show/revoke/authorize/gpu/health/place, block decode/send |
| `cmd/sparknet-gpu` | GPU-telemetry sidecar: the one cgo binary, links NVML (go-nvlib) + DCGM (go-dcgm), publishes a node's GPU snapshot on the health subject |
| `internal/authz` | fail-closed verification of signed envelopes + capability vocabulary + the signed node registry |
| `internal/identity` | Ed25519 node identity + canonical fingerprint |
| `internal/gputel` | pure-Go GPU telemetry: `Collector`/`Snapshot` types, registry, `Null`, `Multi`, and the wire projection (no GPU link) |
| `internal/placement` | pure telemetry-aware dispatch: rank authorized nodes by free VRAM / temperature / health and choose one (no bus, no registry, no GPU) |
| `internal/compute` | the compute-plane seam: delegate explicitly-named operations to a Python/CUDA worker over HTTP (no model, no CUDA) |
| `compute/` | the reference Python/CUDA worker (`sparknet_bridge.py`, generalised): FastAPI `/v1/execute`, systemd unit, example env |
| `internal/kvtransfer` | the reference NVFP4 (E2M1) codec and delta cache |
| `internal/transport` | the HTTP/3 (QUIC) data plane for KV blocks |
| `internal/router` | edge / DGX / cluster escalation decisions |
| `internal/gateway` | Go API gateway in front of a local vLLM upstream: SSE-aware proxy, `/readyz` readiness probe, `/v1/models` discovery |
| `internal/ingest` | concurrent vault/git crawl + chunking for local RAG |
| `internal/embed` | OpenAI-compatible embeddings client, the ingest seam to the compute plane (no model, no CUDA) |
| `internal/kvrouter` | client for a compute-plane RadixAttention engine: asks which node holds a prompt's cached KV prefix (no model, no CUDA) |
| `internal/backup` | signed, content-addressed snapshot / verify / restore of a directory tree (pure Go I/O, no rsync) |
| `internal/bus` | NATS/JetStream-backed bus with an in-memory test double |
| `internal/config` | YAML config + `GOSPARK_*` env overrides |
| `internal/mesh` | WireGuard overlay inspection/re-route |
| `proto/sparknet/v1` | the on-wire contract, one `.proto` per message family |
| `spec/sparknet` | read-only snapshots of the fabric's contract SSOT (see its `PROVENANCE.md`) |

## Authority comes from key possession

goSpark keeps the fabric's asymmetry: **the signer signs, the verifier
verifies, and holding a capability name without the key grants nothing.** A
`TaskEnvelope` is signed by the control plane's operational key. An agent
verifies the signature, liveness, the required capability and the replay nonce
— *in that order* — and refuses on the first failure. The agent's result is
signed in turn, so the control plane learns who computed it, never that the
computation was *permitted*.

## Build and test

```sh
make build          # bin/sparknet-control, bin/sparknet-agent, bin/sparknetctl (CGO_ENABLED=0)
make build-gpu      # bin/sparknet-gpu — the cgo GPU sidecar (links NVML/DCGM)
make test           # all tests
make ci             # fmt-check + vet + race tests
```

Requires Go 1.27+. The core binaries build with `CGO_ENABLED=0` (ADR 0001); the
GPU sidecar is the only cgo binary (ADR 0004).

## Run a two-process demo (in-memory bus)

```sh
make build

# Mint the control-plane key and a node key.
bin/sparknetctl keygen -node control -role master -out .state/control.key
bin/sparknetctl keygen -node node-a  -role peer   -out .state/node.key

MASTER_PUB=$(bin/sparknetctl fingerprint -key .state/control.key >/dev/null; \
  python3 -c "import json;print(json.load(open('.state/control.key'))['public_key_b64'])")
NODE_PUB=$(python3 -c "import json;print(json.load(open('.state/node.key'))['public_key_b64'])")
NODE_FP=$(python3 -c "import json;print(json.load(open('.state/node.key'))['fingerprint'])")

# Agent (accepts tasks signed by the control-plane key).
bin/sparknet-agent -config configs/agent.example.yaml \
  -key .state/node.key -master-pub "$MASTER_PUB" &

# Control plane dispatches one arithmetic task to node-a and keeps serving.
bin/sparknet-control -config configs/control.example.yaml \
  -master-key .state/control.key \
  -executors "node-a=$NODE_PUB" \
  -dispatch node-a -op arith -params '{"expression":"6*7"}'
```

The in-memory bus only works within one process. For two processes, point both
at a NATS server with `GOSPARK_BUS_URL=nats://host:4222` (or the `bus.url` key).

```sh
make nats-dev                       # JetStream server on 127.0.0.1:4222
export GOSPARK_BUS_URL=nats://127.0.0.1:4222
# then run sparknet-control and sparknet-agent as above, in separate shells
```

With JetStream the bus is store-and-forward: a task published while an agent is
off the mesh is delivered when it reconnects (at-least-once). Each instance gets
its own durable consumer, so the control plane and a monitoring subscriber both
see every result; set `bus.queue` to a shared value only to make HA replicas
load-balance instead. `make test-nats` runs the integration tests against a
throwaway server (they skip if `nats-server` is not installed).

## Authorize nodes with a signed registry

A registry is the Master's signed statement of which nodes may hold which
capabilities. Without one, the control plane runs in open mode and an agent is
permissive; with one, both refuse a node the registry does not authorize.

```sh
# Sign a registry authorizing node-a for compute; keep control.key 0600.
bin/sparknetctl registry sign -master-key .state/control.key \
  -node-key .state/node.key -caps cluster-serve,telemetry -out .state/registry.json
bin/sparknetctl registry verify -registry .state/registry.json -master-key .state/control.key
bin/sparknetctl registry show   -registry .state/registry.json

# Revoke (and reinstate) a node: only the Master can, since the flag is signed.
bin/sparknetctl registry revoke -master-key .state/control.key \
  -registry .state/registry.json -node node-a -out .state/registry.json
bin/sparknetctl registry revoke -master-key .state/control.key \
  -registry .state/registry.json -node node-a -unrevoke -out .state/registry.json

# Control signs and serves the registry; the agent adopts and verifies it.
bin/sparknet-control -config configs/control.example.yaml \
  -master-key .state/control.key -registry .state/registry.json -dispatch node-a
bin/sparknet-agent -config configs/agent.example.yaml \
  -key .state/node.key -master-pub "$MASTER_PUB" -registry .state/registry.json

# Change the registry of a RUNNING control plane over its admin surface. The
# surface grants full registry authority, so it binds loopback-only and is
# disabled unless admin.listen is set. See configs/control.example.yaml.
bin/sparknetctl admin show      -addr http://127.0.0.1:8899
bin/sparknetctl admin authorize -node node-b -pubkey "$NODE_B_PUB" -caps cluster-serve
bin/sparknetctl admin revoke    -node node-a         # or -unrevoke to reinstate
bin/sparknetctl admin gpu       -addr http://127.0.0.1:8899   # read-only GPU telemetry
bin/sparknetctl admin health    -addr http://127.0.0.1:8899   # health + authz + GPU per node
bin/sparknetctl admin place     -addr http://127.0.0.1:8899   # which node dispatch would pick
bin/sparknetctl admin kvcache   -addr http://127.0.0.1:8899   # KV-residency routing seam status
```

Each entry is signed **per entry** over the fabric's `signed_envelope_fields`
(`fingerprint`, `identity`, sorted `capabilities`, `overlay_ipv4`, `issued_at`,
`expires_at`, `revoked`), so the document itself is unsigned — exactly as the
fabric ships it. The trust anchor is the agent's pinned `-master-pub`, not the
key embedded in the file. `Revoked` (and the timestamps) live inside each
entry's signature, so only the Master can revoke, and an agent refuses a
replayed older registry. See [`docs/architecture.md`](docs/architecture.md) for
the mechanism.

## GPU telemetry (read-only)

GPU state is **telemetry, never authority**. A per-host sidecar — the one cgo
binary — reads NVML (metrics) and DCGM (health) and publishes a `NodeHealth`
carrying a `GpuTelemetry` on the `sparknet.health` subject; the control plane
records it and serves it read-only. A health report carries no capabilities and
grants nothing.

```sh
# Print one snapshot (works even with no GPU: reports available=false + reason).
bin/sparknet-gpu -once -node-id node-a -text

# Publish to the control plane every 30s over the same bus as everything else.
bin/sparknet-gpu -watch -node-id node-a -interval 30s -config configs/gpu-sidecar.example.yaml

# Read the last snapshot per node back from the control plane.
bin/sparknetctl admin gpu -addr http://127.0.0.1:8899
```

The core binaries are pure Go and compile with `CGO_ENABLED=0`; only
`cmd/sparknet-gpu` links a GPU library, and it *builds* on a driverless host and
*degrades at runtime* to `available=false` with a reason. The NVIDIA Go
libraries are consumed by path (`require` + local `replace`); see
[`docs/adr/0004-gpu-telemetry-sidecar.md`](docs/adr/0004-gpu-telemetry-sidecar.md).

## Telemetry-aware dispatch

Dispatch chooses a node by its **last-known capacity**, not by name. The
placement engine ranks the nodes the registry authorizes for the capability —
preferring a fresh GPU reading, then more free accelerator memory, lower
temperature, lower utilization and lower load — and the control plane dispatches
to the winner. Capture happens in `internal/placement`, a pure function with no
bus, registry or GPU, so the policy is unit-tested directly.

```sh
# Dispatch to the best node with at least 32 GiB free, then keep serving.
bin/sparknet-control -config configs/control.example.yaml \
  -master-key .state/control.key -registry .state/registry.json \
  -dispatch auto -dispatch-timeout 30s -op arith -params '{"expression":"6*7"}'

# Ask a RUNNING control plane which node it would pick, without dispatching
# (read-only; the reason and full ranking come back too).
bin/sparknetctl admin place -addr http://127.0.0.1:8899
bin/sparknetctl admin place -addr http://127.0.0.1:8899 -min-free-mem 34359738368 -max-temp 80 -require-healthy
```

A **zero requirement** makes every serving node eligible and the ranking
advisory; a **non-zero requirement** (a memory floor, a temperature ceiling, or
`-require-healthy`) needs an *available, fresh* reading and reports why a node
was excluded (`accelerator unavailable`, `no telemetry for requirement`,
`telemetry stale`, `insufficient free accelerator memory`, …). Telemetry is
still never authority: placement only ever chooses *among* the nodes the
registry authorizes, so a node cannot lie its way into work. See
[`docs/adr/0005-telemetry-aware-dispatch.md`](docs/adr/0005-telemetry-aware-dispatch.md).

## Compute-plane bridge

The Go plane runs no model, tensor or CUDA code. A node that has GPU work to do
names a Python/CUDA worker and the operations that worker may run; the agent
still verifies the Master-signed task and signs the result, but delegates the
*execution* of those operations across the seam.

```yaml
# in an agent's config
compute:
  endpoint: http://127.0.0.1:9090
  secret: <shared with the worker's SPARKNET_SHARED_SECRET>
  operations: [infer, embed]   # must not shadow arith/digest
  timeout: 60s
```

The closed sandbox is still the default: local operations (`arith`, `digest`)
always run in-process and can never be shadowed, only the explicitly listed
operations are delegated, and everything else is still refused. With no
`compute.endpoint`, nothing is delegated. The reference worker is
[`compute/compute_worker.py`](compute/README.md) — the blueprint's
`sparknet_bridge.py`, generalised.

```sh
# On the GPU host: run the worker beside vLLM.
cd compute && python3 -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt && SPARKNET_SHARED_SECRET=… python compute_worker.py

# Elsewhere: the control plane signs an infer task; the agent delegates it.
bin/sparknet-control -master-key .state/control.key -executors node-a=<pub> \
  -dispatch node-a -op infer -params '{"prompt":"hello gpu"}'
```

See [`docs/adr/0006-compute-plane-task-bridge.md`](docs/adr/0006-compute-plane-task-bridge.md).

## Serving seam

The gateway is the interactive door to the same engine: it proxies
`/v1/chat/completions` to vLLM with SSE streaming, tells the truth about whether
the engine is up (`/healthz` liveness vs `/readyz` readiness), and exposes model
discovery (`/v1/models`). A model server and an optional Open WebUI reference are
in [`compute/docker-compose.vllm.yml`](compute/docker-compose.vllm.yml); the
gateway stays a pure, GPU-free proxy. See
[`docs/adr/0007-gateway-serving-seam.md`](docs/adr/0007-gateway-serving-seam.md).

## Knowledge ingest seam

Ingestion is I/O-bound Go work — crawl, chunk, delta-digest — that keeps the
local RAG pipeline fed while the GPU stays free for inference. Turning text into
vectors is model work, so it happens beside the GPU: `internal/embed` is an
OpenAI-compatible client for an embeddings service, and `cmd/sparknet-ingest`
drives crawl → chunk → embed, pairing every chunk with its vector.

```yaml
# in an ingest driver's config
embed:
  endpoint: http://127.0.0.1:9090
  secret: <shared with the worker's SPARKNET_SHARED_SECRET>
  model: sparknet-embed-hash
  batch: 32
  timeout: 60s
```

```sh
# On the GPU host: the reference worker also serves /v1/embeddings and /v1/models.
# Elsewhere: crawl a vault and write a vectorized index.
bin/sparknet-ingest -root /mnt/knowledge -embed-endpoint http://127.0.0.1:9090 \
  -embed-secret … -out index.json
# No endpoint: crawl and chunk only.
bin/sparknet-ingest -root /mnt/knowledge
```

With no `embed.endpoint`, nothing is vectorized and the crawl/chunk result is
still usable — the seam is additive. The reference worker
([`compute/README.md`](compute/README.md)) serves the embeddings routes and falls
back to a deterministic hashed embedding when no engine is configured, so the
pipeline can be exercised without a GPU. See
[`docs/adr/0008-ingest-embedding-seam.md`](docs/adr/0008-ingest-embedding-seam.md).

## KV-routing seam

The blueprint's RadixAttention design keeps a prefix index of what the cluster
has already computed, so a prompt whose prefix is cached is routed to the node
that holds it and the prefill is not paid twice. The prefix tree is
compute-plane (only the engine knows what it cached); *which node to send to* is
a routing decision, so goSpark owns the query. `internal/kvrouter` asks the
engine over `POST /v1/kv/lookup`; a hit becomes an advisory `ResidentNode` hint
that placement ranks first among eligible nodes.

```yaml
# in a control plane's config
kv:
  endpoint: http://127.0.0.1:9100
  secret: <shared with the daemon's SPARKNET_SHARED_SECRET>
  timeout: 2s
```

A miss, a task with no prompt, or an unreachable engine all degrade to ordinary
telemetry placement — the hint is never authority. Inspect it with
`bin/sparknetctl admin kvcache [-prompt "…"]`. The reference RadixAttention
daemon is [`compute/kv_cache_router.py`](compute/README.md); the NVFP4 blocks it
indexes move over [`internal/kvtransfer`](docs/architecture.md) +
`internal/transport`. See
[`docs/adr/0009-kv-routing-seam.md`](docs/adr/0009-kv-routing-seam.md).

## Backup seam

The blueprint's §3.11 `sparknet_backup.py` mirrors a directory with rsync. A copy
that cannot prove it is the copy that was taken is not a backup, so goSpark's
slice is the half that makes one trustworthy: `sparknet-backup` hashes every
file, signs the manifest with the node key the same way every fabric envelope is
signed, and re-hashes the tree on verify.

```bash
# One signed snapshot (incremental by default — unchanged files are hard-linked)
bin/sparknet-backup snapshot -source /mnt/knowledge -target /mnt/data/backups -key /etc/sparknet/node.key
# Prove a snapshot authentic and intact before trusting it
bin/sparknet-backup verify -dir /mnt/data/backups/snap_20261015_030000 -key /etc/sparknet/node.key
# Restore (verifies first; refuses a tampered backup)
bin/sparknet-backup restore -dir /mnt/data/backups/snap_20261015_030000 -dest /tmp/restored -key /etc/sparknet/node.key
# Daemon: snapshot on an interval
bin/sparknet-backup run -config configs/backup.example.yaml
```

No shell, no `--delete`; a snapshot is authentic (signature), intact (re-hash) and
chained (`previous_digest`). See
[`docs/adr/0010-backup-seam.md`](docs/adr/0010-backup-seam.md).

## Contract provenance

`spec/sparknet/` holds read-only snapshots of `sparknet/contracts/`. The fabric
is the authority; the snapshots exist so goSpark builds and tests standalone.
When a snapshot moves, the mirrored Go in `internal/authz` and
`internal/identity` moves in the same change. See
[`spec/sparknet/PROVENANCE.md`](spec/sparknet/PROVENANCE.md).

## Status

First-pass scaffold + control-plane core. The control/data-plane split, the
signed-envelope verification, the NVFP4 codec, the QUIC transport, the live
admin surface, GPU telemetry (via the cgo sidecar), telemetry-aware dispatch and
the compute-plane task bridge are real and tested. Model inference is
deliberately **not** here — that is the compute plane, reached over the bridge.
See
[`docs/adr/0001-go-control-plane.md`](docs/adr/0001-go-control-plane.md),
[`docs/adr/0004-gpu-telemetry-sidecar.md`](docs/adr/0004-gpu-telemetry-sidecar.md),
[`docs/adr/0005-telemetry-aware-dispatch.md`](docs/adr/0005-telemetry-aware-dispatch.md)
and
[`docs/adr/0006-compute-plane-task-bridge.md`](docs/adr/0006-compute-plane-task-bridge.md)
for the decisions and their consequences.
