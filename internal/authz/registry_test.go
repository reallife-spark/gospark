package authz_test

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/identity"
)

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// authzEntry mints a signed entry for id granting caps, valid now.
func authzEntry(t *testing.T, master, id *identity.Identity, caps ...authz.Capability) apiv1.NodeAuthorization {
	t.Helper()
	capStrs := make([]string, 0, len(caps))
	for _, c := range caps {
		capStrs = append(capStrs, string(c))
	}
	now := time.Now()
	entry, err := authz.NewAuthorization(master, id.Projection(), capStrs, now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("new authorization: %v", err)
	}
	return entry
}

func registryOf(entries ...apiv1.NodeAuthorization) *apiv1.NodeRegistry {
	return &apiv1.NodeRegistry{SchemaVersion: apiv1.RegistrySchemaVersion, Authorizations: entries}
}

func TestRegistrySignVerify(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", identity.RolePeer)
	reg := registryOf(authzEntry(t, master, a, authz.CapClusterServe))

	if err := authz.VerifyRegistry(master.Public, reg); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// A different key must not verify the same document.
	other, _ := identity.Generate("other", identity.RolePeer)
	if err := authz.VerifyRegistry(other.Public, reg); !errors.Is(err, authz.ErrRegistrySignature) {
		t.Fatalf("wrong key err = %v, want ErrRegistrySignature", err)
	}
}

func TestRegistryTamperDetected(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", identity.RolePeer)
	reg := registryOf(authzEntry(t, master, a, authz.CapClusterServe))

	// Escalating a node's capabilities after signing must invalidate it.
	reg.Authorizations[0].Capabilities = append(reg.Authorizations[0].Capabilities, string(authz.CapGuardApply))
	if err := authz.VerifyRegistry(master.Public, reg); !errors.Is(err, authz.ErrRegistrySignature) {
		t.Fatalf("tampered caps err = %v, want ErrRegistrySignature", err)
	}

	// Reinstating a revoked node requires a fresh signature; flipping the flag
	// back on a signed-revoked entry fails.
	reg2 := registryOf(authzEntry(t, master, a, authz.CapClusterServe))
	reg2.Authorizations[0].Revoked = true
	if err := authz.SignRegistry(master, reg2); err != nil {
		t.Fatalf("sign revoked: %v", err)
	}
	reg2.Authorizations[0].Revoked = false
	if err := authz.VerifyRegistry(master.Public, reg2); !errors.Is(err, authz.ErrRegistrySignature) {
		t.Fatalf("un-revoked err = %v, want ErrRegistrySignature", err)
	}
}

func TestRegistryIdentityMismatchRefused(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", identity.RolePeer)
	entry := authzEntry(t, master, a, authz.CapClusterServe)
	// Point the fingerprint at some other identity without re-signing.
	entry.Fingerprint = "deadbeef"
	if err := authz.VerifyAuthorization(master.Public, &entry); !errors.Is(err, authz.ErrRegistryIdentityMismatch) {
		t.Fatalf("err = %v, want ErrRegistryIdentityMismatch", err)
	}
}

func TestRegistryUnknownCapabilityRefused(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", identity.RolePeer)
	// NewAuthorization refuses at mint time...
	if _, err := authz.NewAuthorization(master, a.Projection(), []string{"root-everything"}, time.Now(), time.Now().Add(time.Hour)); !errors.Is(err, authz.ErrRegistryUnknownCap) {
		t.Fatalf("mint err = %v, want ErrRegistryUnknownCap", err)
	}
	// ...and verification refuses an entry smuggled in around it.
	entry := authzEntry(t, master, a, authz.CapClusterServe)
	entry.Capabilities = []string{"root-everything"}
	if err := authz.VerifyAuthorization(master.Public, &entry); !errors.Is(err, authz.ErrRegistryUnknownCap) {
		t.Fatalf("verify err = %v, want ErrRegistryUnknownCap", err)
	}
}

func TestRegistryUnauthorizedWrongSchema(t *testing.T) {
	master := newSigner(t)
	reg := registryOf()
	reg.SchemaVersion = "2.0"
	if err := authz.VerifyRegistry(master.Public, reg); !errors.Is(err, authz.ErrRegistrySchema) {
		t.Fatalf("err = %v, want ErrRegistrySchema", err)
	}
}

func TestRegistryAuthorized(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", identity.RolePeer)
	b, _ := identity.Generate("node-b", identity.RolePeer)
	reg := registryOf(
		authzEntry(t, master, a, authz.CapClusterServe, authz.CapTelemetry),
		authzEntry(t, master, b, authz.CapTelemetry),
	)
	view := authz.NewRegistry(reg)
	now := time.Now()

	if err := view.Authorized("node-a", authz.CapClusterServe, now); err != nil {
		t.Fatalf("node-a cluster-serve: %v", err)
	}
	if err := view.Authorized("node-b", authz.CapClusterServe, now); !errors.Is(err, authz.ErrMissingCapability) {
		t.Fatalf("node-b cluster-serve err = %v, want ErrMissingCapability", err)
	}
	if err := view.Authorized("node-c", authz.CapTelemetry, now); !errors.Is(err, authz.ErrRegistryUnknownNode) {
		t.Fatalf("node-c err = %v, want ErrRegistryUnknownNode", err)
	}
}

func TestRegistryRevoked(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", identity.RolePeer)
	reg := registryOf(authzEntry(t, master, a, authz.CapClusterServe))
	reg.Authorizations[0].Revoked = true
	if err := authz.SignRegistry(master, reg); err != nil {
		t.Fatal(err)
	}

	view := authz.NewRegistry(reg)
	if err := view.Authorized("node-a", authz.CapClusterServe, time.Now()); !errors.Is(err, authz.ErrRegistryRevoked) {
		t.Fatalf("err = %v, want ErrRegistryRevoked", err)
	}
	// A revoked node contributes no executor key.
	pubs, err := view.ExecutorPublicKeys(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pubs["node-a"]; ok {
		t.Fatal("revoked node still yielded a public key")
	}
}

func TestRegistryExpired(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", identity.RolePeer)
	now := time.Now()
	entry, err := authz.NewAuthorization(master, a.Projection(), []string{string(authz.CapClusterServe)}, now.Add(-2*time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	view := authz.NewRegistry(registryOf(entry))
	if err := view.Authorized("node-a", authz.CapClusterServe, now); !errors.Is(err, authz.ErrRegistryExpired) {
		t.Fatalf("err = %v, want ErrRegistryExpired", err)
	}
}

func TestRegistryNotYetValid(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", identity.RolePeer)
	now := time.Now()
	entry, err := authz.NewAuthorization(master, a.Projection(), []string{string(authz.CapClusterServe)}, now.Add(time.Hour), now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	view := authz.NewRegistry(registryOf(entry))
	if err := view.Authorized("node-a", authz.CapClusterServe, now); !errors.Is(err, authz.ErrRegistryNotYetValid) {
		t.Fatalf("err = %v, want ErrRegistryNotYetValid", err)
	}
}

func TestRegistryDigestStable(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", identity.RolePeer)
	reg := registryOf(authzEntry(t, master, a, authz.CapClusterServe))

	d1, err := authz.RegistryDigest(reg)
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := authz.RegistryDigest(reg)
	if d1 != d2 {
		t.Fatal("digest not stable")
	}
	if d1 == "" {
		t.Fatal("empty digest")
	}
}

func TestRegistryExecutorKeys(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", identity.RolePeer)
	view := authz.NewRegistry(registryOf(authzEntry(t, master, a, authz.CapClusterServe)))
	pubs, err := view.ExecutorPublicKeys(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := pubs["node-a"]
	if !ok {
		t.Fatal("node-a key missing")
	}
	if !got.Equal(ed25519.PublicKey(a.Public)) {
		t.Fatal("node-a key mismatch")
	}
}

func TestAuthorizationMessageMatchesFabric(t *testing.T) {
	// A golden vector: the signed envelope is the fabric's
	// signed_envelope_fields, sorted capabilities, projected identity.
	master, _ := identity.Generate("master", identity.RoleMaster)
	a, _ := identity.Generate("node-a", identity.RolePeer)
	a.OverlayIPv4 = "10.100.0.7"
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	entry, err := authz.NewAuthorization(master, a.Projection(), []string{"telemetry", "cluster-serve"}, at, at.Add(365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := authz.AuthorizationMessage(&entry)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"capabilities":["cluster-serve","telemetry"],"expires_at":"2027-01-02T03:04:05Z","fingerprint":"` +
		entry.Fingerprint + `","identity":{"node_id":"node-a","overlay_ipv4":"10.100.0.7","requested_capabilities":[],"role":"peer","wireguard_public_key_b64":"` +
		a.PublicKeyB64() + `"},"issued_at":"2026-01-02T03:04:05Z","overlay_ipv4":"10.100.0.7","revoked":false}`
	if string(msg) != want {
		t.Fatalf("authorization message mismatch:\n got %s\nwant %s", msg, want)
	}
}
