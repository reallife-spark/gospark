package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/identity"
)

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func newIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate("node-a", identity.RolePeer)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// makeTree builds a small source tree with a nested file and a symlink.
func makeTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.md"), "alpha", 0o644)
	writeFile(t, filepath.Join(src, "sub", "b.txt"), "bravo", 0o600)
	if err := os.Symlink("a.md", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestSnapshotVerifyRestoreRoundTrip(t *testing.T) {
	id := newIdentity(t)
	src := makeTree(t)
	snap := filepath.Join(t.TempDir(), "snap")

	m, stats, err := Create(src, snap, id, Options{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if stats.Files != 2 || stats.Symlinks != 1 {
		t.Fatalf("stats = %+v, want 2 files + 1 symlink", stats)
	}
	if err := m.Verify(id.Public); err != nil {
		t.Fatalf("verify manifest: %v", err)
	}
	if err := VerifyTree(snap, m); err != nil {
		t.Fatalf("verify tree: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if err := Restore(snap, dest, m, id.Public); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "sub", "b.txt"))
	if err != nil || string(got) != "bravo" {
		t.Fatalf("restored file = %q, %v", got, err)
	}
	if target, err := os.Readlink(filepath.Join(dest, "link")); err != nil || target != "a.md" {
		t.Fatalf("restored symlink = %q, %v", target, err)
	}
}

func TestVerifyDetectsTamperedFile(t *testing.T) {
	id := newIdentity(t)
	src := makeTree(t)
	snap := filepath.Join(t.TempDir(), "snap")
	m, _, err := Create(src, snap, id, Options{})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(snap, "a.md"), "alpha-tampered", 0o644)
	if err := VerifyTree(snap, m); err == nil {
		t.Fatal("expected tamper detection, got nil")
	}
}

func TestVerifyDetectsMissingAndUntracked(t *testing.T) {
	id := newIdentity(t)
	src := makeTree(t)
	snap := filepath.Join(t.TempDir(), "snap")
	m, _, err := Create(src, snap, id, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(snap, "a.md")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTree(snap, m); err == nil {
		t.Fatal("expected missing-file detection, got nil")
	}
	// A fresh snapshot restores the missing file so only the untracked case
	// remains.
	if _, _, err := Create(src, snap, id, Options{}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(snap, "extra.md"), "surprise", 0o644)
	if err := VerifyTree(snap, m); err == nil {
		t.Fatal("expected untracked-file detection, got nil")
	}
}

func TestSignatureForgeryRejected(t *testing.T) {
	id := newIdentity(t)
	src := makeTree(t)
	snap := filepath.Join(t.TempDir(), "snap")
	m, _, err := Create(src, snap, id, Options{})
	if err != nil {
		t.Fatal(err)
	}

	// An attacker re-signs with their own key: the digest recomputes cleanly but
	// the signature does not verify against the real node's key.
	forger := newIdentity(t)
	if err := m.sign(forger); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(id.Public); err == nil {
		t.Fatal("expected forgery rejection, got nil")
	}
	if err := m.Verify(forger.Public); err != nil {
		t.Fatalf("attacker's own key should verify their manifest: %v", err)
	}
}

func TestDigestMismatchRejected(t *testing.T) {
	id := newIdentity(t)
	src := makeTree(t)
	snap := filepath.Join(t.TempDir(), "snap")
	m, _, err := Create(src, snap, id, Options{})
	if err != nil {
		t.Fatal(err)
	}
	m.Entries = append(m.Entries, Entry{Path: "injected", Size: 1, SHA256: "00"})
	if err := m.Verify(id.Public); err == nil {
		t.Fatal("expected digest mismatch, got nil")
	}
}

func TestIncrementalHardLinksUnchanged(t *testing.T) {
	id := newIdentity(t)
	src := makeTree(t)
	base := t.TempDir()

	snap1 := filepath.Join(base, SnapshotDirName(mustTime(t, "2026-01-01T00:00:00Z")))
	m1, _, err := Create(src, snap1, id, Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Change one file, leave the other untouched.
	writeFile(t, filepath.Join(src, "a.md"), "alpha-v2", 0o644)
	snap2 := filepath.Join(base, SnapshotDirName(mustTime(t, "2026-01-02T00:00:00Z")))
	m2, stats, err := Create(src, snap2, id, Options{Previous: m1, PreviousDir: snap1})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Linked == 0 {
		t.Fatalf("expected the unchanged file to be hard-linked, stats = %+v", stats)
	}
	if m2.PreviousDigest != m1.Digest {
		t.Fatalf("previous_digest = %q, want %q", m2.PreviousDigest, m1.Digest)
	}
	if err := VerifyTree(snap2, m2); err != nil {
		t.Fatalf("verify incremental tree: %v", err)
	}

	// The unchanged file shares an inode with snapshot 1 (linked, not copied).
	f1, err := os.Stat(filepath.Join(snap2, "sub", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	o1, err := os.Stat(filepath.Join(snap1, "sub", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(f1, o1) {
		t.Fatal("unchanged file was copied, not hard-linked")
	}
}

func TestLatestSnapshotPicksNewest(t *testing.T) {
	id := newIdentity(t)
	src := makeTree(t)
	base := t.TempDir()
	if _, _, ok := LatestSnapshot(base); ok {
		t.Fatal("empty base reported a snapshot")
	}
	older := filepath.Join(base, SnapshotDirName(mustTime(t, "2026-01-01T00:00:00Z")))
	if _, _, err := Create(src, older, id, Options{}); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(base, SnapshotDirName(mustTime(t, "2026-03-01T00:00:00Z")))
	mNew, _, err := Create(src, newer, id, Options{})
	if err != nil {
		t.Fatal(err)
	}
	dir, got, ok := LatestSnapshot(base)
	if !ok || dir != newer || got.Digest != mNew.Digest {
		t.Fatalf("latest = %q/%v, want %q", dir, ok, newer)
	}
}

func TestCreateRefusesVerifyOnlyIdentity(t *testing.T) {
	verifyOnly := newIdentity(t)
	verifyOnly.Private = nil
	if _, _, err := Create(makeTree(t), t.TempDir(), verifyOnly, Options{}); err == nil {
		t.Fatal("expected a verify-only identity to be refused")
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
