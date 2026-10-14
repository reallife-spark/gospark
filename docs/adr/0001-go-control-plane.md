# ADR 0001 — A Go control/service plane beside the Python fabric

- **Status:** accepted
- **Date:** 2026-10-14
- **Context:** SparkNet design session ("SparkNet Technical Architecture
  Overview") and the fabric's existing contract SSOT under
  `sparknet/contracts/`.

## Decision

Build goSpark: a standalone Go module (`github.com/reallife-spark/goSpark`)
implementing the fabric's **control and service plane** — identity, capability
verification, task dispatch, routing, the API gateway and the data plane. The
fabric's **compute plane** stays Python/CUDA. goSpark reads the fabric's
contracts as read-only snapshots and never edits the fabric.

## Why Go for this plane

- **Concurrency at the edges.** A node serves many independent KV-block streams
  and gateway requests at once. Go's goroutine-per-stream model and `context`
  cancellation map directly onto "one lost block must not stall the others".
- **A small, static operator binary.** The control plane should run with no
  interpreter, no venv and no CUDA toolkit. `sparknetctl`, `sparknet-control`
  and `sparknet-agent` are single static binaries.
- **The heavy thing is elsewhere.** Inference is the cost centre and it belongs
  to the GPU runtime. Go must *not* grow a tensor library; the boundary is the
  point.

## Why hand-written messages, not generated ones (yet)

`proto/sparknet/v1/*.proto` is the written contract. The compiled types live in
`internal/apiv1` as hand-written structs with a canonical byte form
(`apiv1.CanonicalBytes`). This keeps the build free of `protoc` and makes the
signing bytes auditable in one file. Generating Go from the `.proto` files is a
follow-up; when it happens, `apiv1` becomes the generated package and the
canonical serialisation must stay byte-compatible or every signature breaks.

## Consequences

1. **Two languages, one contract.** The risk is drift. Mitigation: snapshots
   under `spec/sparknet/` with explicit provenance, and the rule that any
   vocabulary/schema change edits the Go mirror in the same change.
2. **The operation whitelist is the sandbox.** An agent executes `arith` and
   `digest`, nothing else. There is deliberately no "run a script" operation,
   because that would make every agent a general remote-code endpoint and void
   the fail-closed model.
3. **The bus is untrusted transport.** NATS/JetStream carries signed bytes; it
   is never asked to *authorise*. A compromised bus can drop or delay, not
   forge.
4. **Compute-serve rides cluster-serve.** The fabric folded `compute-serve`
   into `cluster-serve`; goSpark gates delegated compute on `cluster-serve`.

## Not decided here

- KV-cache transfer over the public internet (the scaffold assumes a WireGuard
  overlay; see `docs/transport.md`).
- A cluster-serve consensus/ratification implementation. `apiv1.ClusterJob`
  carries `RatificationB64`, but the election/signer arc remains the fabric's.
- Model-side tokenisation. The router uses a deterministic estimate precisely
  so the control plane never depends on the model's tokenizer.
