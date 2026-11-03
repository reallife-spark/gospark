# ADR 0015 — Memory is per-vault, and the Go store holds no authority

## Status

Accepted, 2026-11-02.

## Context

Sparknet already has three memory-shaped things: `MEMORY.md` standing state,
dated session files, and the knowledge vault. Open WebUI adds a per-user KB;
OpenCode adds a session directory. A node-wide memory would make the same vault
mean different things depending on which surface asked.

## Decision

- One memory store per resolved vault path. `vault_id` is a hash of the node id
  and the canonical absolute path; the path is held only in a `0600` catalog.
- The Go service (`cmd/sparknet-memory`) accepts only a local gateway token. It
  verifies no signature, signs no note and writes no vault page.
- The Python gateway (`shared/sparknet-memory-gateway.py`) is the only front
  door. It verifies a signed grant or an Open WebUI session, narrows to one
  vault, and then forwards.
- A web or console surface is an adapter; it never owns a second store.

## Consequences

The same vault opened in OpenCode and Open WebUI returns the same records. A
query cannot cross a vault even if the caller names another path. The Go service
can be replaced or restarted without touching authority, and the Python side
remains the one implementation that decides who may read.
