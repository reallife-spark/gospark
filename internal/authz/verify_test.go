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

func newSigner(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate("master", identity.RoleMaster)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return id
}

func signedTask(t *testing.T, id *identity.Identity, mutate func(*apiv1.TaskEnvelope)) apiv1.TaskEnvelope {
	t.Helper()
	now := time.Now()
	env := apiv1.TaskEnvelope{
		Fingerprint:        id.Fingerprint(),
		Capabilities:       []string{string(authz.CapClusterServe)},
		TaskID:             "t-1",
		Operation:          "arith",
		ParamsJSON:         `{"expression":"2+2"}`,
		ContentInSignature: true,
		Nonce:              "nonce-1",
		IssuedAtUnix:       now.Add(-time.Second).Unix(),
		ExpiresAtUnix:      now.Add(time.Minute).Unix(),
	}
	if mutate != nil {
		mutate(&env)
	}
	msg, err := apiv1.CanonicalBytes(&env)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	sig, err := id.Sign(msg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// A mutated envelope that is meant to fail signature verification keeps the
	// pre-mutation signature only if the mutation touched signed fields.
	env.SignatureB64 = identity.EncodeSignature(sig)
	return env
}

func newVerifier() *authz.Verifier {
	return &authz.Verifier{Nonces: authz.NewMemoryNonceStore(nil, time.Hour)}
}

func TestVerifyTaskAccepts(t *testing.T) {
	id := newSigner(t)
	env := signedTask(t, id, nil)
	if err := newVerifier().VerifyTask(id.Public, &env, authz.CapClusterServe); err != nil {
		t.Fatalf("expected accept, got %v", err)
	}
}

func TestVerifyTaskRejectsWrongKey(t *testing.T) {
	id := newSigner(t)
	other := newSigner(t)
	env := signedTask(t, id, nil)
	err := newVerifier().VerifyTask(other.Public, &env, authz.CapClusterServe)
	if !errors.Is(err, authz.ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}
}

func TestVerifyTaskTamperedContent(t *testing.T) {
	id := newSigner(t)
	env := signedTask(t, id, nil)
	env.ParamsJSON = `{"expression":"6*7"}` // mutate after signing
	err := newVerifier().VerifyTask(id.Public, &env, authz.CapClusterServe)
	if !errors.Is(err, authz.ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}
}

func TestVerifyTaskContentNotSigned(t *testing.T) {
	id := newSigner(t)
	env := signedTask(t, id, func(e *apiv1.TaskEnvelope) { e.ContentInSignature = false })
	err := newVerifier().VerifyTask(id.Public, &env, authz.CapClusterServe)
	if !errors.Is(err, authz.ErrContentNotSigned) {
		t.Fatalf("expected ErrContentNotSigned, got %v", err)
	}
}

func TestVerifyTaskExpired(t *testing.T) {
	id := newSigner(t)
	env := signedTask(t, id, func(e *apiv1.TaskEnvelope) {
		e.ExpiresAtUnix = time.Now().Add(-time.Minute).Unix()
	})
	err := newVerifier().VerifyTask(id.Public, &env, authz.CapClusterServe)
	if !errors.Is(err, authz.ErrExpired) {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestVerifyTaskMissingCapability(t *testing.T) {
	id := newSigner(t)
	env := signedTask(t, id, func(e *apiv1.TaskEnvelope) {
		e.Capabilities = []string{string(authz.CapTelemetry)}
	})
	err := newVerifier().VerifyTask(id.Public, &env, authz.CapClusterServe)
	if !errors.Is(err, authz.ErrMissingCapability) {
		t.Fatalf("expected ErrMissingCapability, got %v", err)
	}
}

func TestVerifyTaskUnknownCapability(t *testing.T) {
	id := newSigner(t)
	env := signedTask(t, id, func(e *apiv1.TaskEnvelope) {
		e.Capabilities = append(e.Capabilities, "root-everything")
	})
	err := newVerifier().VerifyTask(id.Public, &env, authz.CapClusterServe)
	if !errors.Is(err, authz.ErrUnknownCapability) {
		t.Fatalf("expected ErrUnknownCapability, got %v", err)
	}
}

func TestVerifyTaskReplayRefused(t *testing.T) {
	id := newSigner(t)
	env := signedTask(t, id, nil)
	v := newVerifier()
	if err := v.VerifyTask(id.Public, &env, authz.CapClusterServe); err != nil {
		t.Fatalf("first use should succeed: %v", err)
	}
	err := v.VerifyTask(id.Public, &env, authz.CapClusterServe)
	if !errors.Is(err, authz.ErrReplay) {
		t.Fatalf("expected ErrReplay, got %v", err)
	}
}

func TestVerifyTaskFingerprintPinned(t *testing.T) {
	id := newSigner(t)
	env := signedTask(t, id, nil)
	v := newVerifier()
	v.SignerFingerprint = "not-the-signer"
	err := v.VerifyTask(id.Public, &env, authz.CapClusterServe)
	if !errors.Is(err, authz.ErrFingerprint) {
		t.Fatalf("expected ErrFingerprint, got %v", err)
	}
}

func TestVerifyResultRoundTrip(t *testing.T) {
	id := newSigner(t)
	env := apiv1.ResultEnvelope{
		TaskID:        "t-1",
		NodeID:        "node-a",
		Fingerprint:   id.Fingerprint(),
		Operation:     "arith",
		Result:        "4",
		CompletedUnix: time.Now().Unix(),
	}
	msg, err := apiv1.CanonicalBytes(&env)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := id.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	env.SignatureB64 = identity.EncodeSignature(sig)

	if err := authz.VerifyResult(id.Public, &env); err != nil {
		t.Fatalf("expected accept, got %v", err)
	}
	env.Result = "5"
	if err := authz.VerifyResult(id.Public, &env); !errors.Is(err, authz.ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature after tamper, got %v", err)
	}
}

func TestVocabularyMatchesFabric(t *testing.T) {
	want := []string{
		"admin-supervise", "cluster-dispatch", "cluster-serve", "cluster-vote",
		"cross-department-share", "guard-apply", "memory-supervise", "model-uplift",
		"p2p-sync", "push-receive", "telemetry",
	}
	got := authz.KnownSorted()
	if len(got) != len(want) {
		t.Fatalf("capability count = %d, want %d", len(got), len(want))
	}
	for i, c := range got {
		if string(c) != want[i] {
			t.Errorf("capability[%d] = %q, want %q", i, c, want[i])
		}
	}
	if !authz.IsSensitive(authz.CapClusterServe) || !authz.IsSensitive(authz.CapGuardApply) || !authz.IsSensitive(authz.CapMemorySupervise) {
		t.Error("cluster-serve, guard-apply and memory-supervise must be sensitive")
	}
	if authz.IsSensitive(authz.CapTelemetry) {
		t.Error("telemetry must not be sensitive")
	}
}

func TestEmptyNonceIsRefused(t *testing.T) {
	store := authz.NewMemoryNonceStore(nil, time.Hour)
	ok, err := store.Use("", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("empty nonce must never be accepted")
	}
}

func TestPublicKeySizeGuard(t *testing.T) {
	id := newSigner(t)
	env := signedTask(t, id, nil)
	short := ed25519.PublicKey([]byte{1, 2, 3})
	if err := newVerifier().VerifyTask(short, &env, authz.CapClusterServe); !errors.Is(err, authz.ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature for short key, got %v", err)
	}
}

func TestVerifyTaskRejectsZeroTTL(t *testing.T) {
	id := newSigner(t)
	env := signedTask(t, id, func(e *apiv1.TaskEnvelope) {
		e.ExpiresAtUnix = 0
	})
	err := newVerifier().VerifyTask(id.Public, &env, authz.CapClusterServe)
	if !errors.Is(err, authz.ErrMalformedEnvelope) {
		t.Fatalf("expected ErrMalformedEnvelope, got %v", err)
	}
}

func TestVerifyTaskRequiresNonceStore(t *testing.T) {
	id := newSigner(t)
	env := signedTask(t, id, nil)
	verifier := &authz.Verifier{}
	err := verifier.VerifyTask(id.Public, &env, authz.CapClusterServe)
	if !errors.Is(err, authz.ErrMalformedEnvelope) {
		t.Fatalf("expected ErrMalformedEnvelope, got %v", err)
	}
}
