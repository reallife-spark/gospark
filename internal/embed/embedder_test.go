package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/embed"
)

// embedServer is a fake OpenAI-compatible embeddings endpoint that records the
// batches it receives.
type embedServer struct {
	mu       sync.Mutex
	requests [][]string
	dim      int
	// reorder makes the server shift result indexes to exercise index mapping.
	reorder bool
	// short returns one fewer vector than requested.
	short bool
	// secret, when set, is required in the auth header.
	secret string
	status int
}

func (s *embedServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if s.secret != "" && r.Header.Get(embed.HeaderAuth) != s.secret {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "bad token"}})
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, req.Input)
		s.mu.Unlock()
		if s.status != 0 {
			w.WriteHeader(s.status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "engine boom"}})
			return
		}
		data := make([]map[string]any, 0, len(req.Input))
		for i := range req.Input {
			if s.short && i == len(req.Input)-1 {
				continue
			}
			idx := i
			if s.reorder {
				idx = len(req.Input) - 1 - i
			}
			vec := make([]float32, s.dim)
			vec[0] = float32(idx + 1)
			data = append(data, map[string]any{"object": "embedding", "index": idx, "embedding": vec})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": req.Model})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if s.secret != "" && r.Header.Get(embed.HeaderAuth) != s.secret {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{}})
	})
	return mux
}

func newTestEmbedder(t *testing.T, s *embedServer, opts embed.Options) *embed.HTTPEmbedder {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	opts.Endpoint = srv.URL
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}
	e, err := embed.NewHTTPEmbedder(opts)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEmbedHappyPath(t *testing.T) {
	s := &embedServer{dim: 4}
	e := newTestEmbedder(t, s, embed.Options{Model: "m"})
	inputs := []string{"a", "b", "c"}
	vecs, err := e.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 4 {
			t.Errorf("vector %d dim = %d", i, len(v))
		}
		if v[0] != float32(i+1) {
			t.Errorf("vector %d out of order: %v", i, v[0])
		}
	}
}

func TestEmbedBatches(t *testing.T) {
	s := &embedServer{dim: 2}
	e := newTestEmbedder(t, s, embed.Options{Batch: 2})
	if _, err := e.Embed(context.Background(), []string{"1", "2", "3", "4", "5"}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(s.requests))
	}
	if len(s.requests[0]) != 2 || len(s.requests[2]) != 1 {
		t.Errorf("batch sizes = %d,%d,%d", len(s.requests[0]), len(s.requests[1]), len(s.requests[2]))
	}
}

func TestEmbedReorderedResults(t *testing.T) {
	s := &embedServer{dim: 2, reorder: true}
	e := newTestEmbedder(t, s, embed.Options{})
	vecs, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vecs {
		if v[0] != float32(i+1) {
			t.Errorf("vector %d mis-ordered: %v", i, v[0])
		}
	}
}

func TestEmbedSendsAuth(t *testing.T) {
	s := &embedServer{dim: 1, secret: "s3cr3t"}
	e := newTestEmbedder(t, s, embed.Options{Secret: "s3cr3t"})
	if _, err := e.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
}

func TestEmbedRejectsShortResult(t *testing.T) {
	s := &embedServer{dim: 1, short: true}
	e := newTestEmbedder(t, s, embed.Options{})
	if _, err := e.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("expected error for short result")
	}
}

func TestEmbedErrorStatus(t *testing.T) {
	s := &embedServer{dim: 1, status: http.StatusInternalServerError}
	e := newTestEmbedder(t, s, embed.Options{})
	_, err := e.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); !strings.Contains(got, "engine boom") {
		t.Errorf("error should carry server message: %v", got)
	}
}

func TestHealth(t *testing.T) {
	s := &embedServer{dim: 1, secret: "s"}
	e := newTestEmbedder(t, s, embed.Options{Secret: "s"})
	if err := e.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewHTTPEmbedderRequiresEndpoint(t *testing.T) {
	if _, err := embed.NewHTTPEmbedder(embed.Options{}); err == nil {
		t.Fatal("expected error for empty endpoint")
	}
}
