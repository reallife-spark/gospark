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
| `cmd/sparknetctl` | operator CLI: keygen, fingerprint, capability list, route, block decode/send |
| `internal/authz` | fail-closed verification of signed envelopes + capability vocabulary |
| `internal/identity` | Ed25519 node identity + canonical fingerprint |
| `internal/kvtransfer` | the reference NVFP4 (E2M1) codec and delta cache |
| `internal/transport` | the HTTP/3 (QUIC) data plane for KV blocks |
| `internal/router` | edge / DGX / cluster escalation decisions |
| `internal/gateway` | Go API gateway in front of a local vLLM upstream, SSE-aware |
| `internal/ingest` | concurrent vault/git crawl + chunking for local RAG |
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
make build          # bin/sparknet-control, bin/sparknet-agent, bin/sparknetctl
make test           # all tests
make ci             # fmt-check + vet + race tests
```

Requires Go 1.27+.

## Run a two-process demo (in-memory bus)

```sh
make build

# Mint the control-plane key and a node key.
bin/sparknetctl keygen -node control -owner ops -out .state/control.key
bin/sparknetctl keygen -node node-a  -owner ops -out .state/node.key

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

## Contract provenance

`spec/sparknet/` holds read-only snapshots of `sparknet/contracts/`. The fabric
is the authority; the snapshots exist so goSpark builds and tests standalone.
When a snapshot moves, the mirrored Go in `internal/authz` and
`internal/identity` moves in the same change. See
[`spec/sparknet/PROVENANCE.md`](spec/sparknet/PROVENANCE.md).

## Status

First-pass scaffold + control-plane core. The control/data-plane split, the
signed-envelope verification, the NVFP4 codec and the QUIC transport are real
and tested. Model inference is deliberately **not** here — that is the compute
plane. See [`docs/adr/0001-go-control-plane.md`](docs/adr/0001-go-control-plane.md)
for the decisions and their consequences.
