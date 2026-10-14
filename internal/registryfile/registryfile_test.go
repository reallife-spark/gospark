package registryfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reallife-spark/goSpark/internal/registryfile"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAuthorizationsArray(t *testing.T) {
	path := write(t, "authz.json", `[{"node_id":"node-a","capabilities":["telemetry"]}]`)
	entries, err := registryfile.LoadAuthorizations(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].NodeID != "node-a" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestLoadAuthorizationsObject(t *testing.T) {
	path := write(t, "reg.json", `{"schema_version":"1.0","authorizations":[{"node_id":"node-a"}]}`)
	entries, err := registryfile.LoadAuthorizations(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].NodeID != "node-a" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestLoadFullRegistry(t *testing.T) {
	path := write(t, "reg.json", `{"schema_version":"1.0","issued_at_unix":42,"master_public_key_b64":"abc","signature_b64":"sig","authorizations":[{"node_id":"node-a"}]}`)
	reg, err := registryfile.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reg.IssuedAtUnix != 42 || reg.SignatureB64 != "sig" || len(reg.Authorizations) != 1 {
		t.Fatalf("registry = %+v", reg)
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	if _, err := registryfile.Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
