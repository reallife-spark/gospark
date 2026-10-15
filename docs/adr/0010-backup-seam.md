# ADR 0010 — Backup seam (signed, content-addressed snapshots)

- **Status:** accepted
- **Date:** 2026-10-15
- **Context:** The blueprint's §3.11 `sparknet_backup.py` is a host-plane rsync
  wrapper: it builds a `subprocess.run(..., shell=True)` command from
  environment variables and mirrors a source directory into a fresh,
  timestamped target where `--delete` is a no-op. It is EU AI Act Art. 12
  material (record-keeping / disaster recovery), but as written it produces a
  copy that cannot prove it is the copy that was taken — a corrupted or
  substituted backup looks identical to a good one.

  goSpark's slice is the control/service plane. Backup is not compute-plane (no
  GPU, no model), and the fabric already treats its signed registry and ledger
  as the durable core, so this is goSpark's work: the half that makes a backup
  *trustworthy*.

## Decision

Implement the backup daemon as a signed, content-addressed snapshot in pure Go
(`internal/backup` + `cmd/sparknet-backup`), and do not shell out.

- **Content-addressed.** Every file is SHA-256 hashed (symlinks by target). The
  manifest lists each entry's path, size, mode and hash, plus the total; entries
  are sorted so the canonical form is deterministic.
- **Signed like every other fabric envelope.** The manifest's signable body is
  canonicalised with `apiv1.CanonicalJSON` (sorted keys, tight separators) and
  Ed25519-signed with the node key via `internal/identity`; the hex SHA-256 of
  the same bytes is the manifest `digest`, which content-addresses the snapshot
  and becomes the next run's `previous_digest`.
- **Verify is two checks, not one.** `Manifest.Verify(pub)` proves the manifest
  itself (digest recomputes, signature valid against a key the caller trusts —
  never blindly against the embedded one); `VerifyTree` re-hashes the tree and
  fails on a changed file, a missing file, or an added one. A restore runs both
  before writing anything, so a tampered backup is refused rather than restored.
- **Incremental without a copy.** A file whose hash matches the same path in the
  previous snapshot is hard-linked from the previous snapshot directory instead
  of recopied; if the link fails (cross-device), it falls back to a copy. The
  daemon verifies each snapshot immediately after writing it, so a corrupt write
  is reported at backup time, not at restore time.
- **No shell, no delete.** The command is built in-process; nothing is passed to
  a shell and no `--delete` semantics exist. A source file that changes mid-run
  is caught by the immediate verify.
- **Config-integrated.** `backup.source` / `target` / `key_file` / `interval` /
  `skip_dirs` (plus `GOSPARK_BACKUP_*`), loaded and validated like every other
  component. `sparknet-backup run` loops; `snapshot`, `verify`, `restore` and
  `show` are the operator actions.

## Consequences

- One new pure-Go package, one new binary, one config block. `make build` still
  builds everything with `CGO_ENABLED=0`.
- A backup now has provenance: it is authentic (signature), intact (re-hash) and
  chained (previous_digest links successive snapshots).
- Hard-linking shares inodes between a snapshot and its predecessor, so snapshots
  must be treated as immutable once written — the standard `rsync --link-dest`
  caveat. The directory name carries the timestamp, so this is the expected use.
- Empty directories and non-regular, non-symlink entries (sockets, devices,
  FIFOs) are not captured; the snapshot reports how many were skipped.
