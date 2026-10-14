#!/usr/bin/env python3
"""Sparknet compute-plane worker (reference implementation).

This is the Python/CUDA side of the goSpark compute seam. The Go agent verifies
the Master-signed task and signs the attested result; this worker is where the
heavy work actually happens, beside the GPU. It exposes two routes:

    GET  /v1/health   -> {"status": "active", ...}
    POST /v1/execute  -> {"result": "<string>"}  or  {"error": "<string>"}

The POST body is {"node_id": ..., "operation": ..., "params_json": ...} and the
caller authenticates with the shared secret in the X-Sparknet-Auth header. The
operation set is intentionally closed: only the names below are accepted, so the
worker cannot be turned into a general remote-code endpoint.

Operations:
    infer  params {"prompt": str, "max_tokens": int?, "temperature": float?}
           -> runs local vLLM chat/completions, returns the generated text.
    embed  params {"text": str}      -> returns a JSON array of float32s.

If no vLLM endpoint is reachable, `infer` returns a deterministic stub so the
bridge can be exercised end to end without a GPU. Set SPARKNET_REQUIRE_VLLM=1 to
make an unreachable engine a hard error instead.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import struct

import httpx
from fastapi import FastAPI, HTTPException, Request, status
from fastapi.responses import JSONResponse

API_KEY_NAME = "X-Sparknet-Auth"
SHARED_SECRET = os.getenv("SPARKNET_SHARED_SECRET", "")
VLLM_ENDPOINT = os.getenv("VLLM_ENDPOINT", "http://127.0.0.1:8000/v1/chat/completions")
VLLM_MODEL = os.getenv("VLLM_MODEL", "Qwen/Qwen2.5-32B-Instruct")
REQUIRE_VLLM = os.getenv("SPARKNET_REQUIRE_VLLM", "0") == "1"
NODE_NAME = os.uname().nodename

# The closed operation set. Adding a name here is a contract change: it must be
# mirrored in the Go node's config.compute.operations list.
OPERATIONS = {name for name in os.getenv("COMPUTE_OPERATIONS", "infer,embed").split(",") if name}

app = FastAPI(title="Sparknet Compute Worker", version="1.0.0")


def _authorize(request: Request) -> None:
    """Reject a caller that does not present the shared secret.

    An empty SHARED_SECRET disables the check, which is only sane on loopback
    against a node that also sends no secret.
    """
    if not SHARED_SECRET:
        return
    if request.headers.get(API_KEY_NAME) != SHARED_SECRET:
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail="invalid Sparknet authorization token",
        )


@app.get("/v1/health")
async def health() -> dict:
    return {
        "status": "active",
        "node": NODE_NAME,
        "operations": sorted(OPERATIONS),
        "vllm": VLLM_ENDPOINT,
        "require_vllm": REQUIRE_VLLM,
    }


@app.post("/v1/execute")
async def execute(request: Request) -> JSONResponse:
    _authorize(request)
    try:
        body = await request.json()
    except Exception as exc:  # noqa: BLE001 - report the parse failure to the caller
        raise HTTPException(status_code=400, detail=f"invalid JSON body: {exc}") from exc

    operation = body.get("operation", "")
    params_raw = body.get("params_json", "{}")
    try:
        params = json.loads(params_raw) if isinstance(params_raw, str) else params_raw
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=400, detail=f"invalid params_json: {exc}") from exc

    if operation not in OPERATIONS:
        raise HTTPException(status_code=400, detail=f"operation {operation!r} not in {sorted(OPERATIONS)}")

    try:
        if operation == "infer":
            result = await _infer(params)
        elif operation == "embed":
            result = _embed(params)
        else:  # unreachable while OPERATIONS is closed, but keeps failure explicit
            raise HTTPException(status_code=400, detail=f"unhandled operation {operation!r}")
    except HTTPException:
        raise
    except Exception as exc:  # noqa: BLE001
        return JSONResponse(status_code=500, content={"error": f"local execution failed: {exc}"})

    return JSONResponse(status_code=200, content={"result": result})


async def _infer(params: dict) -> str:
    prompt = str(params.get("prompt", ""))
    max_tokens = int(params.get("max_tokens", 1024))
    temperature = float(params.get("temperature", 0.2))
    payload = {
        "model": VLLM_MODEL,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens,
        "temperature": temperature,
    }
    try:
        async with httpx.AsyncClient(timeout=60.0) as client:
            resp = await client.post(VLLM_ENDPOINT, json=payload)
            resp.raise_for_status()
            data = resp.json()
        return data["choices"][0]["message"]["content"].strip()
    except Exception as exc:  # noqa: BLE001
        if REQUIRE_VLLM:
            raise
        return _stub_infer(prompt, exc)


def _stub_infer(prompt: str, cause: Exception) -> str:
    """Deterministic stand-in so the bridge works without a GPU.

    It is obviously not a model: it echoes a digest of the prompt so a caller
    can confirm the round trip reached the compute plane.
    """
    digest = hashlib.sha256(prompt.encode("utf-8")).hexdigest()[:16]
    return f"[compute-stub] node={NODE_NAME} prompt_sha256={digest} (vLLM unavailable: {type(cause).__name__})"


def _embed(params: dict) -> str:
    """Reference embedding: a hashed bag-of-words projected to a fixed vector.

    Real deployments replace this with the model's embedding endpoint; the
    contract (a JSON array of floats as the result string) is what matters.
    """
    text = str(params.get("text", ""))
    dim = int(params.get("dim", 256))
    vector = [0.0] * dim
    for token in text.lower().split():
        h = hashlib.sha256(token.encode("utf-8")).digest()
        for i in range(0, len(h), 8):
            idx = struct.unpack("<Q", h[i : i + 8])[0] % dim
            vector[idx] += 1.0
    norm = math.sqrt(sum(v * v for v in vector)) or 1.0
    return json.dumps([round(v / norm, 6) for v in vector])


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(
        "compute_worker:app",
        host=os.getenv("COMPUTE_LISTEN_HOST", "127.0.0.1"),
        port=int(os.getenv("COMPUTE_LISTEN_PORT", "9090")),
        workers=int(os.getenv("COMPUTE_WORKERS", "1")),
    )
