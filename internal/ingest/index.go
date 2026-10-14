package ingest

import (
	"context"
	"fmt"
)

// Embedder is the seam to the compute plane: it turns texts into vectors. The
// interface lives here so ingest stays free of any model dependency; concrete
// embedders live in internal/embed.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// Embedded pairs a chunk with its vector.
type Embedded struct {
	Chunk
	Vector []float32 `json:"vector,omitempty"`
}

// EmbedOptions tunes the embedding pass.
type EmbedOptions struct {
	// Batch is the number of chunks sent per call. Zero uses the embedder's
	// own batching (a single call over all chunks).
	Batch int
}

// EmbedChunks runs the crawled chunks through the embedder and pairs each chunk
// with its vector, preserving chunk order. A short or over-long result is an
// error: a knowledge index with silently shifted vectors is worse than none.
func EmbedChunks(ctx context.Context, chunks []Chunk, e Embedder, opts EmbedOptions) ([]Embedded, error) {
	if e == nil {
		return nil, fmt.Errorf("ingest: embedder is nil")
	}
	out := make([]Embedded, 0, len(chunks))
	step := opts.Batch
	if step <= 0 {
		step = len(chunks)
	}
	for start := 0; start < len(chunks); start += step {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		end := start + step
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[start:end]
		texts := make([]string, len(batch))
		for i, c := range batch {
			texts[i] = c.Text
		}
		vecs, err := e.Embed(ctx, texts)
		if err != nil {
			return out, fmt.Errorf("ingest: embed chunks %d..%d: %w", start, end-1, err)
		}
		if len(vecs) != len(batch) {
			return out, fmt.Errorf("ingest: embedder returned %d vectors for %d chunks", len(vecs), len(batch))
		}
		for i, c := range batch {
			out = append(out, Embedded{Chunk: c, Vector: vecs[i]})
		}
	}
	return out, nil
}
