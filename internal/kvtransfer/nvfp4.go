// Package kvtransfer implements the goSpark NVFP4 KV-cache transfer.
//
// It is the *reference* codec for the wire format: a faithful E2M1 4-bit value
// packing with a per-16-element block scale and a tensor-level global scale.
// It is deliberately CPU code and not a CUDA kernel — the production path hands
// the packed blocks to the GPU runtime, which is the Python/CUDA compute plane.
//
// The transport (internal/transport) streams these blocks over HTTP/3.
package kvtransfer

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
)

// BlockSize is the NVFP4 micro-block width.
const BlockSize = 16

// DType is the canonical dtype tag on the wire.
const DType = "nvfp4-e2m1"

// e2m1 magnitudes indexed by the 3 magnitude bits (sign is the 4th bit).
var e2m1Magnitudes = [8]float32{0, 0.5, 1, 1.5, 2, 3, 4, 6}

// Block is a compressed tensor slice.
type Block struct {
	ID          string
	NumElements int
	GlobalScale float32
	BlockScale  []float32 // one scale per 16 elements
	Packed      []byte    // 2 E2M1 codes per byte
}

// Errors.
var (
	ErrEmpty      = errors.New("kvtransfer: empty input")
	ErrBadLength  = errors.New("kvtransfer: packed length does not match element count")
	ErrBlockScale = errors.New("kvtransfer: block scale count does not match element count")
)

// Codec is the encode/decode surface, named so a hardware codec can replace the
// reference one without touching callers.
type Codec interface {
	Encode(values []float32) (*Block, error)
	Decode(b *Block) ([]float32, error)
}

// ReferenceCodec is the pure-Go NVFP4 codec.
type ReferenceCodec struct{}

// Encode quantises values into an NVFP4 block.
func (ReferenceCodec) Encode(values []float32) (*Block, error) {
	if len(values) == 0 {
		return nil, ErrEmpty
	}
	nblocks := (len(values) + BlockSize - 1) / BlockSize
	scales := make([]float32, nblocks)
	packed := make([]byte, 0, (len(values)+1)/2)
	var globalMax float32

	var cur byte
	for i, v := range values {
		block := i / BlockSize
		if i%BlockSize == 0 {
			// Determine this block's max magnitude.
			end := (block + 1) * BlockSize
			if end > len(values) {
				end = len(values)
			}
			var m float32
			for _, x := range values[block*BlockSize : end] {
				if a := absf(x); a > m {
					m = a
				}
			}
			scale := m / e2m1Magnitudes[7] // largest representable is 6.0
			if scale == 0 {
				scale = 1
			}
			scales[block] = scale
			if scale > globalMax {
				globalMax = scale
			}
		}
		code := quantizeE2M1(v / scales[block])
		nibble := byte(code & 0x0F)
		if i%2 == 0 {
			cur = nibble
			if i == len(values)-1 {
				packed = append(packed, cur)
			}
		} else {
			packed = append(packed, cur|(nibble<<4))
		}
	}

	return &Block{
		NumElements: len(values),
		GlobalScale: globalMax,
		BlockScale:  scales,
		Packed:      packed,
	}, nil
}

// Decode reconstructs the float values from an NVFP4 block.
func (ReferenceCodec) Decode(b *Block) ([]float32, error) {
	if b == nil || b.NumElements == 0 {
		return nil, ErrEmpty
	}
	if want := (b.NumElements + 1) / 2; len(b.Packed) != want {
		return nil, fmt.Errorf("%w: have %d want %d", ErrBadLength, len(b.Packed), want)
	}
	if want := (b.NumElements + BlockSize - 1) / BlockSize; len(b.BlockScale) != want {
		return nil, fmt.Errorf("%w: have %d want %d", ErrBlockScale, len(b.BlockScale), want)
	}
	out := make([]float32, b.NumElements)
	for i := range out {
		byteIdx := i / 2
		var code byte
		if i%2 == 0 {
			code = b.Packed[byteIdx] & 0x0F
		} else {
			code = (b.Packed[byteIdx] >> 4) & 0x0F
		}
		out[i] = dequantizeE2M1(code) * b.BlockScale[i/BlockSize]
	}
	return out, nil
}

// Digest is the sha256 hex of the block's canonical payload. Two blocks with the
// same digest carry the same bytes, which is what makes delta sync sound.
func (b *Block) Digest() string {
	return hex.EncodeToString(sha256Bytes(b.Canonical()))
}

// Canonical serialises the block deterministically for hashing and transport.
func (b *Block) Canonical() []byte {
	buf := make([]byte, 0, 24+len(b.BlockScale)*4+len(b.Packed))
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], uint32(b.NumElements))
	buf = append(buf, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], math.Float32bits(b.GlobalScale))
	buf = append(buf, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], uint32(len(b.Packed)))
	buf = append(buf, tmp[:]...)
	for _, s := range b.BlockScale {
		binary.LittleEndian.PutUint32(tmp[:], math.Float32bits(s))
		buf = append(buf, tmp[:]...)
	}
	buf = append(buf, b.Packed...)
	return buf
}

func quantizeE2M1(x float32) byte {
	neg := x < 0
	m := absf(x)
	best := 0
	bestErr := float32(math.MaxFloat32)
	for i, c := range e2m1Magnitudes {
		e := absf(m - c)
		if e < bestErr {
			bestErr = e
			best = i
		}
	}
	code := byte(best)
	if neg {
		code |= 0x08
	}
	return code
}

func dequantizeE2M1(code byte) float32 {
	mag := e2m1Magnitudes[code&0x07]
	if code&0x08 != 0 {
		return -mag
	}
	return mag
}

func absf(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}
