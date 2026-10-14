// Command sparknet-ingest is the knowledge-indexer driver: it crawls a vault,
// chunks it (internal/ingest) and, when an embedding endpoint is configured,
// turns those chunks into vectors through the compute-plane seam
// (internal/embed).
//
// It is pure orchestration: no tokenizer, no model, no CUDA. The vectors come
// from the compute plane; this binary keeps the local RAG pipeline fed while the
// GPU stays free for inference.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/embed"
	"github.com/reallife-spark/goSpark/internal/ingest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sparknet-ingest:", err)
		os.Exit(1)
	}
}

// index is the serialized knowledge index.
type index struct {
	Root     string            `json:"root"`
	Chunks   int               `json:"chunks"`
	Model    string            `json:"model,omitempty"`
	Dim      int               `json:"dim,omitempty"`
	Embedded bool              `json:"embedded"`
	Entries  []ingest.Embedded `json:"entries"`
}

func run() error {
	fs := flag.NewFlagSet("sparknet-ingest", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (YAML)")
	root := fs.String("root", "", "vault/repo root to crawl (defaults to config knowledge dir or /mnt/knowledge)")
	chunkBytes := fs.Int("chunk-bytes", 4096, "chunk size in bytes")
	overlap := fs.Int("overlap", 0, "bytes repeated between adjacent chunks")
	workers := fs.Int("workers", 4, "crawl concurrency")
	embedEndpoint := fs.String("embed-endpoint", "", "embeddings server base URL override")
	embedModel := fs.String("embed-model", "", "embedding model name override")
	embedSecret := fs.String("embed-secret", "", "shared secret override (X-Sparknet-Auth)")
	embedBatch := fs.Int("embed-batch", 0, "inputs per embeddings request override")
	out := fs.String("out", "", "write the index as JSON to this file instead of printing a summary")
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *embedEndpoint != "" {
		cfg.Embed.Endpoint = *embedEndpoint
	}
	if *embedModel != "" {
		cfg.Embed.Model = *embedModel
	}
	if *embedSecret != "" {
		cfg.Embed.Secret = *embedSecret
	}
	if *embedBatch > 0 {
		cfg.Embed.Batch = *embedBatch
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	walkRoot := *root
	if walkRoot == "" {
		walkRoot = os.Getenv("GOSPARK_KNOWLEDGE_DIR")
	}
	if walkRoot == "" {
		walkRoot = "/mnt/knowledge"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	chunks, err := ingest.Crawl(ctx, walkRoot, ingest.Options{
		ChunkBytes: *chunkBytes,
		Overlap:    *overlap,
		Workers:    *workers,
	})
	if err != nil {
		return err
	}

	idx := index{Root: walkRoot, Chunks: len(chunks), Model: cfg.Embed.Model}
	if cfg.Embed.Endpoint != "" {
		emb, err := embed.NewHTTPEmbedder(embed.Options{
			Endpoint: cfg.Embed.Endpoint,
			Secret:   cfg.Embed.Secret,
			Model:    cfg.Embed.Model,
			Batch:    cfg.Embed.Batch,
			Timeout:  cfg.Embed.Timeout,
		})
		if err != nil {
			return err
		}
		embedded, err := ingest.EmbedChunks(ctx, chunks, emb, ingest.EmbedOptions{Batch: cfg.Embed.Batch})
		if err != nil {
			return err
		}
		idx.Embedded = true
		idx.Entries = embedded
		for _, e := range embedded {
			if len(e.Vector) > idx.Dim {
				idx.Dim = len(e.Vector)
			}
		}
	} else {
		idx.Entries = make([]ingest.Embedded, len(chunks))
		for i, c := range chunks {
			idx.Entries[i] = ingest.Embedded{Chunk: c}
		}
	}

	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(idx); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "sparknet-ingest: wrote %d chunks (embedded=%t, dim %d) to %s\n",
			idx.Chunks, idx.Embedded, idx.Dim, *out)
		return nil
	}
	fmt.Printf("root:     %s\n", idx.Root)
	fmt.Printf("chunks:   %d\n", idx.Chunks)
	if idx.Embedded {
		fmt.Printf("embedded: yes (model %q, dim %d)\n", idx.Model, idx.Dim)
	} else {
		fmt.Printf("embedded: no (set embed.endpoint or -embed-endpoint to vectorize)\n")
	}
	return nil
}
