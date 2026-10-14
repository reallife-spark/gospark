package ingest_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/reallife-spark/goSpark/internal/ingest"
)

// fakeEmbedder records the batches it is called with and returns a vector per
// input whose first element is the input's global position.
type fakeEmbedder struct {
	mu      sync.Mutex
	batches [][]string
	count   int
	dim     int
	failAt  int
	calls   int
}

func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, append([]string(nil), inputs...))
	call := f.calls
	f.calls++
	if f.failAt > 0 && call == f.failAt {
		return nil, fmt.Errorf("embedding upstream down")
	}
	vecs := make([][]float32, len(inputs))
	for i := range inputs {
		vec := make([]float32, f.dim)
		vec[0] = float32(f.count + i + 1)
		vecs[i] = vec
	}
	f.count += len(inputs)
	return vecs, nil
}

func (f *fakeEmbedder) batchSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(f.batches))
	for i, b := range f.batches {
		out[i] = len(b)
	}
	return out
}

func chunks(n int) []ingest.Chunk {
	out := make([]ingest.Chunk, n)
	for i := range out {
		out[i] = ingest.Chunk{Source: "f.md", Index: i, Text: fmt.Sprintf("t%d", i), Digest: fmt.Sprintf("d%d", i)}
	}
	return out
}

func TestEmbedChunksPairsInOrder(t *testing.T) {
	e := &fakeEmbedder{dim: 3}
	out, err := ingest.EmbedChunks(context.Background(), chunks(4), e, ingest.EmbedOptions{Batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("got %d embeddings", len(out))
	}
	for i, ec := range out {
		if ec.Chunk.Index != i {
			t.Errorf("chunk %d out of order: %+v", i, ec.Chunk)
		}
		if len(ec.Vector) != 3 || ec.Vector[0] != float32(i+1) {
			t.Errorf("vector %d mis-paired: %v", i, ec.Vector)
		}
	}
	if got := e.batchSizes(); len(got) != 2 || got[0] != 2 || got[1] != 2 {
		t.Errorf("batch sizes = %v, want [2 2]", got)
	}
}

func TestEmbedChunksSingleBatchByDefault(t *testing.T) {
	e := &fakeEmbedder{dim: 1}
	if _, err := ingest.EmbedChunks(context.Background(), chunks(5), e, ingest.EmbedOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := e.batchSizes(); len(got) != 1 || got[0] != 5 {
		t.Errorf("batch sizes = %v, want [5]", got)
	}
}

func TestEmbedChunksPropagatesError(t *testing.T) {
	e := &fakeEmbedder{dim: 1, failAt: 2}
	_, err := ingest.EmbedChunks(context.Background(), chunks(4), e, ingest.EmbedOptions{Batch: 1})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestEmbedChunksNilEmbedder(t *testing.T) {
	if _, err := ingest.EmbedChunks(context.Background(), chunks(1), nil, ingest.EmbedOptions{}); err == nil {
		t.Fatal("expected error for nil embedder")
	}
}
