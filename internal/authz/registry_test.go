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

func signedRegistry(t *testing.T, master *identity.Identity, nodes ...apiv1.NodeAuthorization) *apiv1.NodeRegistry {
	t.Helper()
	reg := &apiv1.NodeRegistry{
		IssuedAtUnix:   time.Now().Unix(),
		Authorizations: nodes,
	}
	if err := authz.SignRegistry(master, reg); err != nil {
		t.Fatalf("sign registry: %v", err)
	}
	return reg
}

func node(t *testing.T, id *identity.Identity, caps ...authz.Capability) apiv1.NodeAuthorization {
	t.Helper()
	capStrs := make([]string, 0, len(caps))
	for _, c := range caps {
		capStrs = append(capStrs, string(c))
	}
	return apiv1.NodeAuthorization{
		NodeID:       id.NodeID,
		Fingerprint:  id.Fingerprint(),
		PublicKeyB64: id.PublicKeyB64(),
		Capabilities: capStrs,
		IssuedAtUnix: time.Now().Unix(),
	}
}

func TestRegistrySignVerify(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", "ops")
	reg := signedRegistry(t, master, node(t, a, authz.CapClusterServe))

	if err := authz.VerifyRegistry(master.Public, reg); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// A different key must not verify the same document.
	other, _ := identity.Generate("other", "ops")
	if err := authz.VerifyRegistry(other.Public, reg); !errors.Is(err, authz.ErrRegistrySignature) {
		t.Fatalf("wrong key err = %v, want ErrRegistrySignature", err)
	}
}

func TestRegistryTamperDetected(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", "ops")
	reg := signedRegistry(t, master, node(t, a, authz.CapClusterServe))

	// Escalating a node's capabilities after signing must invalidate it.
	reg.Authorizations[0].Capabilities = append(reg.Authorizations[0].Capabilities, string(authz.CapGuardApply))
	if err := authz.VerifyRegistry(master.Public, reg); !errors.Is(err, authz.ErrRegistrySignature) {
		t.Fatalf("tampered caps err = %v, want ErrRegistrySignature", err)
	}

	// Reinstating a revoked node requires a fresh signature; flipping the flag
	// back on a signed-revoked document fails too.
	reg2 := signedRegistry(t, master, apiv1.NodeAuthorization{
		NodeID: "node-a", Fingerprint: a.Fingerprint(), PublicKeyB64: a.PublicKeyB64(), Revoked: true,
	})
	reg2.Authorizations[0].Revoked = false
	if err := authz.VerifyRegistry(master.Public, reg2); !errors.Is(err, authz.ErrRegistrySignature) {
		t.Fatalf("un-revoked err = %v, want ErrRegistrySignature", err)
	}
}

func TestRegistryUnknownCapabilityRefused(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", "ops")
	reg := &apiv1.NodeRegistry{
		IssuedAtUnix: time.Now().Unix(),
		Authorizations: []apiv1.NodeAuthorization{{
			NodeID:       a.NodeID,
			Fingerprint:  a.Fingerprint(),
			Capabilities: []string{"root-everything"},
		}},
	}
	if err := authz.SignRegistry(master, reg); err != nil {
		t.Fatalf("sign: %v", err)
	}
	// Signing does not refuse; verification does, before trusting anything.
	if err := authz.VerifyRegistry(master.Public, reg); !errors.Is(err, authz.ErrRegistryUnknownCap) {
		t.Fatalf("err = %v, want ErrRegistryUnknownCap", err)
	}
}

func TestRegistryUnauthorizedWrongSchema(t *testing.T) {
	master := newSigner(t)
	reg := signedRegistry(t, master)
	reg.SchemaVersion = "2.0"
	if err := authz.VerifyRegistry(master.Public, reg); !errors.Is(err, authz.ErrRegistrySchema) {
		t.Fatalf("err = %v, want ErrRegistrySchema", err)
	}
}

func TestRegistryAuthorized(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", "ops")
	b, _ := identity.Generate("node-b", "ops")
	reg := signedRegistry(t, master,
		node(t, a, authz.CapClusterServe, authz.CapTelemetry),
		apiv1.NodeAuthorization{
			NodeID: "node-b", Fingerprint: b.Fingerprint(), PublicKeyB64: b.PublicKeyB64(),
			Capabilities: []string{string(authz.CapTelemetry)},
		},
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
	a, _ := identity.Generate("node-a", "ops")
	revoked := node(t, a, authz.CapClusterServe)
	revoked.Revoked = true
	reg := signedRegistry(t, master, revoked)

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
	a, _ := identity.Generate("node-a", "ops")
	entry := node(t, a, authz.CapClusterServe)
	entry.ExpiresAtUnix = time.Now().Add(-time.Minute).Unix()
	reg := signedRegistry(t, master, entry)

	view := authz.NewRegistry(reg)
	if err := view.Authorized("node-a", authz.CapClusterServe, time.Now()); !errors.Is(err, authz.ErrRegistryExpired) {
		t.Fatalf("err = %v, want ErrRegistryExpired", err)
	}
}

func TestRegistryDigestStableAndOrderIndependent(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", "ops")
	b, _ := identity.Generate("node-b", "ops")
	first := signedRegistry(t, master, node(t, a, authz.CapClusterServe), node(t, b, authz.CapClusterServe))
	// Same entries, reverse order; SignRegistry normalises, so digests match.
	second := signedRegistry(t, master, node(t, b, authz.CapClusterServe), node(t, a, authz.CapClusterServe))

	d1, err := authz.RegistryDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := authz.RegistryDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("digests differ by order: %s vs %s", d1, d2)
	}

	// A different issued_at (fresh signature) changes the digest.
	third := signedRegistry(t, master, node(t, a, authz.CapClusterServe), node(t, b, authz.CapClusterServe))
	third.IssuedAtUnix++
	if err := authz.SignRegistry(master, third); err != nil {
		t.Fatal(err)
	}
	d3, _ := authz.RegistryDigest(third)
	if d3 == d1 {
		t.Fatal("digest unchanged after re-sign with a new issue time")
	}
}

func TestRegistryExecutorKeys(t *testing.T) {
	master := newSigner(t)
	a, _ := identity.Generate("node-a", "ops")
	reg := signedRegistry(t, master, node(t, a, authz.CapClusterServe))
	view := authz.NewRegistry(reg)
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
