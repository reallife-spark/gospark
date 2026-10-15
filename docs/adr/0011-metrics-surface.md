# ADR 0011 — A Prometheus metrics surface

- **Status:** accepted
- **Date:** 2026-10-15
- **Context:** goSpark exposed its state three ways — the `/admin/health`,
  `/admin/gpu` and `/admin/placement` JSON routes, the counters behind the
  control plane's own logs, and the `sparknet-gpu` sidecar's one-shot output.
  None of it was scrapeable, so an operator could see a snapshot only by running
  a command; there was no time series to alert on. The seed blueprint's Phase 4
  asks for exactly this: "expose a `/metrics` route in Go tracking GPU memory
  consumption, core utilization percentages and API request latency."

## Decision

Add a Prometheus text-exposition surface, in pure Go, with no client-library
dependency.

- **A pure renderer, `internal/metrics`.** `Write(io.Writer, Snapshot)` renders
  the exposition format (v0.0.4): `# HELP`/`# TYPE` lines and samples, labels
  escaped (`\`, `"`, newline) and sorted for determinism. The package takes a
  plain-value `Snapshot`, so the policy of *what to expose* stays in
  `internal/server` and the renderer only formats. The format is a handful of
  lines; a client library would put a schema and a dependency inside the control
  plane for no gain, and the metric set is hand-picked, not instrumented ad hoc.
- **Two surfaces, one handler.** `/admin/metrics` is served on the existing
  loopback-only admin mux, so an operator can `curl` it without any new
  listener. A non-empty `metrics.listen` (`GOSPARK_METRICS_LISTEN`,
  **disabled by default**) additionally starts a standalone server answering
  `/metrics` and `/healthz` for a Prometheus scrape.
- **Content: counters and state, not authority.** The counter set
  (`gospark_tasks_verified_total`, `_refused_total`, `_results_attested_total`,
  `gospark_blocks_*`, `gospark_bytes_streamed_total`) is emitted as `counter`;
  per-node health and authorization (`gospark_node_serving`,
  `_load_permille`, `_authorized`) and per-device accelerator telemetry
  (`gospark_gpu_memory_used_bytes`, `_memory_total_bytes`,
  `_utilization_percent`, `_memory_utilization_percent`,
  `_temperature_celsius`, `_power_watts`, `_healthy`) as `gauge`. A metric family
  is declared only when it has a sample, so an unconfigured registry leaves no
  empty `gospark_node_authorized`.
- **One source of truth for the node view.** `metricsSnapshot` reuses
  `healthSnapshot` — the same join `/admin/health` serves — so the node set and
  authorization verdicts cannot drift between the two routes.

## Why not the `prometheus/client_golang` registry

The library is designed for a program that increments many metrics from many
call sites and wants a global registry. goSpark already keeps its counters in one
place (`internal/telemetry`) and its node view in one place
(`internal/server`); the exposition is a projection of two existing snapshots,
not a new instrumentation layer. Rendering it directly keeps one dependency out
of the module graph and the metric names reviewable in a single file.

## Consequences

1. **Scrapeable without a new trust boundary by default.** The default posture is
   unchanged: no listener, and the read-only route rides the loopback admin
   surface. Enabling `metrics.listen` is an explicit act with a documented
   caveat — the snapshot carries node ids, authorization state and accelerator
   telemetry.
2. **API request latency is not yet a metric.** The seed names it; the gateway
   does not currently record per-request latency, and a histogram is a larger
   change than this ADR. It is a follow-up (a gateway request-duration histogram,
   recorded where `/v1/chat/completions` is proxied).
3. **`gospark_build_info` needs a version to be useful.** `server.Version`
   defaults to `"dev"` and is meant to be stamped at build time with
   `-ldflags "-X ...internal/server.Version=..."`.

## Not decided here

- A gateway latency histogram and request-count-by-status metric.
- Whether the standalone listener should ever authenticate (it currently assumes
  a trusted network).
