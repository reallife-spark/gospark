package apiv1

import (
	"bytes"
	"testing"
)

func TestCanonicalJSONKeyOrderStable(t *testing.T) {
	a := map[string]any{"b": 2, "a": 1, "c": 3}
	b := map[string]any{"c": 3, "a": 1, "b": 2}
	ba, err := CanonicalJSON(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := CanonicalJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ba, bb) {
		t.Fatalf("canonical differs by key order: %s vs %s", ba, bb)
	}
	if !bytes.Equal(ba, []byte(`{"a":1,"b":2,"c":3}`)) {
		t.Fatalf("canonical bytes = %s", ba)
	}
}

func TestCanonicalBytesBlanksSignature(t *testing.T) {
	env := &TaskEnvelope{TaskID: "t", SignatureB64: "some-signature"}
	raw, err := CanonicalBytes(env)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("some-signature")) {
		t.Fatalf("signature leaked into canonical bytes: %s", raw)
	}
	// The original must be untouched.
	if env.SignatureB64 != "some-signature" {
		t.Fatal("CanonicalBytes mutated its input")
	}
}

func TestCanonicalBytesDiffersWhenSignedFieldChanges(t *testing.T) {
	a := &TaskEnvelope{TaskID: "t1"}
	b := &TaskEnvelope{TaskID: "t2"}
	ba, _ := CanonicalBytes(a)
	bb, _ := CanonicalBytes(b)
	if bytes.Equal(ba, bb) {
		t.Fatal("different task ids must canonicalise differently")
	}
}

func TestCanonicalBytesRejectsUnknownEnvelope(t *testing.T) {
	if _, err := CanonicalBytes(struct{ X int }{1}); err == nil {
		t.Fatal("expected error for an unsupported envelope type")
	}
}

func TestEqualCanonical(t *testing.T) {
	a := &ResultEnvelope{TaskID: "x", Result: "1"}
	b := &ResultEnvelope{TaskID: "x", Result: "1"}
	eq, err := EqualCanonical(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !eq {
		t.Fatal("equal envelopes must canonicalise equally")
	}
}
