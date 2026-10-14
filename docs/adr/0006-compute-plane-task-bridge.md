# ADR 0006 — Compute-plane task bridge

- **Status:** accepted
- **Date:** 2026-10-14
- **Context:** Every seam in this repository names the same boundary: **Go runs
  the control and service plane; Python/CUDA runs the compute plane** (roadmap,
  ADR 0001). The closed operation sandbox (`internal/server/ops.go`) enforces
  that in the strongest form — the executor runs only `arith` and `digest` and
  there is deliberately no "run this script" operation. But the blueprint's
  central node-side component, `sparknet_bridge.py` (§3.3), is exactly a bridge
  that *receives a task and runs inference against a local GPU engine*. With
  nothing but the local sandbox, a node could verify a task but had no way to
  actually perform GPU work, so the blueprint's serving path was unreachable.
  The special-cased `-dispatch auto` and admin surfaces had no compute to feed.

## Decision

Add an explicit, opt-in **compute-plane seam**. A node may name a
Python/CUDA worker and the operations that worker is trusted to run; the Go
plane then verifies the Master-signed task, delegates execution of *those named
operations* to the worker, and still signs the attested result.

- **`internal/compute` is the seam, and it is small.** It defines an
  `Executor` interface (`Handles`, `Execute`, `Health`, `Operations`) and one
  implementation, `HTTPExecutor`, which POSTs `{node_id, operation, params_json}`
  to the worker's `/v1/execute` and reads back `{result}`. Authentication is the
  blueprint's shared secret in the `X-Sparknet-Auth` header. There is no model,
  tensor, CUDA or tokenizer code here — the package only moves bytes to the
  process that has them.
- **The sandbox stays the default; delegation is a named exception.**
  `App.dispatchOperation` tries the local closed whitelist first, so a local
  operation can never be shadowed; only an operation listed in
  `config.compute.operations` is delegated; everything else is still refused with
  `ErrUnknownOperation`. With no `compute.endpoint`, the seam is inert and the
  node behaves exactly as before.
- **Config refuses the dangerous shapes.** `config.Validate` rejects a
  delegated operation that shadows `arith`/`digest`, an empty operation name, a
  duplicate, an `endpoint` with no `operations`, and `operations` with no
  `endpoint`. The config package keeps its own copy of the two local operation
  names rather than importing `internal/server`, so it stays free of that
  dependency and the collision check cannot silently drift.
- **The trust boundary is the worker, not the Go plane.** The Go plane forwards
  the parameters but does not interpret them; the worker owns its closed
  operation set, its engine and its GPU. The authority to run *any* operation at
  all is unchanged: the task must still be Master-signed and capability-gated
  (`cluster-serve`), and the node must still be registry-authorized.
- **A reference worker ships beside it.** `compute/compute_worker.py` is the
  blueprint's FastAPI daemon generalised: `/v1/health` and `/v1/execute`, the
  shared-secret check, a closed operation set (`infer`, `embed`), and a
  deterministic stub when vLLM is unreachable so the seam is testable without a
  GPU. `compute/` also carries a hardened systemd unit and an example env file.

## Consequences

- The blueprint's node-side serving path is now implementable end to end:
  Master signs a task → agent verifies → agent delegates to the worker → worker
  runs the model → agent attests → control verifies. This was proven live across
  processes with Go control + Go agent + the Python worker (see the session log).
- The Go binary still links no CUDA and still builds with `CGO_ENABLED=0`; the
  seam is pure `net/http`.
- The seam makes a node *capable* of delegating execution to an operator-chosen
  process. That is the point of the split, but it is worth stating plainly: the
  safety property is not "the Go plane runs nothing" — it is "the Go plane runs
  nothing *that was not explicitly configured by the operator*, and the compute
  plane is the designated trust domain for GPU work."
- The later seams (gateway → vLLM, ingest → embeddings, router → RadixAttention)
  reuse this same worker contract; this ADR establishes the pattern, and the
  worker's `/v1/execute` operation set grows with them.
