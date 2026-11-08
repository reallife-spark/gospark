// Package ingest prepares local knowledge for the compute plane without
// occupying it: it crawls a vault or git tree concurrently, chunks markdown and
// text, and emits stable digests for delta sync.
//
// It is deliberately I/O-bound Go work — the design's point is that Go keeps
// the local RAG pipeline fed while the GPU runtime stays free for inference.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Chunk is a unit of ingested text.
type Chunk struct {
	Source string `json:"source"` // path relative to the root
	Index  int    `json:"index"`  // ordinal within the source
	Text   string `json:"text"`
	Digest string `json:"digest"` // sha256 of Text
}

// Options configures a crawl.
type Options struct {
	// Extensions to ingest (with dot). Empty means the defaults.
	Extensions []string
	// ChunkBytes bounds a chunk's size.
	ChunkBytes int
	// Overlap is the number of bytes repeated between chunks.
	Overlap int
	// Workers bounds concurrency (0 => GOMAXPROCS-ish default of 4).
	Workers int
	// SkipDirs are directory names to prune.
	SkipDirs []string
}

func (o Options) withDefaults() Options {
	if len(o.Extensions) == 0 {
		o.Extensions = []string{".md", ".markdown", ".txt", ".go", ".py", ".json", ".yaml", ".yml"}
	}
	if o.ChunkBytes <= 0 {
		o.ChunkBytes = 4096
	}
	if o.Overlap < 0 {
		o.Overlap = 0
	}
	if o.Overlap >= o.ChunkBytes {
		o.Overlap = o.ChunkBytes / 8
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if len(o.SkipDirs) == 0 {
		o.SkipDirs = []string{".git", "node_modules", ".venv", "venv", "dist", ".cache"}
	}
	return o
}

// Crawl walks root and returns chunks. Files that vanish mid-walk are skipped;
// a file that cannot be read is recorded as an error but does not abort the
// crawl, because a half-ingested vault is still useful.
func Crawl(ctx context.Context, root string, opts Options) ([]Chunk, error) {
	o := opts.withDefaults()
	skip := make(map[string]struct{}, len(o.SkipDirs))
	for _, d := range o.SkipDirs {
		skip[d] = struct{}{}
	}
	exts := make(map[string]struct{}, len(o.Extensions))
	for _, e := range o.Extensions {
		exts[strings.ToLower(e)] = struct{}{}
	}

	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if _, bad := skip[d.Name()]; bad && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if _, ok := exts[strings.ToLower(filepath.Ext(path))]; !ok {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	jobs := make(chan string)
	results := make(chan []Chunk, len(files))
	errs := make(chan error, len(files))
	var wg sync.WaitGroup
	for i := 0; i < o.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				if ctx.Err() != nil {
					return
				}
				chunks, err := chunkFile(root, path, o)
				if err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						errs <- err
					}
					continue
				}
				results <- chunks
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, f := range files {
			select {
			case jobs <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(results)
	close(errs)

	var all []Chunk
	for cs := range results {
		all = append(all, cs...)
	}
	if ctx.Err() != nil {
		return all, ctx.Err()
	}
	return all, nil
}

func chunkFile(root, path string, o Options) ([]Chunk, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	relPath, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("ingest: %s escapes the crawl root", path)
	}
	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	text := string(data)
	var chunks []Chunk
	step := o.ChunkBytes - o.Overlap
	if step <= 0 {
		step = o.ChunkBytes
	}
	idx := 0
	for start := 0; start < len(text); start += step {
		end := start + o.ChunkBytes
		if end > len(text) {
			end = len(text)
		}
		piece := text[start:end]
		if strings.TrimSpace(piece) == "" {
			if end >= len(text) {
				break
			}
			continue
		}
		chunks = append(chunks, Chunk{
			Source: filepath.ToSlash(rel),
			Index:  idx,
			Text:   piece,
			Digest: digest(piece),
		})
		idx++
		if end >= len(text) {
			break
		}
	}
	return chunks, nil
}

// DigestSet returns the set of chunk digests for delta comparison.
func DigestSet(chunks []Chunk) map[string]struct{} {
	m := make(map[string]struct{}, len(chunks))
	for _, c := range chunks {
		m[c.Digest] = struct{}{}
	}
	return m
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
