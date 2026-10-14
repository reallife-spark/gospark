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

## The bus (`internal/bus`)

The bus carries the signed bytes and nothing more. Two implementations satisfy
one `Bus` interface:

- **Memory** — in-process fan-out, for tests and single-process runs.
- **NATS/JetStream** — the production choice. Plain subjects carry low-latency
  `register`/`result` traffic; a JetStream stream over the same subject root
  gives **store-and-forward**, so a task published while an agent is off the
  WireGuard mesh is delivered when it reconnects (at-least-once).

Consumer naming is what decides fan-out versus load-balance. By default each
connection gets its own durable consumer (named from the connection), so the
control plane and a monitoring subscriber both see every result. Setting a
shared `bus.queue` makes connections compete instead — the right choice for HA
control-plane replicas, the wrong one for an independent observer. `Subscribe`
also injects a reply function into the handler context so `Request`/`Reply`
behaves identically on both implementations.

A failing handler is Nak'd, so JetStream redelivers. Unbounded redelivery would
let a poison message (bad signature, reused nonce) spin a node forever, so every
durable consumer sets a delivery cap (`bus.max_deliver`, default 5); past it the
message is dead-lettered.

## The signed node registry (`internal/authz/registry.go`)

Authority in this fabric comes from key possession, not a grant: the Master
signs, a peer verifies. The **node registry** is that statement of authority —
which nodes may hold which capabilities.

- It is a whole-document signature, not a per-entry one: `SignRegistry` fills
  `MasterPublicKeyB64`, normalises the entries and signs the canonical bytes
  with `SignatureB64` blanked. A caller cannot half-sign a registry.
- The trust anchor is the **pinned master public key** a node is configured
  with (`-master-pub`), not the `master_public_key_b64` embedded in the file —
  that field is informational, so a forged registry cannot nominate its own
  signer.
- `Revoked` sits *inside* the signature, so only the Master can flip it.
- `IssuedAtUnix` is monotonic: an agent refuses a registry older than the one it
  already holds (`ErrRegistryRollback`), which stops a replay of a pre-revocation
  document.
- An empty registry authorizes nobody; a control plane with no registry at all
  runs in explicit *open mode* (a logged warning), and an agent with no adopted
  registry is permissive — the legacy single-process posture.

The registry is enforced twice, client-side. The control plane refuses to
dispatch to a node the registry does not authorize (`authorizeNode`), and the
agent independently refuses a task when its adopted registry does not authorize
*itself* (`registryAllowsSelf`) — defense in depth, so a task injected straight
onto the bus is still rejected.

`sparknetctl registry sign|verify|show` mints and inspects registries; a control
plane loads authorizations from `-registry` (and re-signs them, stamping a fresh
issue time), while an agent loads the signed file and verifies it against its
pinned master key.

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
