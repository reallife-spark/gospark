## What changed

<!-- One logical change. Say what and why; link the ADR if the design was not
     obvious. -->

## How it was verified

- [ ] `make ci` (fmt-check + vet + test-race)
- [ ] `make test-nats` when the bus or an integration path changed
- [ ] `make build-gpu` when `cmd/sparknet-gpu` or a GPU seam changed

<!-- Name any gate that skipped and why (e.g. no nats-server on this host). -->

## The boundary and the authority model

- [ ] No pure-Go core package gained a dependency on a GPU, a model, or the
      compute plane (the core still builds with `CGO_ENABLED=0`).
- [ ] No path was added that trusts a capability name over a signature; if
      verification changed, it still fails closed and in order.
- [ ] Telemetry still grants nothing.
- [ ] A non-obvious design decision is written up as an ADR in `docs/adr/`.

## Could not verify

<!-- A DoD that needs real hardware (a GPU, a driver) is a first-class answer
     here, not a failure. Name what is unverified and why. -->

## Checklist

- [ ] `make keys-check` passes; no key material or plausible placeholder is
      tracked (see `SECURITY.md`).
- [ ] New commands in the README were run at least once.
- [ ] Conventions follow the fabric's `STYLE_GUIDE.md` (sibling `sparknet`
      repository).
