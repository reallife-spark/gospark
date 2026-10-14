// Package identity implements the goSpark node identity: an Ed25519 keypair
// whose fingerprint is the sha256 of a canonical identity string.
//
// The fingerprint is deliberately derived from (node_id, owner_id, public key)
// and not from the key alone: it is the value the fabric's signed envelopes
// carry, so it must name *the node*, not just the key material.
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
)

// Identity is a node's signing identity. Private is nil for verify-only nodes.
type Identity struct {
	NodeID  string
	OwnerID string
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

// Generate creates a fresh identity for nodeID/ownerID.
func Generate(nodeID, ownerID string) (*Identity, error) {
	if strings.TrimSpace(nodeID) == "" {
		return nil, errors.New("identity: node_id is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate: %w", err)
	}
	return &Identity{NodeID: nodeID, OwnerID: ownerID, Public: pub, Private: priv}, nil
}

// Fingerprint is the hex sha256 of the canonical identity string.
func (id *Identity) Fingerprint() string {
	return FingerprintOf(id.NodeID, id.OwnerID, id.Public)
}

// FingerprintOf computes the canonical fingerprint for the given fields.
func FingerprintOf(nodeID, ownerID string, pub ed25519.PublicKey) string {
	var b strings.Builder
	b.WriteString("sparknet.identity.v1\n")
	b.WriteString("node_id=" + nodeID + "\n")
	b.WriteString("owner_id=" + ownerID + "\n")
	b.WriteString("public_key_b64=" + base64.StdEncoding.EncodeToString(pub) + "\n")
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
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

// PublicKeyB64 returns the base64 public key.
func (id *Identity) PublicKeyB64() string {
	return base64.StdEncoding.EncodeToString(id.Public)
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

// KeyFile is the on-disk JSON form. The private half is base64 PKCS8-style raw
// seed+key as produced by ed25519; the file must be 0600.
type KeyFile struct {
	NodeID      string `json:"node_id"`
	OwnerID     string `json:"owner_id"`
	PublicKey   string `json:"public_key_b64"`
	PrivateKey  string `json:"private_key_b64,omitempty"`
	Fingerprint string `json:"fingerprint"`
}

// Save writes id to path with 0600 permissions (private half included).
func (id *Identity) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("identity: mkdir: %w", err)
	}
	kf := KeyFile{
		NodeID:      id.NodeID,
		OwnerID:     id.OwnerID,
		PublicKey:   id.PublicKeyB64(),
		Fingerprint: id.Fingerprint(),
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
	id := &Identity{NodeID: kf.NodeID, OwnerID: kf.OwnerID, Public: pub}
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
