# ADR 0007 — Gateway serving seam (readiness, model discovery, streaming)

- **Status:** accepted
- **Date:** 2026-10-14
- **Context:** `internal/gateway` has always been the Go door to the compute
  plane: it proxies `/v1/chat/completions` to a local vLLM upstream and streams
  SSE back. But it was a *bare* reverse proxy — it could not tell a caller
  whether the engine behind it was actually serving, it exposed no model
  discovery, and its streaming guarantee ("flush per write") stopped at the
  process boundary, so a reverse proxy in front could still buffer tokens. The
  blueprint's §3.2 (departmental vLLM serving) and §3.12 (Open WebUI) both need
  a gateway that reports its own readiness and what it fronts.

## Decision

Close the serving seam in `internal/gateway`, keeping it a pure, GPU-free proxy.

- **Liveness and readiness are separate.** `/healthz` is liveness: the gateway
  process is up, reported fast and without touching the upstream. `/readyz` is
  readiness: it probes the upstream's models endpoint (default `/v1/models`,
  bounded by a probe timeout) and answers `503` until the engine responds, then
  `200 {"ready":true,...}`. A gateway that cannot reach its engine is not ready,
  and an operator (or an orchestrator) can see that without guessing.
- **Model discovery is truthful.** `/v1/models` proxies the live engine when an
  upstream is configured, so the answer is the engine's own model list. With no
  upstream it reports the gateway's declared `model` (the model this node
  fronts), and `503` if none is declared. It never invents a model list when the
  engine is down — a failed proxy is a `502`, not a fabricated success.
- **Streaming is unbuffered end to end.** A `text/event-stream` response is
  forwarded with `X-Accel-Buffering: no` and `Cache-Control: no-cache` in
  addition to the existing flush-per-write loop, so an nginx-style reverse proxy
  in front does not hold tokens back. The proxy is otherwise unchanged: the
  goroutine streams in 32 KiB reads and flushes each write.
- **The engine stays compute-plane.** `docker-compose.vllm.yml` is a reference
  stack (vLLM + optional Open WebUI) that the gateway points at; nothing in it is
  linked into the Go binaries. Open WebUI talks to the *gateway*, so routing,
  authentication and attribution remain in goSpark.

## Consequences

- A deployment can health-check the Go plane and the Python engine independently
  and route around a gateway whose engine is still loading.
- New config: `gateway.model`, `gateway.ready_path`, `gateway.probe_timeout`
  (plus `GOSPARK_GATEWAY_*` overrides). All optional; defaults keep the old
  behaviour for `/healthz` and the proxy path.
- The gateway still performs no inference, holds no model state and links no
  CUDA — the seam is `net/http` and configuration.
- The same worker/engine sits behind both doors: the gateway for interactive
  chat, the compute seam (`internal/compute`) for delegated task operations.
