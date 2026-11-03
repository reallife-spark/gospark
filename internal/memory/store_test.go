package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	vaults := filepath.Join(root, "vaults")
	if err := os.MkdirAll(vaults, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Root:              filepath.Join(root, "memory"),
		NodeID:            "node-a",
		AllowedVaultRoots: []string{vaults},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func testVault(t *testing.T, manager *Manager, name string) *Vault {
	t.Helper()
	path := filepath.Join(manager.cfg.AllowedVaultRoots[0], name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	vault, err := manager.VaultForPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return vault
}

func remember(t *testing.T, vault *Vault, record Record) Record {
	t.Helper()
	stored, _, err := vault.Remember(record)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestVaultIsolationAndPersistence(t *testing.T) {
	manager := newTestManager(t)
	one := testVault(t, manager, "peer-alice")
	two := testVault(t, manager, "peer-bob")

	remember(t, one, Record{Type: Episodic, Text: "alice plans the release", SourceKind: SourceSession, Scope: Scope{UserID: "alice"}})
	remember(t, two, Record{Type: Episodic, Text: "bob plans the release", SourceKind: SourceSession, Scope: Scope{UserID: "bob"}})

	hits, err := one.Query(Query{Text: "release"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Record.Text, "alice") {
		t.Fatalf("alice query leaked or lost records: %+v", hits)
	}
	hits, err = two.Query(Query{Text: "release"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Record.Text, "bob") {
		t.Fatalf("bob query leaked or lost records: %+v", hits)
	}

	// Reopen the manager: the catalog and per-vault files must survive.
	reopened, err := NewManager(manager.cfg)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.VaultByID(one.ID)
	if err != nil {
		t.Fatal(err)
	}
	hits, err = again.Query(Query{Text: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("reopened vault lost records: %+v", hits)
	}
}

func TestScopeFiltering(t *testing.T) {
	manager := newTestManager(t)
	vault := testVault(t, manager, "peer-alice")
	remember(t, vault, Record{Type: Episodic, Text: "project alpha decision", Scope: Scope{UserID: "alice", ProjectID: "alpha"}})
	remember(t, vault, Record{Type: Episodic, Text: "project beta decision", Scope: Scope{UserID: "alice", ProjectID: "beta"}})
	remember(t, vault, Record{Type: Episodic, Text: "other user decision", Scope: Scope{UserID: "bob", ProjectID: "alpha"}})

	hits, err := vault.Query(Query{Text: "decision", Scope: Scope{UserID: "alice", ProjectID: "alpha"}, TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Record.Text, "alpha") {
		t.Fatalf("scope filter failed: %+v", hits)
	}
}

func TestRedactionAndDeduplication(t *testing.T) {
	manager := newTestManager(t)
	vault := testVault(t, manager, "peer-alice")
	stored := remember(t, vault, Record{
		Type:       Episodic,
		Text:       "use password=hunter2 and Bearer abcdefghijklmnop",
		SourceKind: SourceUser,
		SourceRef:  "02_Daily/2026-11-02.md",
	})
	if strings.Contains(stored.Text, "hunter2") || strings.Contains(stored.Text, "abcdefghijklmnop") {
		t.Fatalf("secret was not redacted: %q", stored.Text)
	}
	again, created, err := vault.Remember(Record{Type: Episodic, Text: stored.Text, SourceKind: SourceUser, SourceRef: "02_Daily/2026-11-02.md"})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("expected the duplicate to be deduplicated")
	}
	if again.ID != stored.ID {
		t.Fatalf("dedupe returned a different record: %s vs %s", again.ID, stored.ID)
	}
}

func TestForgetSuppressesAndTombstones(t *testing.T) {
	manager := newTestManager(t)
	vault := testVault(t, manager, "peer-alice")
	keep := remember(t, vault, Record{Type: Semantic, Text: "keep this", SourceKind: SourceVault, SourceRef: "05_Knowledge/keep.md"})
	drop := remember(t, vault, Record{Type: Episodic, Text: "drop this", SourceKind: SourceSession})

	changed, err := vault.Forget([]string{drop.ID}, "suppress", "user request")
	if err != nil || changed != 1 {
		t.Fatalf("suppress = %d, %v", changed, err)
	}
	hits, _ := vault.Query(Query{Text: "drop"})
	if len(hits) != 0 {
		t.Fatalf("suppressed record returned: %+v", hits)
	}
	if _, ok := vault.Get(drop.ID); !ok {
		t.Fatal("suppression erased the audit record")
	}
	changed, err = vault.Forget([]string{keep.ID}, "delete", "erasure request")
	if err != nil || changed != 1 {
		t.Fatalf("delete = %d, %v", changed, err)
	}
	record, ok := vault.Get(keep.ID)
	if !ok || record.RetentionState != Deleted || record.Tombstone != "erasure request" {
		t.Fatalf("delete did not leave a tombstone: %+v", record)
	}
}

func TestPruneSuppressesExpiredRecords(t *testing.T) {
	manager := newTestManager(t)
	vault := testVault(t, manager, "peer-alice")
	expired := remember(t, vault, Record{
		Type:           Episodic,
		Text:           "old note",
		RetentionUntil: "2020-01-01T00:00:00Z",
	})
	changed, err := vault.Prune(time.Now())
	if err != nil || changed != 1 {
		t.Fatalf("prune = %d, %v", changed, err)
	}
	record, _ := vault.Get(expired.ID)
	if record.RetentionState != Suppressed {
		t.Fatalf("expired record state = %s", record.RetentionState)
	}
}

func TestTypesAndTopK(t *testing.T) {
	manager := newTestManager(t)
	vault := testVault(t, manager, "peer-alice")
	remember(t, vault, Record{Type: Episodic, Text: "release notes for the node", Importance: 0.9})
	remember(t, vault, Record{Type: Procedural, Text: "release workflow runbook", Importance: 0.2})

	hits, err := vault.Query(Query{Text: "release", Types: []Type{Procedural}, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Record.Type != Procedural {
		t.Fatalf("type filter failed: %+v", hits)
	}
}

func TestExportImportConflict(t *testing.T) {
	manager := newTestManager(t)
	source := testVault(t, manager, "peer-alice")
	remember(t, source, Record{Type: Semantic, Text: "shared decision", SourceKind: SourceVault, SourceRef: "05_Knowledge/decision.md"})
	bundle, err := source.Export()
	if err != nil {
		t.Fatal(err)
	}
	target := testVault(t, manager, "peer-bob")
	result, err := target.Import(bundle, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 {
		t.Fatalf("import result = %+v", result)
	}
	result, err = target.Import(bundle, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != 1 {
		t.Fatalf("second import result = %+v", result)
	}
}

func TestUnsafeSourceRefAndVaultRoot(t *testing.T) {
	manager := newTestManager(t)
	vault := testVault(t, manager, "peer-alice")
	if _, _, err := vault.Remember(Record{Type: Semantic, Text: "x", SourceRef: "../escape"}); err == nil {
		t.Fatal("expected unsafe source ref refusal")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VaultForPath(outside); err != ErrVaultForbidden {
		t.Fatalf("outside vault = %v, want ErrVaultForbidden", err)
	}
}

func TestStatusIsContentFree(t *testing.T) {
	manager := newTestManager(t)
	vault := testVault(t, manager, "peer-alice")
	remember(t, vault, Record{Type: Episodic, Text: "a secret body", Importance: 0.4})
	status := vault.Status()
	if status.Records != 1 || status.ByType["episodic"] != 1 || status.ByState["active"] != 1 {
		t.Fatalf("status = %+v", status)
	}
	if status.Schema != "sparknet-memory-status/1" {
		t.Fatalf("status schema = %q", status.Schema)
	}
}
