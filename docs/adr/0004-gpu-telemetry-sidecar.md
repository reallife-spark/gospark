# ADR 0004 — GPU telemetry as a separate cgo sidecar

- **Status:** accepted
- **Date:** 2026-10-14
- **Context:** The session's "Cluster Health Monitor" agent scraped GPU state by
  shelling out to `nvidia-smi --query-gpu=…`. goSpark had no GPU visibility at
  all: the control plane could route and dispatch, but could not report a node's
  GPU temperature, memory, utilization or health. The GPU libraries (NVML, DCGM)
  are C and require cgo, which directly conflicts with ADR 0001's decision that
  the goSpark binaries are static and GPU-free.

## Decision

Add GPU telemetry, but keep it out of the core binaries.

- **A pure-Go core, a cgo edge.** `internal/gputel` defines the `Collector`
  interface, the `GPU`/`Health`/`Snapshot` types, a `Null` collector, a
  merge-on-read `Multi`, a self-registration registry and the single wire
  projection (`Snapshot.Wire`). It links no GPU library. The cgo backends live in
  `internal/gputel/nvml` and `internal/gputel/dcgm` behind build tags
  (`cgo && linux`) with non-cgo stubs that register nothing, so `Default()`
  honestly returns a `Null` ("no GPU backend built in") on a CGO_ENABLED=0 build.
- **One cgo binary.** `cmd/sparknet-gpu` is the only binary that imports the
  backends. `make build` builds the control/agent/CLI with `CGO_ENABLED=0`
  (preserving ADR 0001); `make build-gpu` builds the sidecar with
  `CGO_ENABLED=1`.
- **Consume the NVIDIA Go libraries by path, not by copy.**
  `github.com/NVIDIA/go-nvlib` (v0.9.0) and `github.com/NVIDIA/go-dcgm`
  (v1.4701.2) are consumed via `require` + a local `replace` to
  `/mnt/nvme/github`. Both `dlopen` their C library with
  `-Wl,--unresolved-symbols=ignore-in-object-files`, so the sidecar *builds on a
  host with no driver* and degrades at runtime.
- **Telemetry is never authority.** The sidecar publishes a `NodeHealth`
  carrying a `GpuTelemetry` on the existing-but-previously-unused `sparknet.health`
  subject, as a `RegisterNode`. The control plane records it in a map separate
  from the agent `Registry`, keyed by node id, and never verifies it as a signed
  envelope — a telemetry report carries no capabilities and grants nothing. It is
  read back through the read-only `GET /admin/gpu` (and `sparknetctl admin gpu`).

## Why a sidecar and not in the agent

The agent is a *node identity* that verifies and executes control-plane tasks; it
must stay a static, dependency-light binary that runs on any node, GPU or not.
Linking NVML/DCGM into it would make it cgo-only and would drag a GPU library
into every deployment. A separate sidecar keeps the choice per-host: a GPU node
runs `sparknet-gpu -watch`; a control node without a GPU runs neither, and
`GET /admin/gpu` simply reports nothing. It also matches the session's own
layering (a monitoring daemon beside the compute plane, not inside it).

## Why `dlopen` degrades rather than fails to build

Pinning the libraries and building them unconditionally means the *build* proves
the code compiles against the real headers even on a machine with no GPU, and the
*run* decides availability. A missing `libnvidia-ml.so` / `libdcgm.so.4` becomes a
reported reason string (`nvml: … ERROR_LIBRARY_NOT_FOUND`,
`dcgm: … libdcgm.so.4 not found`), not a crash and not a build-time fork.

## Consequences

1. **The core stays static and testable without a GPU.** `make build` and
   `make ci` never touch cgo; `internal/gputel` unit tests run against `Null`
   and `Multi` with no driver present.
2. **`sparknet.health` is now live.** It was declared in `internal/bus` but
   unused; the control plane now subscribes to it. Ordinary agents still publish
   `register`, so a health report without telemetry is ignored.
3. **The wire contract grew additively.** `apiv1` gained `GpuMetrics` /
   `GpuTelemetry` and `NodeHealth.GPUTelemetry` (field 10 in the `.proto`).
   Health is unsigned, so the canonical bytes that signatures cover are
   unaffected; the change is backward-compatible.
4. **The sidecar's GPU snapshots are unavailable on this host.** There is no
   NVIDIA driver here, so the live smoke reports `available:false` with the
   reason — which is the point: the plumbing is proven end to end
   (sidecar → NATS → `/admin/gpu`) without a GPU. Real metrics appear unchanged
   on a node with a driver.

## Not decided here

- **Delta/diffing of snapshots** (alerting on a threshold crossing) — the
  control plane stores the latest snapshot only.
- **GPU capacity for the router.** The router still decides on token counts, not
  free VRAM; feeding telemetry into scheduling is a later step.
- **A vLLM/compute-plane health signal** merged into the same view.
