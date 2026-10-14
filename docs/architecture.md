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
  subscribes to `sparknet.register`, `sparknet.result` and `sparknet.health`,
  keeps a registry of node health, and dispatches tasks. It never executes them.
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

- It is a **per-entry** signature, not a whole-document one, matching the
  fabric byte-for-byte: `SignRegistry` signs each entry over the fabric's
  `signed_envelope_fields` — `fingerprint`, `identity`, sorted `capabilities`,
  `overlay_ipv4`, `issued_at`, `expires_at`, `revoked` — with that entry's
  `signature_b64` blanked. The document itself is unsigned, as the fabric ships
  it.
- The fingerprint is `sha256` over the canonical JSON of the projected identity
  (`node_id`, `role`, `wireguard_public_key_b64`, `overlay_ipv4`,
  `requested_capabilities`); goSpark carries the node's Ed25519 key in the
  `wireguard_public_key_b64` slot, so the entry binds the node to the key its
  results are verified against.
- The trust anchor is the **pinned master public key** a node is configured
  with (`-master-pub`), not the `master_public_key_b64` embedded in the file —
  that field is informational, so a forged registry cannot nominate its own
  signer.
- `Revoked` sits *inside* the signature, so only the Master can flip it.
- `issued_at`/`expires_at` are ISO-8601 and eval order is fabric eval order:
  unknown node → revoked → expired → not-yet-valid (5 min clock skew) →
  identity/fingerprint mismatch → bad signature → unknown capability. Rollback
  is refused by comparing `LatestIssued()` (the newest entry stamp): an agent
  refuses a registry older than the one it already holds
  (`ErrRegistryRollback`), which stops a replay of a pre-revocation document.
- An empty registry authorizes nobody; a control plane with no registry at all
  runs in explicit *open mode* (a logged warning), and an agent with no adopted
  registry is permissive — the legacy single-process posture.

The registry is enforced twice, client-side. The control plane refuses to
dispatch to a node the registry does not authorize (`authorizeNode`), and the
agent independently refuses a task when its adopted registry does not authorize
*itself* (`registryAllowsSelf`) — defense in depth, so a task injected straight
onto the bus is still rejected.

`sparknetctl registry sign|verify|show` mints and inspects registries, and
`registry revoke` flips one node's `revoked` flag and re-signs (a fresh stamp
advances the rollback guard; `-unrevoke` reinstates). Only the Master can do
either, because the flag sits inside the signature. A control plane loads
authorizations from `-registry` (and re-signs each entry, clearing its
`issued_at` to force a fresh stamp), while an agent loads the signed file and
verifies every entry against its pinned master key.

Changing a *running* control plane's registry goes through the **admin
surface** (`internal/server/admin.go`), an HTTP endpoint disabled unless
`admin.listen` is set. `GET /admin/registry` snapshots the served registry,
`POST /admin/registry/authorize` grants or updates one node (the
sign-authorization right, `sparknetctl admin authorize`),
`POST /admin/registry/revoke` flips one node's flag, and `PUT /admin/registry`
replaces the whole authorization set. Every route funnels through
`mutateRegistry`, the single write path that re-signs with a fresh stamp,
re-derives the accepted executor keys and re-publishes — so an admin change is
indistinguishable on the wire from a config-driven distribute. Because the
process holds the Master key, the surface has full registry authority and
refuses to bind any non-loopback address (`requireLoopback`); remote
administration would need a signed-request path and is deliberately out of
scope. A separate read-only route, `GET /admin/gpu`, serves the last GPU
telemetry per node (see below), and `GET /admin/placement` ranks dispatch
candidates and reports the choice (see placement, below); neither has a write
counterpart.

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

## GPU telemetry (`internal/gputel`)

GPU state is **telemetry, never authority**, and it is deliberately outside the
core binaries. `internal/gputel` is pure Go: it defines `Collector`, the
`GPU`/`Health`/`Snapshot` types, a `Null` collector, a merge-on-read `Multi`, a
self-registering backend table, and the one wire projection (`Snapshot.Wire`).
It links no GPU library. The cgo backends (`gputel/nvml`, `gputel/dcgm`) sit
behind build tags with non-cgo stubs, and only `cmd/sparknet-gpu` imports them —
so `make build` stays `CGO_ENABLED=0` (ADR 0001) and the sidecar is the single
cgo binary (ADR 0004).

The sidecar collects a snapshot and, in `-watch` mode, publishes a `NodeHealth`
carrying a `GpuTelemetry` on the previously-unused `sparknet.health` subject, as
a `RegisterNode`. The control plane's `onGPUHealth` records it in a map keyed by
node id — **separate from the agent `Registry`**, so a telemetry-only report can
never clobber an agent's full health record — and does *not* verify it as a
signed envelope, because a telemetry report carries no capability and grants
nothing. It is read back through the read-only `GET /admin/gpu` (and
`sparknetctl admin gpu`); there is no write counterpart.

A missing driver or a `CGO_ENABLED=0` build is a normal, reported condition: the
sidecar builds on a driverless host and answers `available=false` with a reason
(`nvml: … ERROR_LIBRARY_NOT_FOUND`, `dcgm: … libdcgm.so.4 not found`) rather
than crashing.

## Placement (`internal/placement`)

Dispatch chooses a node by its last-known capacity. `Select(candidates,
request) Decision` is **pure**: plain `Candidate` values (node id, serving,
load, last telemetry) in, a `Decision` out — the chosen node, a reason, and the
best-first ranking with a per-node reason. It has no bus, registry or GPU, so
the policy is unit-tested directly. `internal/server` builds the candidate set
from the registry — in fail-closed mode only nodes authorized for the
capability (`App.candidates` / `authorizeNode`), in open mode every registered
node — and joins it with the health registry's `Serving`/`LoadPermille` and the
telemetry map (preferring the GPU sidecar's reading over the agent's embedded
one). Authorization is applied *before* placement, so the engine only picks
*among* authorized nodes; telemetry can never add one (ADR 0005).

Eligibility and ranking are separate. A zero `Requirement` makes every serving
node eligible and the ordering advisory; a non-zero requirement (a free-memory
floor, a temperature ceiling, or require-healthy) demands an **available, fresh**
reading and reports why a node was excluded (`accelerator unavailable`,
`no telemetry for requirement`, `telemetry stale`,
`insufficient free accelerator memory`, `temperature above ceiling`,
`accelerator not healthy`). `available` (the sidecar saw a GPU) and `fresh`
(recent, from an available accelerator, within `MaxTelemetryAge`) are distinct
axes, so a driverless node is not mislabelled "stale". Ranking prefers fresh over
stale/none, then more free memory, lower temperature, lower utilization, lower
load, and finally node id — deterministic. `App.Place` and `App.PlaceAndDispatch`
expose it; `GET /admin/placement` reports it read-only, and
`sparknet-control -dispatch auto` polls it (after `App.Ready()`) until a node is
eligible, then dispatches.

## What is intentionally absent

No model, no tensors, no tokenizer, no CUDA. The compute plane is invoked
through the gateway (`/v1/chat/completions`) and never linked in. `TODO(compute
plane)` marks the seam.
