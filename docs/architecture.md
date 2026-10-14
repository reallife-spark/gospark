# goSpark architecture

goSpark is two cooperating processes over two planes.

```
        ┌──────────────────────────── control plane (bus: NATS/JetStream) ───────────────────────────┐
        │                                                                                             │
  sparknet-control  ──publish signed TaskEnvelope──▶  sparknet.task.<node>                          │
        ▲                                                                                             │
        │                          ┌── sparknet-agent (per node) ──┐                                 │
        │                          │  verify → execute → attest    │                                 │
        └──publish ResultEnvelope──┤  sparknet.result             │                                 │
                                   └───────────────────────────────┘                                │
        └─────────────────────────────────────────────────────────────────────────────────────────┘

        ┌──────────────────────────── data plane (HTTP/3 / QUIC) ──────────────────────────────────┐
   NVFP4 KV blocks  ──────stream──▶  transport.Server  ──▶  kvtransfer.DeltaCache                  │
        └─────────────────────────────────────────────────────────────────────────────────────────┘

        ┌──────────────────────────── service plane (net/http) ─────────────────────────────────────┐
   clients ──/v1/chat/completions──▶  gateway  ──stream (SSE)──▶  vLLM upstream (compute plane)      │
        └─────────────────────────────────────────────────────────────────────────────────────────┘
```

## The two roles

- **control** (`server.RoleControl`) holds the operational signing key. It
  subscribes to `sparknet.register` and `sparknet.result`, keeps a registry of
  node health, and dispatches tasks. It never executes them.
- **agent** (`server.RoleAgent`) holds a node identity but no authority. It
  subscribes to its own `sparknet.task.<node_id>`, verifies the task against the
  control plane's public key, runs a whitelisted operation, and signs the
  result.

The asymmetry is the whole security model: **a task is authoritative because it
is signed, not because it arrived on the bus; a result proves provenance, not
permission.**

## Verification order (`internal/authz`)

`VerifyTask` fails closed and distinguishes refusals so an operator can tell an
attack from a stale message:

1. content must be inside the signature (`ErrContentNotSigned`)
2. fingerprint must match, if pinned (`ErrFingerprint`)
3. every capability must be in the closed vocabulary (`ErrUnknownCapability`)
4. the Ed25519 signature must verify over the canonical bytes (`ErrBadSignature`)
5. liveness against an injected clock (`ErrExpired` / `ErrNotYetValid`)
6. the required capability must be present (`ErrMissingCapability`)
7. the nonce must be unused — checked *last*, so a malformed envelope never
   consumes it (`ErrReplay`)

## Routing (`internal/router`)

The router returns a *decision*, never a result, so a caller may proceed on the
edge immediately while remote work is scheduled out of band. Targets:

- **edge** — within the local domain and token ceiling.
- **dgx** — exceeds the edge ceiling, or demands cross-domain synthesis.
- **cluster** — exceeds the cluster token floor; requires `cluster-dispatch`,
  else it degrades to a single DGX teacher rather than claiming a cluster it
  cannot fan out.

`EstimateTokens` is a deterministic proxy, deliberately not the model's
tokenizer: the control plane must not depend on the compute plane's internals.

## The operation whitelist is the sandbox (`internal/server/ops.go`)

Agents execute `arith` (a restricted expression evaluator: `+ - * / // % **`,
parens, literals — no names, calls or attributes) and `digest` (sha256). Nothing
else. There is no operation that runs a script, because that would turn every
agent into a general remote-code endpoint.

## Configuration

One YAML file plus `GOSPARK_*` env overrides (`internal/config`). Defaults are
explicit; `Validate` rejects a cluster token floor below the edge ceiling, a
non-positive chunk size and an unknown log level.

## What is intentionally absent

No model, no tensors, no tokenizer, no CUDA. The compute plane is invoked
through the gateway (`/v1/chat/completions`) and never linked in. `TODO(compute
plane)` marks the seam.
