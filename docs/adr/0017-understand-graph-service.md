# 0017 — Understand-Anything graph: Python parser, Go service

## Status

Accepted (fabric Step 91).

## Context

The fabric is integrating Understand-Anything (UA), a TypeScript tool that
produces a `knowledge-graph.json` from a repository. The graph is a derived
build artifact: it is gitignored, never authority and never an answer corpus.
The node console needs to render it, the `/understand*` skills need to build and
query it, and the Go control plane needs a stable service boundary.

ADR 0001 fixes the split: Go runs the control/service plane; Python/CUDA runs the
compute/parse plane. Reimplementing UA's TypeScript core in Go would either
duplicate a parser or pull tree-sitter into the pure-Go core through cgo, which
the boundary forbids.

## Decision

- **Python owns parsing and merging.** `shared/sparknet-understand.py` and
  `shared/sparknet_understand/` implement the schema, native extractors,
  optional tree-sitter fallback, graph merge and validation. The Python core is
  the one parser and the one merger.
- **Go owns the service plane.** `internal/understand` walks, fingerprints,
  stores, queries, diffs, exports and serves the graph over a stdio MCP server.
  It never writes authority and opens no network port. The node console reads
  `.ua/` directly through its existing loopback surface.
- **`go/parser` is the native Go extractor.** `GoFragments` parses Go files with
  the standard library and emits the same fragment shape. It is available as
  `sparknet-understand gofrag` and is the native-first path when the service is
  asked to contribute Go fragments; the Python merger still owns the final
  graph, so no graph-building rule is duplicated.
- **No new port.** The graph is a file plus a console route. A new listener would
  be a new attack surface with no authority to justify it.
- **No cgo.** The pure-Go core keeps `CGO_ENABLED=0`. The optional tree-sitter
  dependency lives only in the Python extra.

## Consequences

- A node that wants the graph needs Python, which every node already has.
- A Go-only operator can still scan, query, diff, export and run the MCP server
  over an existing graph.
- The graph and the issue report are separate artifacts: the graph is a build
  artifact, the issue is operational work. Neither is a knowledge channel.
- A future full-Go extractor would need a pure-Go parser or an explicit
  cgo-gated sidecar; that is a new ADR, not a quiet change here.
