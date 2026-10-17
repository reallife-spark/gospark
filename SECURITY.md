# Security policy

## Reporting a vulnerability

This repository is **private** and operated by a single maintainer. Do not open
a public issue for a security problem. Instead, either:

- open a **private vulnerability report** on the repository
  (*Security → Advisories → Report a vulnerability*), or
- contact the maintainer directly through the host.

Include the affected path or command, what you observed, and the smallest
reproduction you have. There is no bug-bounty programme.

## Supported versions

The current `master` branch is supported. There is no tagged release yet;
fixes land on the branch and are not backported.

## What matters most here

goSpark implements the fabric's **fail-closed authority model** in Go, so a
report that bends one of these invariants is the highest-priority kind:

- **Verification fails closed and in order.** An agent verifies an envelope's
  signature, then liveness, then the required capability, then the replay
  nonce — and refuses on the first failure. "Unknown, so allow" is a bug.
- **Authority is key possession, not a name.** The signer signs; the verifier
  verifies. Holding a capability string without the key grants nothing. The
  trust anchor is the pinned `-master-pub`, never a key embedded in a document
  you were handed.
- **The admin surface grants full registry authority**, so it binds
  loopback-only and is disabled unless `admin.listen` is set. A finding that
  reaches it off-loopback, or reaches it without the configured auth, matters.
- **The boundary holds.** A core (pure-Go) binary must never need a GPU, a
  model, or a network call to the compute plane to *build* or to make an
  authorization decision.
- **The registry ships empty.** No node has privileges until the Master signs
  an entry.

## Secrets

- Nothing secret is tracked: no private keys, tokens, PEM blocks or `.env`
  contents — and no plausible-looking placeholders in docs. Reference the
  secret *path* only.
- `.state/` (keys, registry snapshots, delta caches) is gitignored, and
  `make keys-check` refuses a tracked key-shaped file.
- If a secret is committed, **rotate it first** — removing the file does not
  remove it from history — then report it as above. A key pasted into a chat
  is treated as exposed even if it was never committed.
- The GPU sidecar and the compute-plane worker read their configuration from
  `compute/*.env` files that are gitignored (the `.example` files are not
  secrets and are committed).

## Scope

In scope: goSpark's own code and its snapshots of the fabric contract
(`spec/sparknet`), the on-wire schema (`proto/sparknet/v1`), the CLI, the
gateway and the transport. Out of scope: vulnerabilities in third-party Go
modules or NVIDIA libraries, which belong upstream — pin reports to the
version we require.
