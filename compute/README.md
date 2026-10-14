# Compute-plane worker

The Python/CUDA side of the goSpark compute seam. goSpark verifies the
Master-signed task and signs the attested result; this worker — the blueprint's
`sparknet_bridge.py`, generalised — is where the GPU work runs.

```
                    control plane (Go)                 compute plane (Python/CUDA)
  master ──signed──▶ agent ──verify──┐
                                     │  is the op delegated?
                            ┌────────┴─────────┐
                       local sandbox      compute seam
                     (arith, digest)   POST /v1/execute ──▶ compute_worker.py ──▶ vLLM
                                     │◀────── {"result": "..."} ─────┘
                                     ▼
                          attest (sign) ──▶ bus ──▶ control plane
```

## Contract

- `GET /v1/health` → `200 {"status":"active","operations":[...]}`.
- `POST /v1/execute` with `{"node_id","operation","params_json"}` →
  `200 {"result":"<string>"}` or `{"error":"<string>"}`.
- `GET /v1/models` → OpenAI-style model list (also the readiness probe the Go
  embedder waits on).
- `POST /v1/embeddings` with `{"model?","input": <string|[string]>}` →
  OpenAI-compatible `{"data":[{"index","embedding"}]}`. This is the ingest seam
  `cmd/sparknet-ingest` drives directly; it is not a signed task.
- Auth: the shared secret in header `X-Sparknet-Auth`, matching the node's
  `compute.secret` (or `embed.secret`). An empty secret disables the check — use
  it only on loopback.

The Go side (`internal/compute`, wired into `internal/server`) routes an
operation to the worker only if the node's `config.compute.operations` names it.
Local sandbox operations (`arith`, `digest`) always run in-process and can never
be shadowed. With no `compute.endpoint`, nothing is delegated and the closed
sandbox is unchanged.

## Run

```sh
python3 -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt
cp compute-worker.example.env compute-worker.env   # set the shared secret
set -a; . ./compute-worker.env; set +a
python compute_worker.py
```

Then point a node at it (`configs/agent.example.yaml`):

```yaml
compute:
  endpoint: http://127.0.0.1:9090
  secret: <same as SPARKNET_SHARED_SECRET>
  operations: [infer, embed]
  timeout: 60s
```

Dispatch an `infer` task (the control plane signs it) and the agent returns the
worker's output, attested. Or run a task by hand with the CLI once the fabric is
up.

## Without a GPU

`infer` returns a deterministic `[compute-stub] …` string when vLLM is
unreachable, so the bridge is testable anywhere. Set `SPARKNET_REQUIRE_VLLM=1`
to make an unreachable engine a hard failure.

## Production

`sparknet-compute.service` runs the worker under systemd, hardened and
non-privileged. Replace the stub `embed` with the model's embedding endpoint and
point `VLLM_ENDPOINT` at the real serving engine (see the gateway seam).
