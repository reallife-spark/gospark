# ADR 0014 — A read-only MCP server over the admin surface

- **Status:** accepted
- **Date:** 2026-10-15
- **Context:** The seed blueprint's NVIDIA-tooling survey points at the emerging
  MCP (Model Context Protocol) pattern for exposing a service to agent tooling.
  goSpark's control plane already has an ideal backend for this — the loopback
  admin surface (`/admin/health`, `/admin/metrics`, `/admin/registry`,
  `/admin/gpu`, `/admin/placement`) — but reaching it today needs curl or the
  `sparknetctl admin` CLI. An agent or IDE that speaks MCP cannot use it.

## Decision

Add `cmd/sparknet-mcp`, a stdio MCP server (package `internal/mcp`) that exposes
goSpark's **read-only** admin routes as MCP tools.

- **Read-only, by construction.** The tools are `gospark_health`,
  `gospark_metrics`, `gospark_registry`, `gospark_gpu` and `gospark_placement`
  — all `GET`s. The mutating admin routes (`authorize`, `revoke`, `drain`) are
  deliberately **not** exposed as tools, so an MCP client cannot change
  authorization or stop the plane through this path. The authority model is
  unchanged: MCP grants visibility, not authority.
- **No dependencies.** `internal/mcp` speaks JSON-RPC 2.0 over stdio with the
  standard library only: it decodes a stream of JSON values from stdin and
  writes responses to stdout, handling `initialize`, `ping`, `tools/list`,
  `tools/call` and swallowing notifications. A tool's own failure is returned as
  MCP content with `isError: true` (the protocol convention), not as a JSON-RPC
  error.
- **It fronts the existing admin surface, not the bus.** The server simply
  issues HTTP GETs to `AdminBase` (flag `-addr`, env `GOSPARK_ADMIN_ADDR`,
  default `http://127.0.0.1:8899`). It adds no new trust boundary: to reach the
  control plane it must already be able to reach the loopback admin port.
- **Pure Go.** `make build` produces `bin/sparknet-mcp` with `CGO_ENABLED=0`,
  like the other control/service-plane binaries.

## Why read-only

MCP tools run inside an agent loop that may be prompted by untrusted content. An
MCP tool that can revoke a node or drain the plane turns a prompt-injection into
a denial of service or an authorization change. The mutating surface already has
the right shape — an operator on the master host, over loopback, with the Master
key in process — and widening it to an agent is a fabric-level decision (a
signed admin request, ADR 0003's "not decided here"), not something to ship as a
convenience.

## Why not the NVIDIA-specific tooling

The survey names NVIDIA's ecosystem tools and an "MCP/CLI" pattern. goSpark is
the control/service plane; the engine-side tooling (TensorRT-LLM, Triton, the
GPU Operator) belongs on the compute plane, not behind a Go control plane's MCP
server. This ADR implements the *generic* MCP seam — goSpark's observability
surface as tools any MCP client can call — and leaves a specific NVIDIA vendor
integration (if one is wanted) as a separate, additive tool set.

## Consequences

1. **Agent-accessible observability.** An MCP client can ask "what is the
   cluster's GPU temperature / which node would be chosen / what does the
   registry say" and get the same answer `/admin/*` gives an operator, with no
   new endpoint or client.
2. **One more binary to build and deploy.** `bin/sparknet-mcp` is built by
   `make build`; it is not a systemd service (it is spawned by the MCP client),
   so it takes no unit.
3. **The tool set is a fixed list.** Adding a tool means editing `toolList`; that
   is intentional — the exposed surface should be reviewed, not reflected
   automatically off the admin mux (which would silently expose a future
   mutating route).

## Not decided here

- A mutating MCP surface (needs the fabric's signed-admin design).
- MCP transports beyond stdio (HTTP/SSE) for remote clients.
- A vendor-specific (NVIDIA) tool set layered on top of the generic one.
