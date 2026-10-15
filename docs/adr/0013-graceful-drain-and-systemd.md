# ADR 0013 — Graceful drain and systemd units for the Go binaries

- **Status:** accepted
- **Date:** 2026-10-15
- **Context:** Only the compute-plane Python worker had a systemd unit
  (`compute/sparknet-compute.service`); the five Go binaries were run by hand.
  Worse, shutdown was abrupt: on `SIGTERM` the process tore down the gateway and
  returned, so an in-flight inference stream or a task being verified could be
  cut off. The seed blueprint's Phase 4 pairs "systemd" with "drain" for exactly
  this reason — a restart should not drop work.

## Decision

Add a drain concept to the control plane and agent, and ship reference systemd
units for the Go binaries.

- **Drain is a state, not a delay.** `App.BeginDrain()` flips an atomic flag
  (`App.Draining()`); it is set on `SIGTERM` (via `ctx` cancellation) and can be
  triggered earlier with `POST /admin/drain`. While draining:
  - `DispatchTask` refuses to enqueue new work (counted in `tasks_refused`);
  - the agent's `onTask` refuses a new task, returning an error so the bus NAKs
    and JetStream redelivers it elsewhere or after restart;
  - the agent heartbeat reports `serving=false`;
  - the gateway's `/readyz` returns 503 with `reason:"draining"` and `proxy`
    rejects new `/v1/chat/completions` with 503, while responses already
    streaming continue to completion.
- **A bounded window, then exit.** After `BeginDrain`, `Run` shuts the gateway
  down with `http.Server.Shutdown`, which waits for active requests up to
  `drainShutdownTimeout` (5 s) before returning. systemd's `TimeoutStopSec` is
  set above it (20 s).
- **Reference units, hardened.** `deploy/sparknet-{control,agent,gpu,backup}.service`
  run each binary as a non-privileged `sparknet` user under
  `NoNewPrivileges`, `PrivateTmp`, `ProtectSystem=strict`, `ProtectHome`, with
  only the state directory (and the backup target) in `ReadWritePaths`. The GPU
  sidecar gets `SupplementaryGroups=video render` and runs `-watch`; the backup
  daemon runs the looping `run` action.
- **Observable.** The drain state is exposed as `gospark_draining` on the
  `/metrics` surface (ADR 0011) and via `sparknetctl admin drain`.

## Why not refuse to start new TCP connections

A listener-level refusal (close the socket, or stop accepting) would break the
in-flight streams that share the connection pool and would make `/readyz` — the
one signal a load balancer actually polls — unreachable. Gating at the request
handlers keeps the health endpoints answerable for the whole drain, which is the
point: a draining process must keep telling the world it is *not* ready.

## Consequences

1. **Restarts stop losing work.** A `systemctl restart` drains first, so a
   streaming completion finishes and a task in flight is verified rather than
   dropped. A task that arrives mid-drain is redelivered, not lost.
2. **Drain is process-local.** It does not fence the whole cluster — a control
   plane that drains still owns the registry it signed; the operator is expected
   to drain peers/agents on the same host (the agent unit does so on its own
   `SIGTERM`). Cluster-wide coordinated drain is a larger, fabric-level concern.
3. **The units are templates.** They assume `/opt/sparknet`, a `sparknet` user
   and `/etc/sparknet/*.env`; they are validated with `systemd-analyze verify`
   but not installed by the build. Packaging them (dpkg/rpm) is out of scope.

## Not decided here

- A configurable drain window (`config.shutdown_timeout`); it is a constant for
  now.
- Cluster-level coordinated drain / leader hand-off.
- Metrics-sidecar collection of the Go binaries' `/metrics` (the scrape target
  is configured where Prometheus runs, not here).
