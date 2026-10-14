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
// this", so every failure is a distinct, fail-closed refusal.
var (
	ErrRegistrySignature   = errors.New("authz: registry signature does not verify")
	ErrRegistrySchema      = errors.New("authz: unsupported registry schema version")
	ErrRegistryUnknownNode = errors.New("authz: node not in registry")
	ErrRegistryRevoked     = errors.New("authz: node is revoked")
	ErrRegistryExpired     = errors.New("authz: node authorization expired")
	ErrRegistryUnknownCap  = errors.New("authz: registry entry carries an unknown capability")
	ErrRegistryRollback    = errors.New("authz: registry is older than the one already adopted")
)

// SignRegistry fills MasterPublicKeyB64, normalises the entries and signs the
// canonical registry with the master identity. It is the only writer of
// SignatureB64, so a caller cannot accidentally sign a stale document.
func SignRegistry(master *identity.Identity, reg *apiv1.NodeRegistry) error {
	if master == nil || master.Private == nil {
		return errors.New("authz: signing a registry requires the master private key")
	}
	if reg.SchemaVersion == "" {
		reg.SchemaVersion = apiv1.RegistrySchemaVersion
	}
	reg.MasterPublicKeyB64 = master.PublicKeyB64()
	normaliseRegistry(reg)
	msg, err := apiv1.CanonicalBytes(reg)
	if err != nil {
		return fmt.Errorf("authz: canonicalise registry: %w", err)
	}
	sig, err := master.Sign(msg)
	if err != nil {
		return fmt.Errorf("authz: sign registry: %w", err)
	}
	reg.SignatureB64 = identity.EncodeSignature(sig)
	return nil
}

// VerifyRegistry checks a Master-signed registry against the pinned master
// public key. It refuses an unknown schema, an unknown capability in any entry,
// and a bad signature; it does not check liveness (a registry has no start/stop
// window the way a task does).
func VerifyRegistry(masterPub ed25519.PublicKey, reg *apiv1.NodeRegistry) error {
	if reg == nil {
		return errors.New("authz: nil registry")
	}
	if reg.SchemaVersion != apiv1.RegistrySchemaVersion {
		return fmt.Errorf("%w: %q", ErrRegistrySchema, reg.SchemaVersion)
	}
	for _, e := range reg.Authorizations {
		for _, c := range e.Capabilities {
			if !IsKnown(Capability(c)) {
				return fmt.Errorf("%w: node=%s cap=%q", ErrRegistryUnknownCap, e.NodeID, c)
			}
		}
	}
	msg, err := apiv1.CanonicalBytes(reg)
	if err != nil {
		return fmt.Errorf("authz: canonicalise registry: %w", err)
	}
	sig, err := identity.DecodeSignature(reg.SignatureB64)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRegistrySignature, err)
	}
	if !identity.Verify(masterPub, msg, sig) {
		return ErrRegistrySignature
	}
	return nil
}

// RegistryDigest is the sha256 hex of the registry's canonical (unsigned) bytes,
// the value peers echo in RegisterNode.registry_digest to prove they adopted
// the same document the control plane is serving.
func RegistryDigest(reg *apiv1.NodeRegistry) (string, error) {
	msg, err := apiv1.CanonicalBytes(reg)
	if err != nil {
		return "", fmt.Errorf("authz: canonicalise registry: %w", err)
	}
	sum := sha256.Sum256(msg)
	return hex.EncodeToString(sum[:]), nil
}

// Registry is an indexed, readable view of a NodeRegistry.
type Registry struct {
	reg  *apiv1.NodeRegistry
	byID map[string]apiv1.NodeAuthorization
	byFP map[string]apiv1.NodeAuthorization
}

// NewRegistry indexes a registry by node id and fingerprint.
func NewRegistry(reg *apiv1.NodeRegistry) *Registry {
	r := &Registry{reg: reg, byID: map[string]apiv1.NodeAuthorization{}, byFP: map[string]apiv1.NodeAuthorization{}}
	if reg == nil {
		return r
	}
	for _, e := range reg.Authorizations {
		r.byID[e.NodeID] = e
		if e.Fingerprint != "" {
			r.byFP[e.Fingerprint] = e
		}
	}
	return r
}

// Document returns the underlying registry.
func (r *Registry) Document() *apiv1.NodeRegistry { return r.reg }

// Get returns a node's authorization.
func (r *Registry) Get(nodeID string) (apiv1.NodeAuthorization, bool) {
	e, ok := r.byID[nodeID]
	return e, ok
}

// GetByFingerprint returns the authorization filed under a fingerprint.
func (r *Registry) GetByFingerprint(fp string) (apiv1.NodeAuthorization, bool) {
	e, ok := r.byFP[fp]
	return e, ok
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

// Authorized reports whether nodeID may hold cap at time now. It is fail-closed:
// unknown node, revocation and expiry all refuse, and a capability the registry
// never granted is refused even if the node holds it locally.
func (r *Registry) Authorized(nodeID string, cap Capability, now time.Time) error {
	e, ok := r.byID[nodeID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrRegistryUnknownNode, nodeID)
	}
	if e.Revoked {
		return fmt.Errorf("%w: %q", ErrRegistryRevoked, nodeID)
	}
	if e.ExpiresAtUnix > 0 && now.Unix() > e.ExpiresAtUnix {
		return fmt.Errorf("%w: node=%s expires=%d", ErrRegistryExpired, nodeID, e.ExpiresAtUnix)
	}
	if cap != "" && !hasCapability(e.Capabilities, cap) {
		return fmt.Errorf("%w: node=%s need=%q granted=%v", ErrMissingCapability, nodeID, cap, e.Capabilities)
	}
	return nil
}

// ExecutorPublicKeys returns node_id -> public key for every non-revoked,
// unexpired entry, so the control plane can verify results without a second,
// hand-maintained key map.
func (r *Registry) ExecutorPublicKeys(now time.Time) (map[string]ed25519.PublicKey, error) {
	out := make(map[string]ed25519.PublicKey, len(r.byID))
	for id, e := range r.byID {
		if e.Revoked {
			continue
		}
		if e.ExpiresAtUnix > 0 && now.Unix() > e.ExpiresAtUnix {
			continue
		}
		if e.PublicKeyB64 == "" {
			continue
		}
		pub, err := identity.ParsePublicKeyB64(e.PublicKeyB64)
		if err != nil {
			return nil, fmt.Errorf("authz: node %s public key: %w", id, err)
		}
		out[id] = pub
	}
	return out, nil
}

// normaliseRegistry sorts authorizations by node id and de-duplicates by node
// id (last wins) so the canonical bytes — and therefore the signature and
// digest — are deterministic regardless of how the file was assembled.
func normaliseRegistry(reg *apiv1.NodeRegistry) {
	if len(reg.Authorizations) == 0 {
		reg.Authorizations = []apiv1.NodeAuthorization{}
		return
	}
	seen := make(map[string]int, len(reg.Authorizations))
	out := make([]apiv1.NodeAuthorization, 0, len(reg.Authorizations))
	for _, e := range reg.Authorizations {
		if i, ok := seen[e.NodeID]; ok {
			out[i] = e
			continue
		}
		seen[e.NodeID] = len(out)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	reg.Authorizations = out
}
