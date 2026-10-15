# goSpark roadmap — the Gemini blueprint, mapped to owners

The original SparkNet blueprint is a single long Gemini session (captured
verbatim in the fabric vault at
`peer-node/03_Sources/raxxar_raw/gemini-sparknet-session.md`). It specifies a
whole enterprise stack: network tuning, departmental vLLM serving, FastAPI
bridges, a Master orchestrator, knowledge sync, a health/compliance daemon, an
operator CLI, a security gateway, a KV-cache router, backups, an employee
Open WebUI, and an EU-AI-Act compliance mapping.

goSpark is **one slice of that blueprint**: the Go **control and service
plane**. This document maps every module in the blueprint to who owns it, and
says plainly what goSpark has, what is a deliberate seam, and what belongs
elsewhere. The census below follows the blueprint's own consolidated inventory
(Turn 25 §3.1–3.14) plus the concepts introduced in Turn 9.

The rule from the blueprint is the rule here: **Go runs the control and service
plane; Python/CUDA runs the compute plane.** A module that trains, serves or
schedules tensors is *compute plane*; a module that decides, authorizes,
routes, records or transports is *control/service plane*.

## Owner key

| Owner | Meaning |
|---|---|
| **goSpark** | implemented in this repo (Go) |
| **goSpark seam** | goSpark has the interface/hook; the body is compute-plane or future |
| **compute plane** | Python/CUDA/vLLM/ops — belongs beside a GPU, not in the Go plane |
| **fabric** | already specified in `sparknet/contracts`; goSpark must mirror, not invent |
| **out of scope** | not goSpark's job at all (hardware, netplan, OS tuning) |

## Module map (blueprint Turn 25 §3)

| Blueprint module | Owner | In goSpark |
|---|---|---|
| §3.1 `sparknet-net-setup.sh` — RoCEv2, MTU 9000, PFC/ECN, kernel tuning | out of scope (host/ops) | — (see `internal/mesh` for overlay inspection only) |
| §3.2 `docker-compose.yml` — departmental vLLM serving | compute plane | **seam**: `internal/gateway` fronts a vLLM upstream via `/v1/chat/completions`, with `/readyz` readiness, `/v1/models` discovery and unbuffered SSE; the reference engine stack is `compute/docker-compose.vllm.yml` |
| §3.3 `sparknet_bridge.py` — departmental dispatch/orchestration bridge | goSpark | `cmd/sparknet-agent` + `internal/server` (verify → execute → attest); the GPU execution half is now wired through the compute seam (`internal/compute` → `compute/compute_worker.py`), so a signed `infer`/`embed` task reaches vLLM and returns an attested result |
| §3.4 `master_orchestrator.py` — Master Core orchestration/aggregation | goSpark | `cmd/sparknet-control` + `DispatchTask`; aggregation is a **seam**; node selection is capacity-aware via `internal/placement` (`-dispatch auto`) |
| §3.5 `sparknet_knowledge_sync.py` — knowledge indexer/vector retrieval | goSpark seam | `internal/ingest` (crawl + chunk + digest) + `internal/embed` (OpenAI-compatible embeddings client) + `cmd/sparknet-ingest`; the vector store itself stays **compute plane** |
| §3.6 `cluster_health_monitor.py` — health + compliance audit daemon | goSpark | `internal/gputel` + `cmd/sparknet-gpu` (NVML/DCGM) + `internal/server` `Registry`; `nvidia-smi` replaced by direct library reads |
| §3.7 `sparknet-cli` — operator CLI | goSpark | `cmd/sparknetctl` |
| §3.8 `security_gateway.py` — IP whitelist / `X-Sparknet-Auth` gateway | goSpark | `internal/authz` (signed envelopes) + `internal/gateway`; mTLS mesh is **seam** |
| §3.9 `provision_cluster.sh` — node provisioning playbook | out of scope (ops) | — |
| §3.10 `kv_cache_router.py` — distributed RadixAttention + KV prefetch router | goSpark seam | `internal/kvrouter` (residency query) + `internal/router` (decisions) + `internal/kvtransfer` (NVFP4 codec/delta) + `internal/transport` (QUIC); the RadixAttention *tree* is **compute plane** (`compute/kv_cache_router.py` reference) |
| §3.11 `sparknet_backup.py` — storage/knowledge backup daemon | goSpark | `internal/backup` + `cmd/sparknet-backup`: signed, content-addressed incremental snapshots with re-hash verify and restore (no rsync, no shell); the fabric's signed registry/ledger remains the durable core |
| §3.12 `docker-compose.webui.yml` — Open WebUI workstation | compute plane | **seam**: `internal/gateway` is the routing/auth endpoint it calls; Open WebUI runs in `compute/docker-compose.vllm.yml` (profile `webui`) pointed at the gateway |
| §3.13 `sparknet_router_pipeline.py` — Open WebUI routing/auth pipeline | goSpark seam | `internal/router` + `internal/gateway`; persona/context injection is **compute plane** |
| §3.14 `sparknet_e2e_test.py` — end-to-end validation suite | goSpark | `make test`, `make test-nats`, `tests/` integration suites |

## Concept map (blueprint Turn 9)

| Concept | Owner | In goSpark |
|---|---|---|
| Departmental GPU delegation & optimization (the `x_ij` assignment) | compute plane (node choice: goSpark) | **goSpark**: `internal/placement` picks the node by free-VRAM / temp / health; the per-node *sharding* (the `x_ij` optimization) stays compute plane |
| Master→Department aggregation over secure gRPC | goSpark | bus (NATS/JetStream) carries signed tasks/results; gRPC is the blueprint's transport, the fabric chose a store-and-forward bus |
| Disaggregated prefill/decode across the fleet | compute plane | — |
| Global distributed KV-cache fabric via RDMA/RoCE | compute plane | **seam**: `internal/transport` moves NVFP4 blocks over QUIC; RDMA/RoCE is host-layer |
| Federated RadixAttention prefix tree | compute plane | **seam**: `internal/kvrouter` queries the engine's residency and `internal/router`/`internal/placement` route on it; the tree itself is compute-plane state (`compute/kv_cache_router.py`) |

## What goSpark already does (vs the blueprint)

- **Control-plane core** — signed `TaskEnvelope` dispatch, fail-closed
  verification, attested results (`internal/authz`, `internal/server`).
- **Signed node registry** — per-entry signatures, revocation, rollback guard,
  live loopback admin surface (`internal/authz/registry.go`,
  `internal/server/admin.go`; ADR 0002/0003).
- **Service plane** — SSE-aware API gateway in front of vLLM, with a `/readyz`
  readiness probe and `/v1/models` discovery (`internal/gateway`; ADR 0007).
- **Data plane** — NVFP4 (E2M1) codec, delta cache, HTTP/3 QUIC transport
  (`internal/kvtransfer`, `internal/transport`).
- **GPU telemetry** — NVML metrics + DCGM health via a pure-Go core and a single
  cgo sidecar, surfaced read-only on `GET /admin/gpu`
  (`internal/gputel`, `cmd/sparknet-gpu`; ADR 0004).
- **Telemetry-aware dispatch** — a pure placement engine ranks the authorized
  nodes by free accelerator memory, temperature, utilization and health, with
  expressible resource floors and per-node reasons; `-dispatch auto` and
  `GET /admin/placement` / `sparknetctl admin place` surface it
  (`internal/placement`; ADR 0005).
- **Compute-plane bridge** — a node may delegate explicitly-named operations to
  a Python/CUDA worker and attest the worker's result; the closed sandbox stays
  the default and local operations can never be shadowed. Ships with the
  reference worker `compute/compute_worker.py` (`internal/compute`; ADR 0006).
- **Ingest seam** — the indexer crawls, chunks and emits a JSON index, and can
  embed each chunk through an OpenAI-compatible embeddings service; the worker
  serves `/v1/embeddings` and falls back to a deterministic hashed embedding so
  the pipeline runs without a GPU (`internal/ingest`, `internal/embed`,
  `cmd/sparknet-ingest`; ADR 0008).
- **KV-routing seam** — the router asks a compute-plane RadixAttention engine
  which node holds a prompt's cached KV prefix and ranks that node first; a miss
  or an unreachable engine degrades to telemetry placement (`internal/kvrouter`
  + the `ResidentNode` placement hint; ADR 0009).
- **Operator CLI** — `cmd/sparknetctl` (identity, registry, admin, routing,
  blocks, `admin gpu`, `admin place`, `admin health`, `admin kvcache`).
- **Backup seam** — signed, content-addressed incremental snapshots with re-hash
  verify and restore, driven by `cmd/sparknet-backup` (no rsync, no shell;
  `internal/backup`; ADR 0010).

## Deliberate seams (interface exists, body is elsewhere)

1. **Compute plane** — no model, tensors, tokenizer or CUDA is linked in. It is
   reached three ways: the gateway proxies inference requests, agents delegate
   named GPU operations over the compute seam to `compute/compute_worker.py`,
   and the router queries KV-prefix residency from
   `compute/kv_cache_router.py`. Those doors are the only contact; the runtime
   is never linked.
2. **VRAM-aware scheduling** — node *selection* is capacity-aware
   (`internal/placement`: dispatch avoids a node that cannot hold the work and
   prefers more free VRAM); the token-count router's own edge/DGX/cluster
   escalation is unchanged, and per-node sharding is compute-plane.
3. **RadixAttention / KV routing** — the codec and transport are real, and the
   residency *query* is built (`internal/kvrouter` + the placement hint); the
   prefix tree and prefetch policy are compute-plane.
4. **Knowledge index** — crawl, chunk, digest and the embeddings client exist
   (`internal/ingest` + `internal/embed` + `cmd/sparknet-ingest`); the vector
   store itself is compute-plane.
5. **gRPC vs bus** — the blueprint uses gRPC endpoints; the fabric uses a
   NATS/JetStream store-and-forward bus (tolerant of an intermittent WireGuard
   WAN). goSpark follows the fabric.

## EU AI Act mapping (blueprint Turn 25 §4), goSpark's share

| Article | Requirement | goSpark contribution |
|---|---|---|
| Art. 9 | Risk management | `cmd/sparknet-gpu` + `internal/gputel` thermal/memory/health telemetry; control-plane health `Registry` |
| Art. 10 | Data governance & provenance | signed envelopes + `internal/ingest` (crawl/chunk); storage stays local, no cloud |
| Art. 11 | Technical documentation | `docs/architecture.md`, `docs/adr/`, this roadmap |
| Art. 12 | Record-keeping / logging | attested results, `internal/telemetry` counters, per-entry registry ledger, signed backup snapshots (`internal/backup`) |
| Art. 13 | Transparency | gateway/router attribution surfaces; UI labeling is compute plane |
| Art. 14 | Human oversight | admin surface is a human-in-the-loop control; deployment policy is ops |
| Art. 15 | Security & robustness | Ed25519 signed authority, fail-closed verification, QUIC transport |

## Not in this roadmap

- Transposition of the whole blueprint into Go — only the control/service slice.
- Replacing the fabric's Python authz or contracts; goSpark mirrors them
  (`spec/sparknet/`).
- Any host-layer work (netplan, MTU, PFC, nftables, WireGuard config), which the
  peer/master roles already own.
