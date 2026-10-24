package authz

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/identity"
)

// Registry refusals. The registry is the authority for "may this node hold
// this", so every failure is a distinct, fail-closed refusal. They mirror the
// fabric's evaluate() reasons (shared/sparknet-authz.py).
var (
	ErrRegistrySignature        = errors.New("authz: registry entry signature does not verify")
	ErrRegistrySchema           = errors.New("authz: unsupported registry schema version")
	ErrRegistryUnknownNode      = errors.New("authz: node not in registry")
	ErrRegistryRevoked          = errors.New("authz: node is revoked")
	ErrRegistryExpired          = errors.New("authz: node authorization expired")
	ErrRegistryNotYetValid      = errors.New("authz: node authorization not yet valid")
	ErrRegistryIdentityMismatch = errors.New("authz: registry entry identity does not match its fingerprint")
	ErrRegistryUnknownCap       = errors.New("authz: registry entry carries an unknown capability")
	ErrRegistryMalformed        = errors.New("authz: registry entry is malformed")
	ErrRegistryRollback         = errors.New("authz: registry is older than the one already adopted")
)

// DefaultAuthorizationTTL is the registry-entry lifetime the fabric mints by
// default (sign --ttl-days 365).
const DefaultAuthorizationTTL = 365 * 24 * time.Hour

// clockSkew is the tolerance the fabric allows on a not-yet-valid entry.
const clockSkew = 5 * time.Minute

// signedEnvelope is the exact projection the Master signs for one entry: the
// fabric's `signed_envelope_fields`. `capabilities` is sorted and the identity
// projected, so re-serialising cannot change the bytes.
type signedEnvelope struct {
	Fingerprint  string             `json:"fingerprint"`
	Identity     apiv1.NodeIdentity `json:"identity"`
	Capabilities []string           `json:"capabilities"`
	OverlayIPv4  string             `json:"overlay_ipv4"`
	IssuedAt     string             `json:"issued_at"`
	ExpiresAt    string             `json:"expires_at"`
	Revoked      bool               `json:"revoked"`
	// No omitempty: the fabric always signs this key, as null when the Master
	// attested no department. Two canonical forms for one logical entry is the
	// ambiguity the byte-for-byte rule exists to prevent.
	Department *string `json:"department"`
}

// AuthorizationMessage returns the canonical bytes the Master signs for one
// registry entry. It is exported so a verifier and a signer can never compute
// different bytes.
func AuthorizationMessage(entry *apiv1.NodeAuthorization) ([]byte, error) {
	env := signedEnvelope{
		Fingerprint:  entry.Fingerprint,
		Identity:     normaliseIdentity(entry.Identity),
		Capabilities: sortedCaps(entry.Capabilities),
		OverlayIPv4:  entry.OverlayIPv4,
		IssuedAt:     entry.IssuedAt,
		ExpiresAt:    entry.ExpiresAt,
		Revoked:      entry.Revoked,
		Department:   entry.Department,
	}
	return apiv1.CanonicalJSON(env)
}

// NewAuthorization builds and signs one registry entry for the given identity.
// It validates the capabilities are in the vocabulary, sorts them, projects the
// identity, computes the fingerprint and signs the signed envelope.
func NewAuthorization(master *identity.Identity, ni apiv1.NodeIdentity, capabilities []string, issuedAt, expiresAt time.Time) (apiv1.NodeAuthorization, error) {
	var entry apiv1.NodeAuthorization
	if master == nil || master.Private == nil {
		return entry, errors.New("authz: signing an authorization requires the master private key")
	}
	if err := validateCaps(capabilities); err != nil {
		return entry, err
	}
	ni = normaliseIdentity(ni)
	fp, err := identity.FingerprintOf(ni)
	if err != nil {
		return entry, err
	}
	entry = apiv1.NodeAuthorization{
		Fingerprint:  fp,
		Identity:     ni,
		Capabilities: sortedCaps(capabilities),
		OverlayIPv4:  ni.OverlayIPv4,
		IssuedAt:     isoTime(issuedAt),
		ExpiresAt:    isoTime(expiresAt),
	}
	if err := signEntry(master, &entry); err != nil {
		return apiv1.NodeAuthorization{}, err
	}
	return entry, nil
}

// SignRegistry fills the master public key and (re)signs every authorization in
// place. It is the only writer of an entry's fingerprint and signature, so a
// caller cannot accidentally sign a stale or widened entry. An entry with an
// empty issued_at is stamped now, and an empty expires_at gets the default TTL.
func SignRegistry(master *identity.Identity, reg *apiv1.NodeRegistry) error {
	if master == nil || master.Private == nil {
		return errors.New("authz: signing a registry requires the master private key")
	}
	if reg.SchemaVersion == "" {
		reg.SchemaVersion = apiv1.RegistrySchemaVersion
	}
	reg.MasterPublicKeyB64 = master.PublicKeyB64()
	now := time.Now()
	for i := range reg.Authorizations {
		e := &reg.Authorizations[i]
		e.Identity = normaliseIdentity(e.Identity)
		if e.OverlayIPv4 == "" {
			e.OverlayIPv4 = e.Identity.OverlayIPv4
		}
		if e.IssuedAt == "" {
			e.IssuedAt = isoTime(now)
		}
		if e.ExpiresAt == "" {
			e.ExpiresAt = isoTime(now.Add(DefaultAuthorizationTTL))
		}
		fp, err := identity.FingerprintOf(e.Identity)
		if err != nil {
			return fmt.Errorf("authz: fingerprint %s: %w", e.Identity.NodeID, err)
		}
		e.Fingerprint = fp
		if err := validateCaps(e.Capabilities); err != nil {
			return fmt.Errorf("authz: %s: %w", e.Identity.NodeID, err)
		}
		e.Capabilities = sortedCaps(e.Capabilities)
		if err := signEntry(master, e); err != nil {
			return err
		}
	}
	normaliseRegistry(reg)
	return nil
}

// SetRevocation flips one node's revocation flag and clears its issued_at so a
// subsequent SignRegistry re-signs it with a fresh stamp (advancing the rollback
// guard). Revoking widens nothing — the flag lives inside the signature, so only
// the Master can set it, and reinstating also needs a fresh Master signature.
func SetRevocation(reg *apiv1.NodeRegistry, nodeID string, revoked bool) error {
	if reg == nil {
		return errors.New("authz: nil registry")
	}
	for i := range reg.Authorizations {
		if reg.Authorizations[i].Identity.NodeID == nodeID {
			reg.Authorizations[i].Revoked = revoked
			reg.Authorizations[i].IssuedAt = ""
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrRegistryUnknownNode, nodeID)
}

// VerifyRegistry checks every entry against the pinned master public key. It
// refuses an unknown schema, and per entry an unknown capability, an identity
// that does not match its fingerprint, and a bad signature. It does not check
// liveness; the Registry view does that at authorization time.
func VerifyRegistry(masterPub ed25519.PublicKey, reg *apiv1.NodeRegistry) error {
	if reg == nil {
		return errors.New("authz: nil registry")
	}
	if reg.SchemaVersion != apiv1.RegistrySchemaVersion {
		return fmt.Errorf("%w: %q", ErrRegistrySchema, reg.SchemaVersion)
	}
	for i := range reg.Authorizations {
		if err := VerifyAuthorization(masterPub, &reg.Authorizations[i]); err != nil {
			return err
		}
	}
	return nil
}

// VerifyAuthorization checks one entry: capabilities known and non-empty, the
// stored identity hashing to the fingerprint it is filed under, and a valid
// Master signature over the signed envelope.
func VerifyAuthorization(masterPub ed25519.PublicKey, entry *apiv1.NodeAuthorization) error {
	if entry == nil {
		return ErrRegistryMalformed
	}
	if len(entry.Capabilities) == 0 {
		return fmt.Errorf("%w: %s has no capabilities", ErrRegistryMalformed, entry.Identity.NodeID)
	}
	for _, c := range entry.Capabilities {
		if !IsKnown(Capability(c)) {
			return fmt.Errorf("%w: node=%s cap=%q", ErrRegistryUnknownCap, entry.Identity.NodeID, c)
		}
	}
	fp, err := identity.FingerprintOf(entry.Identity)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRegistryMalformed, err)
	}
	if fp != entry.Fingerprint {
		return fmt.Errorf("%w: node=%s stored=%s computed=%s", ErrRegistryIdentityMismatch, entry.Identity.NodeID, entry.Fingerprint, fp)
	}
	msg, err := AuthorizationMessage(entry)
	if err != nil {
		return fmt.Errorf("authz: canonicalise authorization: %w", err)
	}
	sig, err := identity.DecodeSignature(entry.SignatureB64)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRegistrySignature, err)
	}
	if !identity.Verify(masterPub, msg, sig) {
		return fmt.Errorf("%w: node=%s", ErrRegistrySignature, entry.Identity.NodeID)
	}
	return nil
}

// RegistryDigest is the sha256 hex of the registry's canonical JSON, the value
// peers echo in RegisterNode.registry_digest to prove they adopted the same
// document the control plane is serving.
func RegistryDigest(reg *apiv1.NodeRegistry) (string, error) {
	msg, err := apiv1.CanonicalJSON(reg)
	if err != nil {
		return "", fmt.Errorf("authz: canonicalise registry: %w", err)
	}
	sum := sha256.Sum256(msg)
	return hex.EncodeToString(sum[:]), nil
}

// Registry is an indexed, readable view of a NodeRegistry.
type Registry struct {
	reg    *apiv1.NodeRegistry
	byID   map[string]*apiv1.NodeAuthorization
	byFP   map[string]*apiv1.NodeAuthorization
	latest time.Time
}

// NewRegistry indexes a registry by node id and fingerprint.
func NewRegistry(reg *apiv1.NodeRegistry) *Registry {
	r := &Registry{
		reg:  reg,
		byID: map[string]*apiv1.NodeAuthorization{},
		byFP: map[string]*apiv1.NodeAuthorization{},
	}
	if reg == nil {
		return r
	}
	for i := range reg.Authorizations {
		e := &reg.Authorizations[i]
		r.byID[e.Identity.NodeID] = e
		if e.Fingerprint != "" {
			r.byFP[e.Fingerprint] = e
		}
		if ts, ok := parseISO(e.IssuedAt); ok && ts.After(r.latest) {
			r.latest = ts
		}
	}
	return r
}

// Document returns the underlying registry.
func (r *Registry) Document() *apiv1.NodeRegistry { return r.reg }

// Get returns a node's authorization by node id.
func (r *Registry) Get(nodeID string) (apiv1.NodeAuthorization, bool) {
	e, ok := r.byID[nodeID]
	if !ok {
		return apiv1.NodeAuthorization{}, false
	}
	return *e, true
}

// GetByFingerprint returns the authorization filed under a fingerprint.
func (r *Registry) GetByFingerprint(fp string) (apiv1.NodeAuthorization, bool) {
	e, ok := r.byFP[fp]
	if !ok {
		return apiv1.NodeAuthorization{}, false
	}
	return *e, true
}

// IDs returns the authorized node ids in sorted order.
func (r *Registry) IDs() []string {
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// LatestIssued is the newest issued_at across entries, the monotonic value the
// control plane and agent use to refuse a rolled-back (older) registry.
func (r *Registry) LatestIssued() time.Time { return r.latest }

// Authorized reports whether nodeID may hold cap at time now. It is fail-closed:
// unknown node, revocation, expiry, a not-yet-valid stamp and a capability the
// registry never granted all refuse.
func (r *Registry) Authorized(nodeID string, cap Capability, now time.Time) error {
	e, ok := r.byID[nodeID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrRegistryUnknownNode, nodeID)
	}
	if e.Revoked {
		return fmt.Errorf("%w: %q", ErrRegistryRevoked, nodeID)
	}
	if exp, ok := parseISO(e.ExpiresAt); ok && now.After(exp) {
		return fmt.Errorf("%w: node=%s expires=%s", ErrRegistryExpired, nodeID, e.ExpiresAt)
	}
	if iss, ok := parseISO(e.IssuedAt); ok && now.Add(clockSkew).Before(iss) {
		return fmt.Errorf("%w: node=%s issued=%s", ErrRegistryNotYetValid, nodeID, e.IssuedAt)
	}
	if cap != "" && !hasCapability(e.Capabilities, cap) {
		return fmt.Errorf("%w: node=%s need=%q granted=%v", ErrMissingCapability, nodeID, cap, e.Capabilities)
	}
	return nil
}

// ExecutorPublicKeys returns node_id -> public key for every non-revoked,
// unexpired entry, so the control plane can verify results without a second,
// hand-maintained key map. The key is the entry identity's
// wireguard_public_key_b64 slot.
func (r *Registry) ExecutorPublicKeys(now time.Time) (map[string]ed25519.PublicKey, error) {
	out := make(map[string]ed25519.PublicKey, len(r.byID))
	for id, e := range r.byID {
		if e.Revoked {
			continue
		}
		if exp, ok := parseISO(e.ExpiresAt); ok && now.After(exp) {
			continue
		}
		if e.Identity.WireGuardPublicKeyB64 == "" {
			continue
		}
		pub, err := identity.ParsePublicKeyB64(e.Identity.WireGuardPublicKeyB64)
		if err != nil {
			return nil, fmt.Errorf("authz: node %s public key: %w", id, err)
		}
		out[id] = pub
	}
	return out, nil
}

func signEntry(master *identity.Identity, entry *apiv1.NodeAuthorization) error {
	msg, err := AuthorizationMessage(entry)
	if err != nil {
		return fmt.Errorf("authz: canonicalise authorization: %w", err)
	}
	sig, err := master.Sign(msg)
	if err != nil {
		return fmt.Errorf("authz: sign authorization: %w", err)
	}
	entry.SignatureB64 = identity.EncodeSignature(sig)
	return nil
}

func validateCaps(caps []string) error {
	for _, c := range caps {
		if !IsKnown(Capability(c)) {
			return fmt.Errorf("%w: %q", ErrRegistryUnknownCap, c)
		}
	}
	return nil
}

func sortedCaps(caps []string) []string {
	out := append([]string(nil), caps...)
	sort.Strings(out)
	return out
}

func normaliseIdentity(ni apiv1.NodeIdentity) apiv1.NodeIdentity {
	if ni.RequestedCapabilities == nil {
		ni.RequestedCapabilities = []string{}
	}
	return ni
}

// normaliseRegistry sorts authorizations by node id and de-duplicates by node
// id (last wins) so the canonical bytes — and therefore the digest — are
// deterministic regardless of how the file was assembled.
func normaliseRegistry(reg *apiv1.NodeRegistry) {
	if len(reg.Authorizations) == 0 {
		reg.Authorizations = []apiv1.NodeAuthorization{}
		return
	}
	seen := make(map[string]int, len(reg.Authorizations))
	out := make([]apiv1.NodeAuthorization, 0, len(reg.Authorizations))
	for _, e := range reg.Authorizations {
		if i, ok := seen[e.Identity.NodeID]; ok {
			out[i] = e
			continue
		}
		seen[e.Identity.NodeID] = len(out)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity.NodeID < out[j].Identity.NodeID })
	reg.Authorizations = out
}

// isoTime renders t as the fabric does: RFC3339 in UTC with a trailing Z. The
// fabric's isoformat() carries sub-second precision, so goSpark keeps it too —
// a whole-second truncation would let two re-signs in the same second share a
// stamp and stall the rollback guard.
func isoTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseISO parses the fabric's ISO-8601 stamps, tolerating fractional seconds.
func parseISO(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
