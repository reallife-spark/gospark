# Provenance of `spec/sparknet/` snapshots

These files are **read-only verbatim copies** of the source-of-truth contract
files in the `sparknet` repository. goSpark does not own or mutate them; it
snapshots them so the Go control plane can be built and tested without a
checkout of the fabric repo alongside it.

| Field | Value |
|---|---|
| Source repository | `/mnt/knowledge/sparknet` (`contracts/`) |
| Source | `a4fffb3` plus the working-tree memory contract (2026-11-02) and the working-tree skill-eval contract (2026-11-11); each snapshot is refreshed again when its change lands |
| Snapshot taken | 2026-11-11 |
| Files | `capabilities.json`, `ports.json`, `identity-schema.json`, `overlay.json`, `memory-schema.json`, `skill-eval-schema.json` |

## Rule

If a snapshot drifts from `sparknet/contracts/`, the fabric is the authority and
the snapshot is wrong. Re-copy the file and update the commit above. Go code
that encodes a vocabulary or schema (`internal/authz/capabilities.go` mirrors
`capabilities.json`; `internal/identity` mirrors `identity-schema.json`) must be
edited in the *same* change as the snapshot, never silently.

## What goSpark takes from each

- **`capabilities.json`** → `internal/authz`: the closed capability vocabulary
  (`known_capabilities`) and the holder-bears-risk set. The master's
  `implied_by_private_key` rights are mirrored as
  `authz.MasterImpliedCapabilities`.
- **`identity-schema.json`** → `internal/identity` and `internal/authz`: the
  projected identity is `(node_id, role, wireguard_public_key_b64, overlay_ipv4,
  requested_capabilities)`, the fingerprint is `sha256` over its canonical JSON
  (`sort_keys`, `separators=(",",":")`), and each registry entry is signed per
  entry over `signed_envelope_fields`. goSpark carries the node's Ed25519 public
  key in the `wireguard_public_key_b64` slot.
- **`ports.json`** → default listen addresses in `configs/*.example.yaml`
  (the control-plane gateway and data plane do not collide with reserved peer
  ports), plus the node memory service at `4110`.
- **`memory-schema.json`** → the typed-memory and delegate vocabulary; the Go
  store implements it and never treats it as authority.
- **`overlay.json`** → the WireGuard-overlay assumption behind `internal/mesh`.
- **`skill-eval-schema.json`** → `internal/delegates`: the closed verdict,
  subject-kind and evaluator-kind vocabulary and the forbidden authority
  fields. The broker validates and stores the record; the canonical digest
  stays with `shared/sparknet-skill-eval.py`, which is the consuming gate.

## Non-goal

goSpark reimplements the *control/service* plane in Go. It does not reimplement
the peer-node guard, the anchor, or the Aegis plan protocol; those stay in the
fabric. This snapshot is what keeps the two honest about each other.
