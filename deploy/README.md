# Deploying goSpark under systemd

Reference units for running the Go binaries as hardened systemd services. They
are templates: adjust paths, users and `EnvironmentFile` values to your host.
The compute-plane Python worker has its own unit — see
`compute/sparknet-compute.service`.

| Unit | Binary | Role |
|---|---|---|
| `sparknet-control.service` | `sparknet-control` | control plane (master) |
| `sparknet-agent.service` | `sparknet-agent` | node agent (peer) |
| `sparknet-gpu.service` | `sparknet-gpu` | GPU telemetry sidecar (`-watch`) |
| `sparknet-backup.service` | `sparknet-backup` | signed backup daemon (`run`) |

## Install

```sh
sudo useradd --system --home /opt/sparknet --shell /usr/sbin/nologin sparknet
sudo install -d -o sparknet -g sparknet /opt/sparknet/.state /etc/sparknet
# place the built binaries under /opt/sparknet/bin (make build / make build-gpu)
sudo install -m 0644 deploy/sparknet-*.service /etc/systemd/system/
# per-unit environment files, mode 0600 (they name key paths and secrets):
sudo install -m 0600 /dev/null /etc/sparknet/control.env   # then edit
sudo systemctl daemon-reload
sudo systemctl enable --now sparknet-control sparknet-agent sparknet-gpu
```

Each `EnvironmentFile` supplies the variables the `ExecStart` references, e.g.
`control.env`:

```
GOSPARK_CONTROL_CONFIG=/etc/sparknet/control.yaml
GOSPARK_MASTER_KEY=/opt/sparknet/.state/control.key
```

`agent.env` adds `GOSPARK_AGENT_CONFIG`, `GOSPARK_NODE_KEY`,
`GOSPARK_MASTER_PUB`. `gpu.env` and `backup.env` set their config paths (and the
backup signing key). The units are `ProtectSystem=strict`, so only the paths in
`ReadWritePaths` are writable.

## Drain and shutdown

`sparknet-control` and `sparknet-agent` handle `SIGTERM` (systemd's stop signal)
as a **drain**: the process stops accepting new work — new task dispatches are
refused, `/readyz` reports 503, and the agent reports `serving=false` — while
in-flight work finishes, bounded by the internal drain window (5 s).
`TimeoutStopSec=20` leaves room for that plus socket teardown; keep it above the
drain window if you change it.

An operator can drain early, before a restart, without stopping the unit:

```sh
bin/sparknetctl admin drain          # POST /admin/drain on the loopback admin surface
```

The drain state is visible in `bin/sparknetctl admin metrics` as
`gospark_draining` and by scraping `/metrics` (set `metrics.listen`).
