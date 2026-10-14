package apiv1

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// CanonicalJSON renders v with object keys sorted and insignificant whitespace
// removed, so two processes signing "the same" message produce the same bytes.
//
// The bytes are deliberately identical to Python's
// json.dumps(value, sort_keys=True, separators=(",", ":")) — the fabric's
// canonicalisation (contracts/identity-schema.json, `canonicalisation`). To
// match it this normalises through encoding/json twice (the second marshal
// emits map keys in sorted order) and disables HTML escaping, which Python's
// json never applies but Go's encoding/json does by default (`<`, `>`, `&`
// would otherwise sign differently on the two implementations).
//
// One caveat: Python's json.dumps defaults to ensure_ascii=True and so escapes
// non-ASCII runes as \uXXXX, while Go emits raw UTF-8. The bytes therefore
// agree only for ASCII input. The fabric's signed vocabulary — node_id, role,
// capability names, base64 keys and ISO-8601 stamps — is ASCII by construction,
// so this matches in practice; a signed field that could carry arbitrary
// Unicode would need a Python-compatible escaper here.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical: marshal: %w", err)
	}
	var norm any
	if err := json.Unmarshal(raw, &norm); err != nil {
		return nil, fmt.Errorf("canonical: normalise: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(norm); err != nil {
		return nil, fmt.Errorf("canonical: re-marshal: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// CanonicalBytes returns the canonical JSON for v with the signature field
// blanked, which is the exact byte string a signer signs and a verifier checks.
//
// It is generic over the signed envelope types so callers never hand-roll the
// "clear the signature, marshal, sign" dance and never diverge from the
// verifier.
func CanonicalBytes(env any) ([]byte, error) {
	clone, err := clearSignature(env)
	if err != nil {
		return nil, err
	}
	return CanonicalJSON(clone)
}

func clearSignature(env any) (any, error) {
	switch e := env.(type) {
	case *TaskEnvelope:
		c := *e
		c.SignatureB64 = ""
		return c, nil
	case TaskEnvelope:
		c := e
		c.SignatureB64 = ""
		return c, nil
	case *ResultEnvelope:
		c := *e
		c.SignatureB64 = ""
		return c, nil
	case ResultEnvelope:
		c := e
		c.SignatureB64 = ""
		return c, nil
	case *ClusterJob:
		c := *e
		c.SignatureB64 = ""
		c.RatificationB64 = ""
		return c, nil
	case ClusterJob:
		c := e
		c.SignatureB64 = ""
		c.RatificationB64 = ""
		return c, nil
	case *ClusterResult:
		c := *e
		c.SignatureB64 = ""
		return c, nil
	case ClusterResult:
		c := e
		c.SignatureB64 = ""
		return c, nil
	default:
		return nil, fmt.Errorf("canonical: unsupported envelope %T", env)
	}
}

// EqualCanonical reports whether two values canonicalise to identical bytes.
func EqualCanonical(a, b any) (bool, error) {
	ba, err := CanonicalJSON(a)
	if err != nil {
		return false, err
	}
	bb, err := CanonicalJSON(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ba, bb), nil
}
