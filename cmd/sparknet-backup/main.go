// Command sparknet-backup is the goSpark storage & knowledge backup daemon: it
// takes a signed, content-addressed snapshot of a source tree and can prove a
// snapshot authentic and intact, then restore it.
//
// It is the control-plane half of the blueprint's sparknet_backup.py. Where that
// script shells out to rsync and mirrors a directory, this binary hashes every
// file, signs the manifest with the node key the same way every fabric envelope
// is signed, and re-hashes the tree on verify — so a restore can tell a faithful
// backup from a tampered one. It links no model, no CUDA and starts no external
// process.
package main

import (
	"context"
	"crypto/ed25519"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/reallife-spark/goSpark/internal/backup"
	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/identity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sparknet-backup:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sparknet-backup — goSpark signed snapshot daemon

Usage:
  sparknet-backup snapshot [-config F] [-source DIR] [-target DIR] -key FILE [-skip-dirs a,b]
  sparknet-backup run      [-config F] [-source DIR] [-target DIR] -key FILE [-interval DUR] [-skip-dirs a,b]
  sparknet-backup verify   -dir DIR [-manifest FILE] (-key FILE | -pub B64)
  sparknet-backup restore  -dir DIR [-manifest FILE] -dest DIR (-key FILE | -pub B64)
  sparknet-backup show     -dir DIR [-manifest FILE]
`)
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "snapshot":
		return cmdSnapshot(os.Args[2:])
	case "run":
		return cmdRun(os.Args[2:])
	case "verify":
		return cmdVerify(os.Args[2:])
	case "restore":
		return cmdRestore(os.Args[2:])
	case "show":
		return cmdShow(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	return nil
}

// backupSettings resolves the source, target, key, interval and skip-dirs from a
// config file, overridden by explicit flags. Interval returns 0 when unset.
func backupSettings(fs *flag.FlagSet, args []string) (source, target string, keys, skip []string, interval time.Duration, err error) {
	cfgPath := fs.String("config", "", "config file (YAML)")
	src := fs.String("source", "", "source tree to snapshot (overrides config)")
	tgt := fs.String("target", "", "target directory for snapshots (overrides config)")
	keysFlag := fs.String("key", "", "comma-separated signing key files")
	ivl := fs.Duration("interval", 0, "snapshot cadence (overrides config)")
	skipFlag := fs.String("skip-dirs", "", "directory names to prune (comma-separated)")
	if err = fs.Parse(args); err != nil {
		return
	}
	var cfg config.Config
	if cfg, err = config.Load(*cfgPath); err != nil {
		return
	}
	source = firstNonEmpty(*src, cfg.Backup.Source)
	target = firstNonEmpty(*tgt, cfg.Backup.Target)
	keys = splitCSV(firstNonEmpty(*keysFlag, cfg.Backup.KeyFile))
	skip = append(splitCSV(*skipFlag), cfg.Backup.SkipDirs...)
	interval = *ivl
	if interval == 0 {
		interval = cfg.Backup.Interval
	}
	return
}

func cmdSnapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	source, target, keys, skip, _, err := backupSettings(fs, args)
	if err != nil {
		return err
	}
	if source == "" || target == "" || len(keys) == 0 {
		return fmt.Errorf("snapshot: -source, -target and -key are required (or set them in the config)")
	}
	signer, err := identity.Load(pickSigningKey(keys))
	if err != nil {
		return err
	}
	dir, m, stats, err := takeSnapshot(source, target, signer, skip)
	if err != nil {
		return err
	}
	fmt.Printf("snapshot: %s\n", dir)
	fmt.Printf("node=%s digest=%s files=%d symlinks=%d linked=%d copied=%d bytes=%d\n",
		m.NodeID, short(m.Digest), stats.Files, stats.Symlinks, stats.Linked, stats.Copied, stats.TotalBytes)
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	source, target, keys, skip, every, err := backupSettings(fs, args)
	if err != nil {
		return err
	}
	if source == "" || target == "" || len(keys) == 0 {
		return fmt.Errorf("run: -source, -target and -key are required (or set them in the config)")
	}
	if every <= 0 {
		every = 24 * time.Hour
	}
	signer, err := identity.Load(pickSigningKey(keys))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "sparknet-backup: snapshotting %s -> %s every %s (Ctrl-C to stop)\n", source, target, every)
	for {
		dir, m, stats, err := takeSnapshot(source, target, signer, skip)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sparknet-backup: snapshot failed: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "sparknet-backup: snapshot %s digest=%s files=%d linked=%d copied=%d bytes=%d\n",
				dir, short(m.Digest), stats.Files, stats.Linked, stats.Copied, stats.TotalBytes)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}

// takeSnapshot creates one timestamped, incremental snapshot and verifies it
// immediately, so a corrupt write is reported now rather than at restore time.
func takeSnapshot(source, target string, signer *identity.Identity, skip []string) (string, *backup.Manifest, backup.Stats, error) {
	opts := backup.Options{SkipDirs: skip}
	if prevDir, prev, ok := backup.LatestSnapshot(target); ok {
		opts.Previous, opts.PreviousDir = prev, prevDir
	}
	dir := filepath.Join(target, backup.SnapshotDirName(time.Now()))
	m, stats, err := backup.Create(source, dir, signer, opts)
	if err != nil {
		return dir, nil, stats, err
	}
	if err := backup.VerifyTree(dir, m); err != nil {
		return dir, nil, stats, fmt.Errorf("freshly written snapshot does not verify: %w", err)
	}
	return dir, m, stats, nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", "", "snapshot directory")
	manifest := fs.String("manifest", "", "manifest file (default <dir>/"+backup.ManifestName+")")
	pub := fs.String("pub", "", "signing public key, base64")
	key := fs.String("key", "", "signing key file (alternative to -pub)")
	_ = fs.Parse(args)
	if *dir == "" {
		return fmt.Errorf("verify: -dir is required")
	}
	pubKey, err := resolvePub(*pub, *key)
	if err != nil {
		return err
	}
	m, err := loadManifest(*dir, *manifest)
	if err != nil {
		return err
	}
	if err := m.Verify(pubKey); err != nil {
		return err
	}
	if err := backup.VerifyTree(*dir, m); err != nil {
		return err
	}
	fmt.Printf("OK node=%s digest=%s files=%d bytes=%d created=%s\n",
		m.NodeID, short(m.Digest), len(m.Entries), m.TotalBytes, m.CreatedAt)
	return nil
}

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	dir := fs.String("dir", "", "snapshot directory")
	manifest := fs.String("manifest", "", "manifest file (default <dir>/"+backup.ManifestName+")")
	dest := fs.String("dest", "", "restore destination directory")
	pub := fs.String("pub", "", "signing public key, base64")
	key := fs.String("key", "", "signing key file (alternative to -pub)")
	_ = fs.Parse(args)
	if *dir == "" || *dest == "" {
		return fmt.Errorf("restore: -dir and -dest are required")
	}
	pubKey, err := resolvePub(*pub, *key)
	if err != nil {
		return err
	}
	m, err := loadManifest(*dir, *manifest)
	if err != nil {
		return err
	}
	if err := backup.Restore(*dir, *dest, m, pubKey); err != nil {
		return err
	}
	fmt.Printf("restored %d entries to %s (node=%s digest=%s)\n", len(m.Entries), *dest, m.NodeID, short(m.Digest))
	return nil
}

func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	dir := fs.String("dir", "", "snapshot directory")
	manifest := fs.String("manifest", "", "manifest file (default <dir>/"+backup.ManifestName+")")
	_ = fs.Parse(args)
	if *dir == "" && *manifest == "" {
		return fmt.Errorf("show: -dir or -manifest is required")
	}
	m, err := loadManifest(*dir, *manifest)
	if err != nil {
		return err
	}
	fmt.Printf("node=%s source=%s created=%s\n", m.NodeID, m.Source, m.CreatedAt)
	fmt.Printf("digest=%s previous=%s entries=%d bytes=%d\n",
		short(m.Digest), short(m.PreviousDigest), len(m.Entries), m.TotalBytes)
	for _, e := range m.Entries {
		kind := "file"
		if e.Link != "" {
			kind = "link -> " + e.Link
		}
		fmt.Printf("  %s\t%d\t%s\t%s\n", e.SHA256[:12], e.Size, kind, e.Path)
	}
	return nil
}

func loadManifest(dir, manifest string) (*backup.Manifest, error) {
	path := manifest
	if path == "" {
		if dir == "" {
			return nil, fmt.Errorf("no manifest file or snapshot directory given")
		}
		path = filepath.Join(dir, backup.ManifestName)
	}
	return backup.Load(path)
}

func resolvePub(pubB64, keyFile string) (ed25519.PublicKey, error) {
	switch {
	case pubB64 != "":
		return identity.ParsePublicKeyB64(pubB64)
	case keyFile != "":
		id, err := identity.Load(keyFile)
		if err != nil {
			return nil, err
		}
		return id.Public, nil
	default:
		return nil, fmt.Errorf("need -pub or -key to verify the manifest signature")
	}
}

// pickSigningKey returns the key file that should sign; the first one listed.
func pickSigningKey(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
