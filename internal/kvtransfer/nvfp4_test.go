package kvtransfer

import (
	"bytes"
	"math"
	"testing"
)

func TestEncodeDecodeRoundTripWithinTolerance(t *testing.T) {
	codec := ReferenceCodec{}
	values := []float32{
		0, 0.5, -0.5, 1, -1, 1.5, -1.5, 2, -2, 3, -3, 4, -4, 6, -6, 0.25,
	}
	b, err := codec.Encode(values)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := codec.Decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(values) {
		t.Fatalf("decoded %d values, want %d", len(got), len(values))
	}
	// Per-block scale is max/6; the smallest representable step is 0.5*scale.
	// For this block the block max is 6, so scale is 1 and error must be <= 0.25.
	for i, want := range values {
		if err := math.Abs(float64(got[i] - want)); err > 0.25 {
			t.Errorf("value[%d]: got %v want %v (err %v)", i, got[i], want, err)
		}
	}
}

func TestEncodeEmptyRefused(t *testing.T) {
	if _, err := (ReferenceCodec{}).Encode(nil); err != ErrEmpty {
		t.Fatalf("expected ErrEmpty, got %v", err)
	}
}

func TestPackedLength(t *testing.T) {
	// 16 elements => 8 packed bytes, 1 block scale.
	b, err := (ReferenceCodec{}).Encode(make([]float32, 16))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Packed) != 8 {
		t.Fatalf("packed len = %d, want 8", len(b.Packed))
	}
	if len(b.BlockScale) != 1 {
		t.Fatalf("block scales = %d, want 1", len(b.BlockScale))
	}
	// 17 elements => 9 packed bytes, 2 block scales.
	b2, err := (ReferenceCodec{}).Encode(make([]float32, 17))
	if err != nil {
		t.Fatal(err)
	}
	if len(b2.Packed) != 9 {
		t.Fatalf("packed len = %d, want 9", len(b2.Packed))
	}
	if len(b2.BlockScale) != 2 {
		t.Fatalf("block scales = %d, want 2", len(b2.BlockScale))
	}
}

func TestCanonicalRoundTrip(t *testing.T) {
	codec := ReferenceCodec{}
	values := []float32{1, -2, 3, -4.5, 0, 6, -0.5, 0.25, 2, 1.5}
	b, err := codec.Encode(values)
	if err != nil {
		t.Fatal(err)
	}
	raw := b.Canonical()
	got, err := DecodeCanonical(raw)
	if err != nil {
		t.Fatalf("decode canonical: %v", err)
	}
	if got.NumElements != b.NumElements || got.GlobalScale != b.GlobalScale {
		t.Fatalf("header mismatch: %+v vs %+v", got, b)
	}
	if !bytes.Equal(got.Packed, b.Packed) {
		t.Fatal("packed bytes differ after round trip")
	}
	if got.Digest() != b.Digest() {
		t.Fatal("digest differs after round trip")
	}
}

func TestDecodeCanonicalRejectsTruncation(t *testing.T) {
	b, _ := (ReferenceCodec{}).Encode([]float32{1, 2, 3, 4})
	raw := b.Canonical()
	if _, err := DecodeCanonical(raw[:len(raw)-1]); err == nil {
		t.Fatal("expected error on truncated payload")
	}
	if _, err := DecodeCanonical(raw[:8]); err == nil {
		t.Fatal("expected error on header-only truncation")
	}
}

func TestDecodeReturnsBadLength(t *testing.T) {
	b := &Block{NumElements: 4, Packed: []byte{0x01}} // needs 2 packed bytes
	if _, err := (ReferenceCodec{}).Decode(b); err == nil {
		t.Fatal("expected ErrBadLength")
	}
}

func TestDigestStableAcrossInstances(t *testing.T) {
	codec := ReferenceCodec{}
	a, _ := codec.Encode([]float32{1, 2, 3})
	b, _ := codec.Encode([]float32{1, 2, 3})
	if a.Digest() != b.Digest() {
		t.Fatal("identical inputs must produce identical digests")
	}
	c, _ := codec.Encode([]float32{1, 2, 4})
	if a.Digest() == c.Digest() {
		t.Fatal("different inputs must not collide")
	}
}

func TestDeltaCachePutAndDiff(t *testing.T) {
	cache := NewDeltaCache()
	codec := ReferenceCodec{}
	b1, _ := codec.Encode([]float32{1, 2, 3})
	b2, _ := codec.Encode([]float32{4, 5, 6})
	const ns = "conv-1"
	d1 := cache.Put(ns, b1)
	d2 := cache.Put(ns, b2)

	if !cache.Have(ns, d1) {
		t.Fatalf("cache should have %s", d1)
	}
	if got, ok := cache.Get(ns, d1); !ok || got.Digest() != d1 {
		t.Fatalf("get %s = %v,%v", d1, got, ok)
	}
	missing := cache.Missing(ns, []string{d1, d2, "deadbeef"})
	if len(missing) != 1 || missing[0] != "deadbeef" {
		t.Fatalf("missing = %v, want [deadbeef]", missing)
	}
	if cache.Len(ns) != 2 {
		t.Fatalf("len = %d, want 2", cache.Len(ns))
	}
	// A peer that already has d1 should be sent only b2.
	send := cache.ResolveDelta(ns, []string{d1})
	if len(send) != 1 || send[0].Digest() != d2 {
		t.Fatalf("resolve delta = %v, want [%s]", send, d2)
	}
}
