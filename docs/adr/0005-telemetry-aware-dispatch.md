# ADR 0005 — Telemetry-aware dispatch via a pure placement engine

- **Status:** accepted
- **Date:** 2026-10-14
- **Context:** ADR 0004 added GPU telemetry but deliberately left it inert:
  "GPU capacity for the router … feeding telemetry into scheduling is a later
  step." The control plane could see a node's free VRAM, temperature and health
  (`GET /admin/gpu`) but its dispatch still chose a node by node id alone, and a
  caller had no way to say "give me a node with at least N free". The session's
  "departmental GPU delegation" model makes the *choice of node* a first-class
  control-plane concern; the heavy constrained assignment (`x_ij` across a
  department's nodes) stays in the compute plane.

## Decision

Add a **pure placement engine** and make the control plane dispatch through it.

- **`internal/placement` is pure.** `Select(candidates, request) Decision`
  takes plain `Candidate` values (node id, serving flag, load, last telemetry)
  and a `Requirement` (minimum free accelerator memory, maximum temperature,
  require-healthy) plus policy (`Now`, `MaxTelemetryAge`), and returns a
  best-first `Decision`: the chosen node, a human reason, and the full ranking
  with per-node reasons. It has no bus, no registry, no GPU and no globals, so
  the dispatch *policy* is unit-tested directly.
- **Authorization is applied before placement.** The server builds the candidate
  set from the registry — in fail-closed mode, only nodes authorized for the
  capability; in open mode, every registered node — then hands it to `Select`.
  The engine never decides *who may* serve, only *which* of those can best take
  the work.
- **Eligibility vs. ranking are separate.** A zero `Requirement` makes every
  serving node eligible and the ordering advisory; a non-zero requirement needs
  an *available, fresh* reading. `fresh` (recent, from an available accelerator)
  and `available` (the sidecar actually saw a GPU) are distinct axes, so a
  driverless node reports `accelerator unavailable` rather than a misleading
  "stale". Ranking prefers fresh over stale/none, then more free memory, then
  lower temperature, lower utilization, lower load, and finally node id — so the
  result is deterministic.
- **Three ways in, one engine.** The control plane exposes `App.Place` /
  `App.PlaceAndDispatch`; `GET /admin/placement` reports the ranking without
  dispatching (read-only, so the policy is inspectable); and
  `sparknetctl admin place` curls it. `sparknet-control -dispatch auto` waits
  for readiness and polls placement until a node is eligible, then dispatches.

## Why a pure engine and not inline in dispatch

The decision has non-trivial policy (freshness, multiple tie-breaks, per-node
explanations) and needs to be *explainable* — an operator must be able to ask
"why node-a and not node-b?" without running a task. Keeping it pure makes that
a plain function call and lets it be tested with table-driven cases instead of a
live bus. The bus/registry plumbing that feeds it stays in `internal/server`,
where the locking and the `cluster-serve` check already live.

## Why telemetry is still not authority

Placement reads the same unsigned telemetry ADR 0004 records: a node cannot
grant itself work by lying about free VRAM, because eligibility never bypasses
the registry — `candidates` only ever contains nodes the registry authorizes for
the capability. Telemetry picks *among the authorized*; it can never add one.

## Consequences

1. **A capacity floor is now expressible.** `PlaceAndDispatch` with a
   `MinFreeMemBytes` requirement refuses to send work to a node whose last
   snapshot cannot hold it, and returns a reason when none can.
2. **The admin surface grew one read-only route.** `GET /admin/placement`
   (with `caps`, `min_free_mem`, `max_temp`, `require_healthy`, `max_age`)
   returns the `Decision` as JSON; it performs no write and no dispatch.
3. **`-dispatch auto` is a real selector, not a placeholder.** It waits on
   `App.Ready()`, then polls placement up to `-dispatch-timeout`, so starting
   the control plane before its nodes have reported no longer silently fails.
4. **`App` exposes a readiness signal.** `Ready()` closes once the health
   registry is installed and subscriptions are armed, giving callers a clean
   happens-before for reading placement inputs populated by bus goroutines.
5. **The router is unchanged.** Placement chooses a node; the token-count router
   still decides edge/DGX/cluster escalation. Wiring free VRAM into the router's
   decision is a separate, later step.

## Not decided here

- **The compute-plane `x_ij` assignment.** Placement picks one node; optimally
  sharding a batch across several GPUs stays in the Python/CUDA compute plane.
- **Placement over the KV router's cache locality.** The decision uses capacity,
  not RadixAttention cache affinity; combining them is a later step.
- **Pushing decisions to nodes.** The control plane chooses; it does not yet
  advertise the choice as a scheduling hint on the bus.
