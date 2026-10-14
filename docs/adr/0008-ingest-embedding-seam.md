# ADR 0008 — Knowledge ingest seam (crawl → chunk → embed)

- **Status:** accepted
- **Date:** 2026-10-14
- **Context:** `internal/ingest` already crawls a vault or repo concurrently,
  chunks markdown/text and emits stable sha256 digests for delta sync — but it
  was a library with no driver and no way to turn text into vectors. The
  blueprint's §3.5 knowledge indexer (`sparknet_knowledge_sync.py`) indexes local
  vaults into a local vector store so agents can query domain context without
  leaving the department. Embedding is model work, so it belongs beside the GPU:
  the Go plane must drive the pipeline without linking a tokenizer or CUDA.

## Decision

Split the pipeline at the same kind of seam as inference. Go owns the I/O-bound
work; the compute plane owns the model.

- **`internal/embed` is an OpenAI-compatible client, not a model.** It POSTs a
  batch of texts to `/v1/embeddings` (auth in `X-Sparknet-Auth`) and returns
  vectors in input order. It honours the server's `index` field when the server
  reorders results, and it fails loudly on a short or mismatched result rather
  than silently misaligning a knowledge index. It also probes `/v1/models` for
  readiness. It links no tokenizer, no tensor and no CUDA.
- **`internal/ingest` gains a driver, not a model.** `ingest.Embedder` is a local
  interface (dependency inversion: ingest never imports `internal/embed`), and
  `ingest.EmbedChunks` batches crawled chunks through it, pairing each chunk with
  its vector and preserving order.
- **`cmd/sparknet-ingest` is the operator entry point.** `-root` crawls;
  `-embed-endpoint` (or `embed.endpoint` config) vectorizes; `-out` writes the
  index as JSON. With no endpoint it stops at crawl-and-chunk, so the seam is
  additive and the GPU-free path is unchanged.
- **The reference worker serves embeddings too.** `compute/compute_worker.py`
  exposes `/v1/embeddings` and `/v1/models`; when `SPARKNET_EMBED_ENDPOINT` is
  set it proxies a real embeddings engine, otherwise it returns a deterministic
  hashed embedding. The pipeline is therefore testable end to end without a GPU.
- **The seam is not the signed-task path.** Embeddings are batch I/O from the
  local ingest driver; they are not Master-signed tasks. The `embed` operation
  remains available over `/v1/execute` for signed work, and both share the same
  closed operation/auth vocabulary.

## Consequences

- One more config block: `embed.endpoint`, `embed.secret`, `embed.model`,
  `embed.batch`, `embed.timeout` (plus `GOSPARK_EMBED_*` overrides). All
  optional; empty `endpoint` keeps crawl-and-chunk only.
- The Go binaries still link no model, tokenizer or CUDA. Vectors cross the seam
  as JSON; the vector *store* is compute-plane and out of scope here.
- The same worker serves inference (signed), embeddings (batch) and inference
  proxying (the gateway): three doors, one engine, no shared model state in Go.
