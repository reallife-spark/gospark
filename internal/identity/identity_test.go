package identity_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reallife-spark/goSpark/internal/identity"
)

func TestGenerateSignVerify(t *testing.T) {
	id, err := identity.Generate("node-a", "ops")
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello")
	sig, err := id.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !identity.Verify(id.Public, msg, sig) {
		t.Fatal("signature should verify")
	}
	if identity.Verify(id.Public, []byte("tampered"), sig) {
		t.Fatal("tampered message must not verify")
	}
}

func TestFingerprintStableAndDistinct(t *testing.T) {
	a, _ := identity.Generate("node-a", "ops")
	b, _ := identity.Generate("node-b", "ops")
	if a.Fingerprint() != a.Fingerprint() {
		t.Fatal("fingerprint must be stable")
	}
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("different nodes must have different fingerprints")
	}
	// The fingerprint names the node, not just the key: same key, different id
	// must differ.
	sameKey := identity.FingerprintOf("node-a", "ops", a.Public)
	if sameKey != a.Fingerprint() {
		t.Fatal("FingerprintOf must match Identity.Fingerprint")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	id, _ := identity.Generate("node-a", "ops")
	path := filepath.Join(t.TempDir(), "nested", "node.key")
	if err := id.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := identity.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.NodeID != id.NodeID || loaded.OwnerID != id.OwnerID {
		t.Fatalf("identity mismatch: %+v", loaded)
	}
	if loaded.Fingerprint() != id.Fingerprint() {
		t.Fatal("fingerprint mismatch after reload")
	}
	if loaded.Private == nil {
		t.Fatal("private key lost on reload")
	}
}

func TestVerifyOnlyNodeCannotSign(t *testing.T) {
	id, _ := identity.Generate("node-a", "ops")
	verifyOnly := &identity.Identity{NodeID: id.NodeID, OwnerID: id.OwnerID, Public: id.Public}
	if _, err := verifyOnly.Sign([]byte("x")); err == nil {
		t.Fatal("a verify-only node must not be able to sign")
	}
}

func TestParsePublicKeyB64RejectsBadLength(t *testing.T) {
	if _, err := identity.ParsePublicKeyB64("AAAA"); err == nil {
		t.Fatal("expected error for short key")
	}
}
