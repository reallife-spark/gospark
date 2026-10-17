package authz

import (
	"errors"
	"fmt"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/identity"
)

// Sentinel refusals. The control plane fails closed: every path that is not an
// explicit success returns one of these.
var (
	ErrBadSignature      = errors.New("authz: signature does not verify")
	ErrExpired           = errors.New("authz: envelope expired")
	ErrNotYetValid       = errors.New("authz: envelope not yet valid")
	ErrReplay            = errors.New("authz: nonce already used")
	ErrMissingCapability = errors.New("authz: required capability absent")
	ErrUnknownCapability = errors.New("authz: capability not in vocabulary")
	ErrContentNotSigned  = errors.New("authz: content must be inside the signature")
	ErrFingerprint       = errors.New("authz: fingerprint mismatch")
)

// Clock is injectable so tests can pin "now".
type Clock func() time.Time

// Verifier checks signed envelopes from a known signer.
type Verifier struct {
	// SignerFingerprint, when non-empty, must equal the envelope fingerprint.
	SignerFingerprint string
	// Nonces is the replay guard. Required.
	Nonces NonceStore
	// Now defaults to time.Now.
	Now Clock
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// VerifyTask performs the fail-closed verification of a signed compute task.
//
// Order mirrors COMPUTE_CAPABILITY_DESIGN.md: signature first, then liveness,
// then capability, then replay — each refusal is its own error so the caller
// can distinguish an attack from a stale message.
func (v *Verifier) VerifyTask(signerPub []byte, env *apiv1.TaskEnvelope, required Capability) error {
	if env == nil {
		return errors.New("authz: nil task envelope")
	}
	if !env.ContentInSignature {
		return ErrContentNotSigned
	}
	if v.SignerFingerprint != "" && env.Fingerprint != v.SignerFingerprint {
		return fmt.Errorf("%w: envelope=%s verifier=%s", ErrFingerprint, env.Fingerprint, v.SignerFingerprint)
	}

	// 1. Unknown capability is refused before anything else is trusted.
	for _, c := range env.Capabilities {
		if !IsKnown(Capability(c)) {
			return fmt.Errorf("%w: %q", ErrUnknownCapability, c)
		}
	}

	// 2. Signature over the canonical bytes.
	msg, err := apiv1.CanonicalBytes(env)
	if err != nil {
		return fmt.Errorf("authz: canonicalise: %w", err)
	}
	sig, err := identity.DecodeSignature(env.SignatureB64)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	if !identity.Verify(signerPub, msg, sig) {
		return ErrBadSignature
	}

	// 3. Liveness against the injected clock.
	now := v.now().Unix()
	if env.ExpiresAtUnix > 0 && now > env.ExpiresAtUnix {
		return fmt.Errorf("%w: now=%d expires=%d", ErrExpired, now, env.ExpiresAtUnix)
	}
	if env.IssuedAtUnix > 0 && now < env.IssuedAtUnix {
		return fmt.Errorf("%w: now=%d issued=%d", ErrNotYetValid, now, env.IssuedAtUnix)
	}

	// 4. Required capability present.
	if !hasCapability(env.Capabilities, required) {
		return fmt.Errorf("%w: need %q", ErrMissingCapability, required)
	}

	// 5. Replay guard last, so a malformed envelope never consumes a nonce.
	if v.Nonces != nil {
		ok, err := v.Nonces.Use(env.Nonce, time.Unix(env.ExpiresAtUnix, 0))
		if err != nil {
			return fmt.Errorf("authz: nonce store: %w", err)
		}
		if !ok {
			return fmt.Errorf("%w: %q", ErrReplay, env.Nonce)
		}
	}
	return nil
}

// VerifyResult checks an executor's attested result. The signature proves
// provenance (who computed it) and never permission — the same asymmetry the
// fabric draws between a vote and a grant.
func VerifyResult(executorPub []byte, env *apiv1.ResultEnvelope) error {
	if env == nil {
		return errors.New("authz: nil result envelope")
	}
	// env.Operation is not validated here: a result's operation names a compute
	// task, not a capability, so it is carried verbatim into the signed bytes
	// (CanonicalBytes below) and proven by signature rather than by a grant.
	msg, err := apiv1.CanonicalBytes(env)
	if err != nil {
		return fmt.Errorf("authz: canonicalise: %w", err)
	}
	sig, err := identity.DecodeSignature(env.SignatureB64)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	if !identity.Verify(executorPub, msg, sig) {
		return ErrBadSignature
	}
	return nil
}

func hasCapability(caps []string, want Capability) bool {
	if want == "" {
		return true
	}
	for _, c := range caps {
		if Capability(c) == want {
			return true
		}
	}
	return false
}
