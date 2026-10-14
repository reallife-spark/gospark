package apiv1

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// CanonicalJSON renders v with object keys sorted and insignificant whitespace
// removed, so two processes signing "the same" message produce the same bytes.
//
// The implementation normalises through encoding/json twice: the second marshal
// emits map keys in sorted order, which is what makes the output stable
// regardless of struct field order. Fields tagged omitempty disappear exactly
// as the first marshal intended.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical: marshal: %w", err)
	}
	var norm any
	if err := json.Unmarshal(raw, &norm); err != nil {
		return nil, fmt.Errorf("canonical: normalise: %w", err)
	}
	out, err := json.Marshal(norm)
	if err != nil {
		return nil, fmt.Errorf("canonical: re-marshal: %w", err)
	}
	return out, nil
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
	case *NodeRegistry:
		c := *e
		c.SignatureB64 = ""
		return c, nil
	case NodeRegistry:
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
