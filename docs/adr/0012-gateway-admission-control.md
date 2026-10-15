# ADR 0012 — Gateway admission control (queueing, not batching)

- **Status:** accepted
- **Date:** 2026-10-15
- **Context:** The seed blueprint's Phase 3 puts "queueing + dynamic batching" in
  the Go inference gateway. That is the right instinct for a from-scratch
  serving stack, but goSpark deliberately is **not** one: it fronts vLLM, which
  already does continuous batching (and paged attention, prefix caching, and the
  scheduler) far better than a proxy in front of it could. A Go batching layer
  would either duplicate the engine's scheduler — wasting the effort and losing
  to it — or hold requests back in a way that defeats the engine's own batching.

## Decision

Implement **edge admission control** in the gateway, and nothing that reorders
or batches prompts.

- **A bounded queue, not a batcher.** `gateway.max_inflight` caps concurrent
  requests proxied to the engine; `gateway.max_queued` caps how many may wait
  for a slot; `gateway.queue_wait` bounds the wait (`0` waits until the request
  context is cancelled). Any of them at `0` disables the limit. The zero value —
  and a nil limiter — admits everything, so the feature is opt-in.
- **Applied to inference only.** `handleProxy` (`/v1/chat/completions` and `/`)
  takes a slot; `/readyz`, `/healthz` and `/v1/models` never do, so the health
  and discovery routes stay answerable while the engine is saturated. A refusal
  is `503` with a `Retry-After` header, never a silent hang.
- **A channel-based limiter, no dependency.** `Limiter` uses a buffered channel
  as a token bag: `len(slots)` is the in-flight count, `cap(slots)` the bound,
  and a release hands the slot directly to a waiting acquirer. No new module
  dependency, and the state is trivially observable.
- **Observable.** `gospark_gateway_inflight_requests`, `_queued_requests`,
  `_max_inflight_requests`, `_max_queued_requests` (gauges) and
  `_admitted_total`, `_rejected_total` (counters) join the `/metrics` surface
  (ADR 0011).

## Why not dynamic batching in the gateway

- **The engine already batches.** vLLM's continuous scheduler admits new
  sequences into a running batch every step. A gateway that collects requests
  into fixed batches would *add* latency (waiting to fill a batch) and then
  hand the engine a rigid group it re-schedules anyway.
- **A proxy cannot do it well.** Batching needs the model's token budget, the
  KV-cache state and the per-sequence deadline — all inside the engine. The
  gateway has none of that; it has HTTP bodies.
- **What the gateway *can* do is shed load.** The failure mode a proxy is
  actually positioned to prevent is oversubscription: thousands of concurrent
  HTTP requests all reaching one engine and thrashing its scheduler. A queue
  with a hard ceiling and a fast refusal turns that into backpressure the
  client can see.

## Consequences

1. **Backpressure is explicit.** A saturated gateway returns `503` +
   `Retry-After` rather than queueing unboundedly, so a caller (or a load
   balancer) can retry elsewhere instead of accumulating tail latency.
2. **No behaviour change by default.** With `max_inflight: 0` the gateway is
   exactly as before; the existing streaming tests are unaffected.
3. **Coalescing identical requests is *not* implemented.** Two clients asking
   the same question still get two upstream calls. Coalescing would need
   response buffering (incompatible with the unbuffered SSE path) and a key that
   includes every sampling parameter; it is left as a possible future addition,
   not a promise.
4. **The queue is in-process.** With several gateway replicas each holds its own
   queue; cluster-wide fair sharing is a routing concern (ADR 0005), not this
   one.

## Not decided here

- Response coalescing / de-duplication of identical concurrent requests.
- A per-tenant or per-capability share of the slots (the blueprint's
  "departmental" fairness); the current ceiling is global.
- Queue depth / latency histograms (an extension of ADR 0011's metrics).
