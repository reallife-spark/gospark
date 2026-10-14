// Package identity implements the goSpark node identity as the fabric defines
// it: a projection of five fields whose sha256 fingerprint names the node, plus
// an Ed25519 keypair goSpark uses to sign and verify envelopes.
//
// The projection and its fingerprint are byte-for-byte the fabric's
// (contracts/identity-schema.json, `identity_fields` + `fingerprint`): the
// fields are node_id, role, wireguard_public_key_b64, overlay_ipv4 and
// requested_capabilities, canonicalised as JSON with sorted keys and tight
// separators, then hashed with sha256. The Ed25519 public key is carried in the
// `wireguard_public_key_b64` slot — the field the contract anchors identity to —
// so the fingerprint binds the node to the key the control plane later verifies
// results against.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reallife-spark/goSpark/internal/apiv1"
)

// Roles are the fabric's two node roles.
const (
	RoleMaster = "master"
	RolePeer   = "peer"
)

// Identity is a node's signing identity. Private is nil for verify-only nodes.
type Identity struct {
	NodeID                string
	Role                  string
	OverlayIPv4           string
	RequestedCapabilities []string
	Public                ed25519.PublicKey
	Private               ed25519.PrivateKey
}

// Generate creates a fresh identity for nodeID in the given role. An empty role
// defaults to "peer", the working-node role.
func Generate(nodeID, role string) (*Identity, error) {
	if strings.TrimSpace(nodeID) == "" {
		return nil, errors.New("identity: node_id is required")
	}
	if role == "" {
		role = RolePeer
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate: %w", err)
	}
	return &Identity{NodeID: nodeID, Role: role, Public: pub, Private: priv}, nil
}

// PublicKeyB64 returns the base64 Ed25519 public key.
func (id *Identity) PublicKeyB64() string {
	return base64.StdEncoding.EncodeToString(id.Public)
}

// Projection returns the five fingerprinted identity fields. The Ed25519 public
// key is placed in wireguard_public_key_b64 (see the package doc).
func (id *Identity) Projection() apiv1.NodeIdentity {
	return apiv1.NodeIdentity{
		NodeID:                id.NodeID,
		Role:                  id.Role,
		WireGuardPublicKeyB64: id.PublicKeyB64(),
		OverlayIPv4:           id.OverlayIPv4,
		RequestedCapabilities: nonNil(id.RequestedCapabilities),
	}
}

// Fingerprint is the hex sha256 of the canonical identity projection.
func (id *Identity) Fingerprint() string {
	fp, err := FingerprintOf(id.Projection())
	if err != nil {
		// CanonicalJSON only fails on unserialisable input; the projection is
		// always serialisable, so this is unreachable.
		panic("identity: fingerprint: " + err.Error())
	}
	return fp
}

// FingerprintOf computes the fabric fingerprint for a projected identity.
// RequestedCapabilities is normalised to a list (never JSON null) so an entry
// read from disk fingerprints the same as one built in memory.
func FingerprintOf(ni apiv1.NodeIdentity) (string, error) {
	ni.RequestedCapabilities = nonNil(ni.RequestedCapabilities)
	canonical, err := apiv1.CanonicalJSON(ni)
	if err != nil {
		return "", fmt.Errorf("identity: canonicalise projection: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Sign signs msg with the node identity key.
func (id *Identity) Sign(msg []byte) ([]byte, error) {
	if id.Private == nil {
		return nil, errors.New("identity: no private key (verify-only node)")
	}
	return ed25519.Sign(id.Private, msg), nil
}

// Verify checks an Ed25519 signature against pub.
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

// ParsePublicKeyB64 decodes a base64 Ed25519 public key.
func ParsePublicKeyB64(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("identity: decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("identity: public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// EncodeSignature / DecodeSignature are base64 helpers for envelope fields.
func EncodeSignature(sig []byte) string { return base64.StdEncoding.EncodeToString(sig) }

func DecodeSignature(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("identity: decode signature: %w", err)
	}
	return raw, nil
}

// KeyFile is the on-disk JSON form. The private half is raw ed25519 key bytes
// base64 encoded; the file must be 0600.
type KeyFile struct {
	NodeID                string   `json:"node_id"`
	Role                  string   `json:"role"`
	OverlayIPv4           string   `json:"overlay_ipv4,omitempty"`
	RequestedCapabilities []string `json:"requested_capabilities,omitempty"`
	PublicKey             string   `json:"public_key_b64"`
	PrivateKey            string   `json:"private_key_b64,omitempty"`
	Fingerprint           string   `json:"fingerprint"`
}

// Save writes id to path with 0600 permissions (private half included).
func (id *Identity) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("identity: mkdir: %w", err)
	}
	kf := KeyFile{
		NodeID:                id.NodeID,
		Role:                  id.Role,
		OverlayIPv4:           id.OverlayIPv4,
		RequestedCapabilities: id.RequestedCapabilities,
		PublicKey:             id.PublicKeyB64(),
		Fingerprint:           id.Fingerprint(),
	}
	if id.Private != nil {
		kf.PrivateKey = base64.StdEncoding.EncodeToString(id.Private)
	}
	data, err := marshalIndent(kf)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Load reads a key file. A verify-only node may omit the private key.
func Load(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("identity: read %s: %w", path, err)
	}
	kf := KeyFile{}
	if err := unmarshalJSON(data, &kf); err != nil {
		return nil, fmt.Errorf("identity: parse %s: %w", path, err)
	}
	pub, err := ParsePublicKeyB64(kf.PublicKey)
	if err != nil {
		return nil, err
	}
	id := &Identity{
		NodeID:                kf.NodeID,
		Role:                  kf.Role,
		OverlayIPv4:           kf.OverlayIPv4,
		RequestedCapabilities: kf.RequestedCapabilities,
		Public:                pub,
	}
	if kf.PrivateKey != "" {
		raw, err := base64.StdEncoding.DecodeString(kf.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("identity: decode private key: %w", err)
		}
		if len(raw) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("identity: private key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
		}
		id.Private = ed25519.PrivateKey(raw)
	}
	return id, nil
}
