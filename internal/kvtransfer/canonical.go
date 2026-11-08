package kvtransfer

import (
	"encoding/binary"
	"fmt"
	"math"
)

// DecodeCanonical reconstructs a Block from the byte form produced by
// Block.Canonical. It is the inverse the transport server uses on receipt.
func DecodeCanonical(buf []byte) (*Block, error) {
	if len(buf) < 12 {
		return nil, fmt.Errorf("kvtransfer: canonical too short (%d bytes)", len(buf))
	}
	p := 0
	numElements := int(binary.LittleEndian.Uint32(buf[p:]))
	p += 4
	globalScale := math.Float32frombits(binary.LittleEndian.Uint32(buf[p:]))
	p += 4
	packedLen := int(binary.LittleEndian.Uint32(buf[p:]))
	p += 4
	if numElements <= 0 {
		return nil, ErrEmpty
	}
	nblocks := (numElements + BlockSize - 1) / BlockSize
	if nblocks > (len(buf)-12)/4 {
		return nil, fmt.Errorf("kvtransfer: block scale table exceeds payload")
	}
	scales := make([]float32, nblocks)
	for i := 0; i < nblocks; i++ {
		if p+4 > len(buf) {
			return nil, fmt.Errorf("kvtransfer: truncated block scales")
		}
		scales[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[p:]))
		p += 4
	}
	if p+packedLen > len(buf) {
		return nil, fmt.Errorf("kvtransfer: truncated packed payload")
	}
	packed := make([]byte, packedLen)
	copy(packed, buf[p:p+packedLen])
	return &Block{
		NumElements: numElements,
		GlobalScale: globalScale,
		BlockScale:  scales,
		Packed:      packed,
	}, nil
}
