# 0018 — Understand graph: Python producer, Go consumer

## Status

Accepted (fabric Step 92A). Supersedes
`gospark/docs/adr/0017-understand-graph-service.md`.

## Context

ADR-0017 gave Go a scan, diff and service plane and claimed a Go-only operator
could use them. The Go graph binary is not part of `gospark/Makefile build`, no
role bundle carries it, the root pre-merge gate does not run
`gospark/internal/understand`, and `goagx/bootstrap-agx.sh` checks only the four
long-standing Go binaries. In practice the claimed surface is not shipped.

Keeping scan and diff in both Python and Go duplicates the scan policy, the diff
algebra and the schema projection. The duplicates already differ: Python and Go
have separate exclude precedence, language and test-path tables, and typed Go
graph structs can drop optional contract fields such as `languageLesson`.

Python is in every node bundle. The graph is a build artifact, not authority and
not a knowledge channel.

## Decision

- **Python is the one graph producer.** `shared/sparknet_understand/` owns
  `scan`, `build`, `merge`, `normalize`, `validate` and `diff`.
- **The graph seam is the path-stable
  `shared/sparknet-understand.py` JSON CLI.** Go calls that path; it does not
  import an installed Python package and does not add a long-lived worker.
- **Go is a graph consumer.** `gospark/internal/understand` keeps `query`,
  `export` and the stdio MCP surface over an existing graph.
- **Go CLI commands are adapters.** `scan`, `diff` and `parse` call the Python
  seam. `gofrag` keeps the native `go/parser` extractor but consumes Python
  `FileRecords`; it does not run a Go scan or own scan policy.
- **Python owns scan policy.** Default excludes, `.understandignore`, language
  and test-path tables, binary detection and the file-size cap have one
  implementation.
- **Go preserves graph fidelity.** Loading keeps the raw graph for JSON export,
  typed structs are a query projection, and `Diff` is Python's result. Unknown
  or optional fields must not be dropped.
- **Failure is explicit.** `scan`, `build`, `diff` and `gofrag` fail closed when
  Python is unavailable or returns invalid output. `query`, `export` and `mcp`
  still work on an existing graph. There is no Go fallback that silently
  recreates the old producer.
- **The consumer ships deliberately.** Build `sparknet-understand` in
  `gospark/Makefile build`, include it in goagx, and add a root
  `understand-check`. Role bundles include it only when a role surface invokes
  the Go CLI; the console continues to read `.ua/` directly.
- **Parity is one test surface.** A checked-in cross-language fixture runs
  Python `scan` and `diff` and the Go adapters, then compares normalized JSON.
  Separate Python and Go fixtures are not the parity proof.

## Consequences

- A Go-only operator can query, export and run MCP over an existing graph, but
  cannot produce a graph without Python. ADR-0017's broader production claim is
  dropped.
- The duplicate Go scan and diff tables are deleted; `gofrag` becomes a native
  extractor over Python's file records.
- The graph schema gains a raw-preserving consumer path; future optional fields
  do not require a Go type change to survive export.
- The Go graph binary becomes an intentional build and goagx artifact instead of
  an unshipped command.
- A future desire to produce graphs without Python requires a new ADR, not a
  quiet second scan or diff implementation.
