# ADR 0002 — Realign identity and the node registry to the fabric byte-for-byte

- **Status:** accepted
- **Date:** 2026-10-14
- **Context:** The first registry implementation signed a whole
  `NodeRegistry` document and stored identity flatly (`node_id`, `owner_id`,
  `public_key_b64`) with unix timestamps. That was goSpark's own scheme, not the
  fabric's. `contracts/identity-schema.json` (reference implementation
  `shared/sparknet-authz.py`) defines something different, so the two could not
  validate each other's artifacts.

## Decision

Realign goSpark's identity and registry to the fabric exactly, rather than
keeping a documented divergence or adding a translation layer.

- **Identity projection.** The fingerprinted fields are `node_id`, `role`,
  `wireguard_public_key_b64`, `overlay_ipv4`, `requested_capabilities`,
  canonicalised as JSON `sort_keys=true, separators=(",",":")` and hashed with
  SHA-256. `role` replaces the former `owner_id`.
- **Per-entry signature.** Each authorization entry carries its own
  `signature_b64` over the fabric's `signed_envelope_fields` (`fingerprint`,
  `identity`, sorted `capabilities`, `overlay_ipv4`, `issued_at`, `expires_at`,
  `revoked`). The registry document itself is unsigned, exactly as the fabric
  ships `manifests/node-authorizations.json`.
- **ISO-8601 timestamps.** `issued_at`/`expires_at` are RFC3339 strings, not
  unix seconds. The rollback guard is the newest entry stamp
  (`Registry.LatestIssued()`), since there is no document-level issue time.
- **The node key occupies `wireguard_public_key_b64`.** goSpark's node Ed25519
  public key is placed in the slot the contract anchors identity to, so the
  fingerprint binds the node to the key its results are verified against. This
  is a deliberate, documented goSpark population of a fabric field.

## Why not keep the native scheme

A local whole-document signature is simpler, but it makes goSpark artifacts
unverifiable by the fabric and vice versa — the one property the contract
exists to guarantee. "Two implementations of one contract" only means something
if the bytes agree. The realignment is bounded: identity, the registry, and the
CLI/config fields that name them.

## Consequences

1. **Bidirectional verifiability.** A registry minted by `sparknetctl` verifies
   under `shared/sparknet-authz.py` and vice-versa; the fingerprints agree. This
   was checked against the fabric's Python reference (see the session record).
2. **No document-level monotonicity.** Rollback protection now comes from the
   per-entry `issued_at`; `ReSignRegistry` clears entry `issued_at` so a
   distribute-registry change always stamps fresh.
3. **Config/CLI surface moved.** `owner_id` → `role`, `overlay_address` →
   `overlay_ipv4`; `GOSPARK_OWNER_ID`/`GOSPARK_OVERLAY_ADDRESS` →
   `GOSPARK_ROLE`/`GOSPARK_OVERLAY_IPV4`; `keygen` takes `-role -overlay -caps`;
   `registry sign` takes `-ttl`/`-issued-at` (RFC3339).
4. **The contract snapshot is intact.** `spec/sparknet/` already matched the
   fabric at commit `59ed8c25`; only goSpark's mirror was wrong, so no snapshot
   moved — `PROVENANCE.md`'s identity description was corrected to match the
   file it describes.

## Not decided here

- Generating `internal/apiv1` from `proto/sparknet/v1/*.proto` (still
  follow-up; the hand-written structs stay byte-compatible).
- The channel by which a signed registry reaches peers (the fabric marks this
  `UNSPECIFIED`; peers read a local copy, the bus carries the snapshot).
- Non-ASCII identity strings: canonical JSON agrees with Python only for ASCII
  input (Python escapes non-ASCII, Go does not). The signed vocabulary is ASCII
  by construction; see the note in `internal/apiv1/canonical.go`.
