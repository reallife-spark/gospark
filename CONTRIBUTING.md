# Contributing to goSpark

goSpark is the Go **control and service plane** for SparkNet — identity,
capability verification, task dispatch, routing, the API gateway and the NVFP4
KV-block data plane. It is a sibling to the `sparknet` fabric, not a
replacement: **Go runs the control/service plane; Python/CUDA runs the compute
plane.** Keeping them in separate processes is deliberate (ADR 0001).

Read `README.md` for what each package is, `docs/architecture.md` for how the
pieces fit, and `docs/adr/` for the decisions that are already settled.

## Build and test

```sh
make build          # the pure-Go binaries (CGO_ENABLED=0) into bin/
make build-gpu      # the cgo GPU sidecar (links NVML/DCGM) — separate on purpose
make test           # all tests
make test-race      # tests under the race detector
make test-nats      # NATS/JetStream integration tests (start a server first)
make nats-dev       # throwaway JetStream server on 127.0.0.1:4222
make lint           # build + vet + fmt-check
make ci             # fmt-check + vet + test-race — the gate
make keys-check     # refuse if a key-shaped file is tracked
```

Requires Go 1.27+. `make ci` is the gate; a change is ready when it passes and
you can say how you ran it. `make test-nats` skips itself when `nats-server` is
absent — that skip is visible on purpose, not silent.

## The rules that are not style

- **The boundary is load-bearing.** Nothing in the pure-Go core may require a
  GPU or a model; the only cgo binary is `cmd/sparknet-gpu`, and it must
  *build* on a driverless host and *degrade at runtime* to
  `available=false` with a reason. If a change would make a core package
  depend on NVML/DCGM/CUDA, it belongs behind a seam (`internal/gputel`,
  `internal/compute`, `internal/embed`) — not in the core.
- **Authority comes from key possession.** The signer signs; the verifier
  verifies and refuses on the first failure (signature → liveness →
  capability → replay nonce, in that order). A capability name without the
  key grants nothing. Do not add a path that trusts a name over a signature.
- **Telemetry is never authority.** A health report grants nothing.
- **No secrets, ever.** Keys live under `.state/` (gitignored) and never in a
  tracked file — not even a plausible placeholder. `make keys-check` enforces
  it; see `SECURITY.md`.

## Design changes

A decision that is not obvious from the code gets an **ADR** in `docs/adr/`,
numbered and written as a decision with its consequences — not as a summary of
the commit. `docs/ROADMAP.md` records what is next.

## Style

Go: `gofmt` is the formatter and `go vet` is the gate (`make ci` runs both).
The fabric's standing convention page — the sibling `sparknet` repository's
`STYLE_GUIDE.md` — governs commits, shell, Makefiles, secrets, CI and release
in both repositories; the Go-specific rules are the two above plus `gofmt`.

Resource-first applies here too: before building a container, model or service
of our own, check NVIDIA's catalog (in the sparknet fabric,
`make nv-search q="…"`) and record the verdict.

## Commits

Conventional commits — `feat|fix|docs|style|refactor|perf|test|build|ci|chore`
— one logical change per commit. A Developer Certificate of Origin is **not**
required today (private, single-operator repository).

## Pull requests

Use the template. In short: one logical change, the gates you actually ran
(naming any that skipped and why), and — for anything touching the boundary or
the authority model — the ADR that justifies it.
