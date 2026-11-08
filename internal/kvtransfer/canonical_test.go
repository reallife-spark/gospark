package kvtransfer

import (
	"encoding/binary"
	"testing"
)

func TestDecodeCanonicalRejectsHugeElementCountBeforeAllocating(t *testing.T) {
	raw := make([]byte, 12)
	binary.LittleEndian.PutUint32(raw[0:], ^uint32(0))
	// globalScale and packedLen remain zero.
	if _, err := DecodeCanonical(raw); err == nil {
		t.Fatal("expected a block-scale table bound error")
	}
}
