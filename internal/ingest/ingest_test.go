package ingest_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/reallife-spark/goSpark/internal/ingest"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCrawlCollectsOnlyKnownExtensions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.md"), "hello markdown")
	writeFile(t, filepath.Join(root, "b.txt"), "hello text")
	writeFile(t, filepath.Join(root, "c.bin"), "should be skipped")
	writeFile(t, filepath.Join(root, "sub", "d.md"), "nested")

	chunks, err := ingest.Crawl(context.Background(), root, ingest.Options{})
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	sources := map[string]bool{}
	for _, c := range chunks {
		sources[c.Source] = true
	}
	if !sources["a.md"] || !sources["b.txt"] || !sources[filepath.ToSlash("sub/d.md")] {
		t.Fatalf("missing expected sources: %v", sources)
	}
	if sources["c.bin"] {
		t.Fatal("binary file should have been skipped")
	}
}

func TestCrawlPrunesSkipDirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep.md"), "keep me")
	writeFile(t, filepath.Join(root, ".git", "config.md"), "pruned")
	writeFile(t, filepath.Join(root, "node_modules", "x.md"), "pruned")

	chunks, err := ingest.Crawl(context.Background(), root, ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if c.Source == filepath.ToSlash(".git/config.md") || c.Source == filepath.ToSlash("node_modules/x.md") {
			t.Fatalf("pruned dir leaked: %s", c.Source)
		}
	}
}

func TestChunkingSplitsAndOverlaps(t *testing.T) {
	root := t.TempDir()
	// 100 bytes, chunk 30, overlap 10 => step 20.
	body := repeat("x", 100)
	writeFile(t, filepath.Join(root, "big.md"), body)

	chunks, err := ingest.Crawl(context.Background(), root, ingest.Options{ChunkBytes: 30, Overlap: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Fatalf("chunk %d has index %d", i, c.Index)
		}
		if c.Digest == "" {
			t.Fatal("chunk missing digest")
		}
	}
}

func TestEmptyFileYieldsNoChunks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "empty.md"), "   \n  \n")
	chunks, err := ingest.Crawl(context.Background(), root, ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 0 {
		t.Fatalf("expected no chunks for whitespace file, got %d", len(chunks))
	}
}

func TestDigestSet(t *testing.T) {
	chunks := []ingest.Chunk{{Digest: "a"}, {Digest: "b"}, {Digest: "a"}}
	set := ingest.DigestSet(chunks)
	if len(set) != 2 {
		t.Fatalf("digest set size = %d, want 2", len(set))
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
