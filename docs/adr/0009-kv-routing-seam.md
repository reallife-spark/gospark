# ADR 0009 — KV-routing seam (RadixAttention residency as a routing input)

- **Status:** accepted
- **Date:** 2026-10-14
- **Context:** `internal/kvtransfer` already implements the NVFP4 KV-cache
  codec and `internal/transport` streams those blocks over HTTP/3 — the wire
  side of the blueprint's "distributed KV cache / RadixAttention" design (§2,
  §4 III, Phase 13 `kv_cache_router.py`). But the router had no way to know
  *where* a prompt's prefill was already cached, so it could send work to a node
  that would recompute a prefix another node already held. The blueprint's
  `kv_cache_router.py` maintains a radix prefix index and routes prompts to
  nodes with KV residency.

  The prefix tree and the prefetch policy are compute-plane (only the engine
  knows what it actually cached), but *which node to send a prompt to* is a
  routing decision — and routing decisions are goSpark's job.

## Decision

Add the missing half of the seam: a client that lets the Go router consult the
compute-plane RadixAttention engine, and an advisory residency hint in the
placement engine.

- **`internal/kvrouter` is the client, not the tree.** It calls a small JSON
  contract on the engine — `POST /v1/kv/lookup`, `POST /v1/kv/register`,
  `GET /v1/kv/health` — authenticating with `X-Sparknet-Auth`. It links no
  model, no attention kernel and no CUDA. The timeout is short by default (2s)
  because a lookup sits on the routing path.
- **Residency is a routing *hint*, not authority.** `placement.Requirement`
  gains `ResidentNode`. When set and that node is an eligible candidate, it is
  ranked first (`better`) and the decision reports
  `"KV-cache prefix resident; prefill already cached"`. If the node is not a
  candidate, or the engine is unreachable, the hint is simply ignored and
  telemetry placement proceeds — a lookup error is not a dispatch error.
- **The dispatch path consults the seam.** `App.PlaceAndDispatch` best-effort
  extracts a `prompt` from the task's `ParamsJSON`; on a residency hit it sets
  the hint. A task with no prompt, or a cold cache, gets no hint and behaves
  exactly as before.
- **The prefix tree stays compute-plane.** `compute/kv_cache_router.py` is the
  reference daemon: a real radix tree over token sequences that marks residency
  along the whole cached path (caching a sequence caches all its prefixes, APC
  semantics) and returns the *longest* matching resident prefix. goSpark only
  reads it.
- **Inspectable over the admin surface.** `GET /admin/kvcache` reports the seam
  status and, with `?prompt=`, the node the engine says holds that prefix;
  `sparknetctl admin kvcache -prompt …` is the CLI.

## Consequences

- New config: `kv.endpoint`, `kv.secret`, `kv.timeout` (plus `GOSPARK_KV_*`).
  Empty `endpoint` (the default) leaves placement to telemetry alone.
- The Go plane still links no model, tokenizer or CUDA. The NVFP4 blocks that
  the tree indexes remain the transport's job (`internal/kvtransfer` +
  `internal/transport`); this ADR only adds the routing query.
- One new client and one reference daemon; no change to the authority model —
  residency is telemetry-like input to a decision, never a capability.
