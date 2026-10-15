// Package backup implements goSpark's storage & knowledge backup seam: a
// signed, content-addressed snapshot of a directory tree that can later be
// proven authentic and intact, and restored, without shelling out to rsync.
//
// The blueprint's sparknet_backup.py is a host-plane rsync wrapper (a
// `shell=True` string built from environment variables, mirroring into a fresh
// timestamped directory where `--delete` is a no-op). goSpark's slice is the
// control-plane half that makes a backup trustworthy: every file is
// content-hashed, the manifest is canonicalised and Ed25519-signed exactly as
// every other fabric envelope is, and Verify re-hashes the tree so a restore
// can prove the snapshot was not altered. Incremental runs hard-link files that
// are byte-for-byte unchanged since the previous snapshot instead of recopying
// them.
//
// It links no model, no CUDA and no external process: it is pure Go I/O,
// consistent with the rule that Go runs the control/service plane.
package backup

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/identity"
)

// ManifestName is the file the signed manifest is stored under, inside the
// snapshot directory it describes.
const ManifestName = "sparknet-backup-manifest.json"

// ManifestVersion is the current on-disk schema version.
const ManifestVersion = 1

// Entry describes one file or symlink in a snapshot. Path is slash-separated
// and relative to the snapshot root, so a manifest is portable across hosts.
type Entry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`   // permission bits
	SHA256 string `json:"sha256"` // hex content hash (symlink: hash of the target)
	// Link is the symlink target when this entry is a symlink. Empty for a
	// regular file.
	Link string `json:"link,omitempty"`
}

// Manifest is the signed record of one snapshot. The signature and digest cover
// every field except Digest, PublicKeyB64 and SignatureB64 themselves.
type Manifest struct {
	Version        int     `json:"version"`
	NodeID         string  `json:"node_id"`
	Source         string  `json:"source"`
	CreatedAt      string  `json:"created_at"` // RFC3339 UTC
	PreviousDigest string  `json:"previous_digest,omitempty"`
	Entries        []Entry `json:"entries"`
	TotalBytes     int64   `json:"total_bytes"`
	// Digest is the hex sha256 of the canonical signable body; it content-
	// addresses the snapshot and is also recorded as the next run's
	// PreviousDigest, forming an incremental chain.
	Digest string `json:"digest"`
	// PublicKeyB64 identifies the signing key; Verify still checks against a key
	// the caller trusts, never blindly against this field.
	PublicKeyB64 string `json:"public_key_b64"`
	SignatureB64 string `json:"signature_b64"`
}

// signedBody mirrors the signed fields. It exists so the signer and verifier
// canonicalise exactly the same structure and can never drift.
type signedBody struct {
	Version        int     `json:"version"`
	NodeID         string  `json:"node_id"`
	Source         string  `json:"source"`
	CreatedAt      string  `json:"created_at"`
	PreviousDigest string  `json:"previous_digest,omitempty"`
	Entries        []Entry `json:"entries"`
	TotalBytes     int64   `json:"total_bytes"`
}

func (m *Manifest) signable() signedBody {
	return signedBody{
		Version:        m.Version,
		NodeID:         m.NodeID,
		Source:         m.Source,
		CreatedAt:      m.CreatedAt,
		PreviousDigest: m.PreviousDigest,
		Entries:        m.Entries,
		TotalBytes:     m.TotalBytes,
	}
}

// Stats reports what a Create run did. It is operational metadata, not part of
// the signed manifest.
type Stats struct {
	Files       int   // regular files captured
	Symlinks    int   // symlinks captured
	Skipped     int   // non-regular, non-symlink entries ignored
	Copied      int   // files written fresh
	Linked      int   // unchanged files hard-linked from the previous snapshot
	CopiedBytes int64 // bytes physically written
	TotalBytes  int64 // bytes represented by the snapshot (all files)
}

// Options configures a snapshot.
type Options struct {
	// Previous, with PreviousDir, enables incremental snapshots: a file whose
	// content hash matches the same path in Previous is hard-linked from
	// PreviousDir rather than copied. If the link fails it falls back to a copy.
	Previous    *Manifest
	PreviousDir string
	// SkipDirs prunes directories by name (e.g. ".git"). Empty captures
	// everything under the source.
	SkipDirs []string
}

// Create hashes src, copies it into targetDir, and writes a signed manifest at
// targetDir/ManifestName. The identity must hold a private key. It is safe to
// call with a targetDir that does not yet exist.
func Create(src, targetDir string, id *identity.Identity, opts Options) (*Manifest, Stats, error) {
	if id == nil || id.Private == nil {
		return nil, Stats{}, errors.New("backup: a signing identity with a private key is required")
	}
	src = filepath.Clean(src)
	info, err := os.Stat(src)
	if err != nil {
		return nil, Stats{}, fmt.Errorf("backup: source: %w", err)
	}
	if !info.IsDir() {
		return nil, Stats{}, fmt.Errorf("backup: source %s is not a directory", src)
	}

	skip := make(map[string]struct{}, len(opts.SkipDirs))
	for _, d := range opts.SkipDirs {
		skip[d] = struct{}{}
	}
	prev := make(map[string]Entry, len(entryList(opts.Previous)))
	for _, e := range entryList(opts.Previous) {
		prev[e.Path] = e
	}

	var stats Stats
	var entries []Entry
	walkErr := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("backup: walk %s: %w", path, err)
		}
		if d.IsDir() {
			if _, bad := skip[d.Name()]; bad && path != src {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("backup: rel %s: %w", path, err)
		}
		rel = filepath.ToSlash(rel)
		fi, err := d.Info()
		if err != nil {
			return fmt.Errorf("backup: stat %s: %w", path, err)
		}

		entry, err := describe(path, rel, fi)
		if err != nil {
			return err
		}
		if entry == nil { // not a file or symlink (socket, device, fifo)
			stats.Skipped++
			return nil
		}
		entries = append(entries, *entry)
		stats.TotalBytes += entry.Size
		if entry.Link != "" {
			stats.Symlinks++
		} else {
			stats.Files++
		}

		if err := materialise(path, targetDir, *entry, prev, opts.PreviousDir, &stats); err != nil {
			return err
		}
		return nil
	})
	if walkErr != nil {
		return nil, stats, walkErr
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	m := &Manifest{
		Version:    ManifestVersion,
		NodeID:     id.NodeID,
		Source:     src,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		Entries:    entries,
		TotalBytes: stats.TotalBytes,
	}
	if opts.Previous != nil {
		m.PreviousDigest = opts.Previous.Digest
	}
	if err := m.sign(id); err != nil {
		return nil, stats, err
	}
	if err := m.Save(filepath.Join(targetDir, ManifestName)); err != nil {
		return nil, stats, err
	}
	return m, stats, nil
}

func entryList(m *Manifest) []Entry {
	if m == nil {
		return nil
	}
	return m.Entries
}

// describe hashes a filesystem entry into an Entry. It returns (nil, nil) for
// entries that are neither regular files nor symlinks.
func describe(path, rel string, fi fs.FileInfo) (*Entry, error) {
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return nil, fmt.Errorf("backup: readlink %s: %w", path, err)
		}
		sum := sha256.Sum256([]byte(target))
		return &Entry{
			Path:   rel,
			Size:   int64(len(target)),
			Mode:   uint32(fi.Mode().Perm()),
			SHA256: hex.EncodeToString(sum[:]),
			Link:   target,
		}, nil
	case fi.Mode().IsRegular():
		size, digest, err := hashFile(path)
		if err != nil {
			return nil, err
		}
		return &Entry{
			Path:   rel,
			Size:   size,
			Mode:   uint32(fi.Mode().Perm()),
			SHA256: digest,
		}, nil
	default:
		return nil, nil
	}
}

// materialise writes one entry into the snapshot, hard-linking from the
// previous snapshot when the content is unchanged.
func materialise(path, targetDir string, e Entry, prev map[string]Entry, prevDir string, stats *Stats) error {
	dst := filepath.Join(targetDir, filepath.FromSlash(e.Path))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("backup: mkdir %s: %w", filepath.Dir(dst), err)
	}

	if e.Link != "" {
		_ = os.Remove(dst)
		if err := os.Symlink(e.Link, dst); err != nil {
			return fmt.Errorf("backup: symlink %s: %w", dst, err)
		}
		stats.Copied++
		return nil
	}

	if p, ok := prev[e.Path]; ok && p.Link == "" && p.SHA256 == e.SHA256 && prevDir != "" {
		from := filepath.Join(prevDir, filepath.FromSlash(e.Path))
		if err := os.Link(from, dst); err == nil {
			stats.Linked++
			return nil
		}
		// Fall through to a copy when the previous file is gone or on another
		// filesystem (hard links cannot cross devices).
	}

	if err := copyFile(path, dst, e.Mode); err != nil {
		return err
	}
	stats.Copied++
	stats.CopiedBytes += e.Size
	return nil
}

// Verify checks the manifest's own digest and signature against pub.
func (m *Manifest) Verify(pub ed25519.PublicKey) error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("backup: unsupported manifest version %d (want %d)", m.Version, ManifestVersion)
	}
	canon, err := apiv1.CanonicalJSON(m.signable())
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canon)
	if got := hex.EncodeToString(sum[:]); got != m.Digest {
		return fmt.Errorf("backup: manifest digest mismatch (recorded %s, computed %s)", m.Digest, got)
	}
	sig, err := identity.DecodeSignature(m.SignatureB64)
	if err != nil {
		return err
	}
	if !identity.Verify(pub, canon, sig) {
		return errors.New("backup: manifest signature does not verify against the given key")
	}
	return nil
}

// VerifyTree proves a snapshot directory matches its (already-trusted) manifest:
// every recorded file re-hashes to its recorded digest, nothing is missing, and
// nothing has been added. The manifest file itself is expected and ignored.
func VerifyTree(dir string, m *Manifest) error {
	want := make(map[string]Entry, len(m.Entries))
	for _, e := range m.Entries {
		want[e.Path] = e
	}
	var problems []string
	for _, e := range m.Entries {
		p := filepath.Join(dir, filepath.FromSlash(e.Path))
		if e.Link != "" {
			target, err := os.Readlink(p)
			if err != nil {
				problems = append(problems, "missing symlink "+e.Path)
			} else if target != e.Link {
				problems = append(problems, "symlink target changed "+e.Path)
			}
			continue
		}
		size, digest, err := hashFile(p)
		if err != nil {
			problems = append(problems, "missing or unreadable "+e.Path)
			continue
		}
		if digest != e.SHA256 {
			problems = append(problems, "content changed "+e.Path)
		} else if size != e.Size {
			problems = append(problems, "size changed "+e.Path)
		}
	}
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName {
			return nil
		}
		if _, ok := want[rel]; !ok {
			problems = append(problems, "untracked file "+rel)
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if len(problems) > 0 {
		return fmt.Errorf("backup: snapshot does not verify: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Restore verifies the manifest and tree against pub and copies the snapshot
// into dest, recreating symlinks.
func Restore(dir, dest string, m *Manifest, pub ed25519.PublicKey) error {
	if err := m.Verify(pub); err != nil {
		return err
	}
	if err := VerifyTree(dir, m); err != nil {
		return err
	}
	for _, e := range m.Entries {
		src := filepath.Join(dir, filepath.FromSlash(e.Path))
		dst := filepath.Join(dest, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("backup: restore mkdir %s: %w", filepath.Dir(dst), err)
		}
		if e.Link != "" {
			_ = os.Remove(dst)
			if err := os.Symlink(e.Link, dst); err != nil {
				return fmt.Errorf("backup: restore symlink %s: %w", dst, err)
			}
			continue
		}
		if err := copyFile(src, dst, e.Mode); err != nil {
			return err
		}
	}
	return nil
}

// Load reads a manifest from disk.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("backup: read %s: %w", path, err)
	}
	m := &Manifest{}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("backup: parse %s: %w", path, err)
	}
	return m, nil
}

// Save writes m as indented JSON.
func (m *Manifest) Save(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: marshal manifest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("backup: mkdir: %w", err)
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// LatestSnapshot returns the most recent snapshot directory under base and its
// manifest. ok is false when base holds no snapshot yet, so a first run can
// start a full backup without treating "nothing yet" as an error.
func LatestSnapshot(base string) (dir string, m *Manifest, ok bool) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", nil, false
	}
	var newest time.Time
	var newestName string
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		cand := filepath.Join(base, de.Name(), ManifestName)
		loaded, err := Load(cand)
		if err != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, loaded.CreatedAt)
		if err != nil {
			continue
		}
		// Order by creation time, breaking ties on the directory name so two
		// snapshots created in the same second still resolve deterministically.
		if ok && (ts.Before(newest) || (ts.Equal(newest) && de.Name() <= newestName)) {
			continue
		}
		newest, newestName = ts, de.Name()
		dir, m, ok = filepath.Join(base, de.Name()), loaded, true
	}
	return dir, m, ok
}

// SnapshotDirName is the timestamped directory name Create's callers should use
// under a backup target, e.g. snap_20261015_030000.
func SnapshotDirName(t time.Time) string {
	return "snap_" + t.UTC().Format("20060102_150405")
}

func (m *Manifest) sign(id *identity.Identity) error {
	canon, err := apiv1.CanonicalJSON(m.signable())
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canon)
	m.Digest = hex.EncodeToString(sum[:])
	sig, err := id.Sign(canon)
	if err != nil {
		return err
	}
	m.PublicKeyB64 = id.PublicKeyB64()
	m.SignatureB64 = identity.EncodeSignature(sig)
	return nil
}

func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("backup: open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", fmt.Errorf("backup: hash %s: %w", path, err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string, mode uint32) error {
	perm := os.FileMode(mode) & 0o777
	if perm == 0 {
		perm = 0o644
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("backup: open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("backup: create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("backup: copy %s -> %s: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("backup: close %s: %w", dst, err)
	}
	return os.Chmod(dst, perm)
}
