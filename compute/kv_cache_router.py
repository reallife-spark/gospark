#!/usr/bin/env python3
"""Sparknet Distributed RadixAttention & KV-cache residency router (reference).

This is the compute-plane side of the KV-routing seam — the blueprint's
`kv_cache_router.py`, made honest. goSpark's Go router is the *client*: it asks
this daemon where a prompt's cached prefix is resident, then routes the request
to that node so the prefill is not recomputed. The daemon owns the prefix tree
and the prefetch policy, because only the engine that holds the KV blocks knows
what is actually cached.

Routes (all authenticate with X-Sparknet-Auth):

    GET  /v1/kv/health    -> {"status": "active", "prefixes": N, "nodes": [...]}
    POST /v1/kv/lookup    {"prompt": "..."} -> {"node","matched_tokens","prefix_hash"}
                          node is "" when nothing matches (a cold cache).
    POST /v1/kv/register  {"prefix": "...", "node": "..."} -> {"ok": true, "prefixes": N}

Unlike the blueprint's snippet, lookup returns the *longest* matching resident
prefix (a real radix walk), not the first token's residency, and registration
merges nodes along the shared prefix instead of duplicating a path per prompt.

This daemon holds no model and no tensors; it is the index of where the KV cache
lives, over the RoCEv2 fabric the transport already streams the NVFP4 blocks on.
"""

from __future__ import annotations

import hashlib
import os
import threading
import time

from fastapi import FastAPI, HTTPException, Request, status
from fastapi.responses import JSONResponse

API_KEY_NAME = "X-Sparknet-Auth"
SHARED_SECRET = os.getenv("SPARKNET_SHARED_SECRET", "")
# Only the first N tokens form a prefix key; beyond that the shared prefix is
# vanishingly unlikely and the tree would grow without bound.
MAX_PREFIX_TOKENS = int(os.getenv("KV_MAX_PREFIX_TOKENS", "64"))

app = FastAPI(title="Sparknet KV-cache Router", version="1.0.0")


class RadixNode:
    __slots__ = ("token", "children", "node_residency", "last_accessed")

    def __init__(self, token: str = ""):
        self.token = token
        self.children: dict[str, RadixNode] = {}
        self.node_residency: list[str] = []
        self.last_accessed = time.time()


class RadixTree:
    """A prefix tree over token sequences with per-node KV residency."""

    def __init__(self) -> None:
        self.root = RadixNode()
        self._lock = threading.Lock()

    @staticmethod
    def tokenize(text: str) -> list[str]:
        return text.split()[:MAX_PREFIX_TOKENS]

    @staticmethod
    def prefix_hash(tokens: list[str]) -> str:
        return hashlib.sha256(" ".join(tokens).encode("utf-8")).hexdigest()[:16]

    def register(self, prefix: str, node: str) -> int:
        tokens = self.tokenize(prefix)
        if not tokens or not node:
            raise ValueError("prefix and node are required")
        with self._lock:
            current = self.root
            for token in tokens:
                nxt = current.children.get(token)
                if nxt is None:
                    nxt = RadixNode(token)
                    current.children[token] = nxt
                current = nxt
                # Caching a sequence caches every prefix of it (the KV of the
                # full sequence includes all earlier positions), so mark
                # residency at each node along the path, not just the leaf.
                if node not in current.node_residency:
                    current.node_residency.append(node)
                current.last_accessed = time.time()
            return self._count_locked()

    def lookup(self, prompt: str) -> dict:
        tokens = self.tokenize(prompt)
        best_node = ""
        best_depth = 0
        best_hash = ""
        with self._lock:
            current = self.root
            for depth, token in enumerate(tokens, start=1):
                nxt = current.children.get(token)
                if nxt is None:
                    break
                current = nxt
                if current.node_residency:
                    # Longest matching prefix that has residency wins.
                    best_node = current.node_residency[0]
                    best_depth = depth
                    best_hash = self.prefix_hash(tokens[:depth])
                    current.last_accessed = time.time()
        return {"node": best_node, "matched_tokens": best_depth, "prefix_hash": best_hash}

    def _count_locked(self) -> int:
        total = 0
        stack = [self.root]
        while stack:
            node = stack.pop()
            total += 1
            stack.extend(node.children.values())
        return total

    def count(self) -> int:
        with self._lock:
            return self._count_locked()

    def nodes(self) -> list[str]:
        seen: dict[str, None] = {}
        with self._lock:
            stack = [self.root]
            while stack:
                node = stack.pop()
                for n in node.node_residency:
                    seen[n] = None
                stack.extend(node.children.values())
        return sorted(seen)


TREE = RadixTree()


def _authorize(request: Request) -> None:
    if not SHARED_SECRET:
        return
    if request.headers.get(API_KEY_NAME) != SHARED_SECRET:
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail="invalid Sparknet authorization token",
        )


@app.get("/v1/kv/health")
async def health() -> dict:
    return {"status": "active", "prefixes": TREE.count(), "nodes": TREE.nodes()}


@app.post("/v1/kv/lookup")
async def lookup(request: Request) -> JSONResponse:
    _authorize(request)
    try:
        body = await request.json()
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=400, detail=f"invalid JSON body: {exc}") from exc
    prompt = body.get("prompt")
    if not isinstance(prompt, str) or not prompt:
        raise HTTPException(status_code=400, detail="prompt is required")
    return JSONResponse(status_code=200, content=TREE.lookup(prompt))


@app.post("/v1/kv/register")
async def register(request: Request) -> JSONResponse:
    _authorize(request)
    try:
        body = await request.json()
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=400, detail=f"invalid JSON body: {exc}") from exc
    prefix = body.get("prefix")
    node = body.get("node")
    if not isinstance(prefix, str) or not isinstance(node, str):
        raise HTTPException(status_code=400, detail="prefix and node are required")
    try:
        count = TREE.register(prefix, node)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return JSONResponse(status_code=200, content={"ok": True, "prefixes": count})


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(
        "kv_cache_router:app",
        host=os.getenv("KV_LISTEN_HOST", "127.0.0.1"),
        port=int(os.getenv("KV_LISTEN_PORT", "9100")),
        workers=int(os.getenv("KV_WORKERS", "1")),
    )
