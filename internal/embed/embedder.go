// Package embed is the seam between Go's knowledge-ingest pipeline and the
// compute plane's embedding model.
//
// The Go side crawls a vault and chunks it (internal/ingest); turning text into
// vectors is model work, so it happens beside the GPU. This package is a small
// OpenAI-compatible client — it links no model, no tokenizer and no CUDA, and it
// only moves text in and vectors out.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Embedder turns a batch of texts into vectors, preserving input order.
type Embedder interface {
	Name() string
	// Embed returns one vector per input, in the same order.
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	Health(ctx context.Context) error
}

// HeaderAuth is the shared-secret header the compute plane uses.
const HeaderAuth = "X-Sparknet-Auth"

const (
	embeddingsPath = "/v1/embeddings"
	modelsPath     = "/v1/models"
)

// DefaultBatch caps how many inputs go in one request.
const DefaultBatch = 32

// DefaultTimeout bounds a single embeddings request.
const DefaultTimeout = 60 * time.Second

// HTTPEmbedder calls an OpenAI-compatible /v1/embeddings endpoint.
type HTTPEmbedder struct {
	endpoint  string
	secret    string
	model     string
	readyPath string
	batch     int
	client    *http.Client
}

// Options configures an HTTPEmbedder.
type Options struct {
	// Endpoint is the embeddings server base URL, e.g. http://127.0.0.1:9090.
	Endpoint string
	// Secret is sent as X-Sparknet-Auth. Empty sends no header.
	Secret string
	// Model is the embedding model name. Empty lets the server choose.
	Model string
	// Batch caps inputs per request. Zero uses DefaultBatch.
	Batch int
	// Timeout bounds one request. Zero uses DefaultTimeout.
	Timeout time.Duration
	// ReadyPath is probed by Health. Empty uses /v1/models.
	ReadyPath string
}

// NewHTTPEmbedder validates Options and builds an embedder.
func NewHTTPEmbedder(o Options) (*HTTPEmbedder, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(o.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("embed: endpoint is required")
	}
	batch := o.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ready := o.ReadyPath
	if ready == "" {
		ready = modelsPath
	}
	return &HTTPEmbedder{
		endpoint:  endpoint,
		secret:    o.Secret,
		model:     o.Model,
		readyPath: ready,
		batch:     batch,
		client:    &http.Client{Timeout: timeout},
	}, nil
}

// Name identifies the embedder in logs.
func (e *HTTPEmbedder) Name() string { return "http:" + e.endpoint }

// Batch returns the configured batch size.
func (e *HTTPEmbedder) Batch() int { return e.batch }

type embeddingsRequest struct {
	Model string   `json:"model,omitempty"`
	Input []string `json:"input"`
}

type embeddingItem struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embeddingsResponse struct {
	Data []embeddingItem `json:"data"`
	// Error is set by some servers as an object; we only need its message.
	Error json.RawMessage `json:"error"`
}

// Embed batches the inputs through the endpoint and returns vectors in input
// order. A server that returns a short or mismatched result is an error, not a
// silently misaligned index.
func (e *HTTPEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += e.batch {
		end := start + e.batch
		if end > len(inputs) {
			end = len(inputs)
		}
		vecs, err := e.embedBatch(ctx, inputs[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (e *HTTPEmbedder) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	body, err := json.Marshal(embeddingsRequest{Model: e.model, Input: batch})
	if err != nil {
		return nil, fmt.Errorf("embed: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint+embeddingsPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.secret != "" {
		req.Header.Set(HeaderAuth, e.secret)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("embed: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embed: status %d: %s", resp.StatusCode, errorMessage(data))
	}
	var parsed embeddingsResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(parsed.Data) != len(batch) {
		return nil, fmt.Errorf("embed: got %d vectors for %d inputs", len(parsed.Data), len(batch))
	}
	vecs := make([][]float32, len(batch))
	seen := make([]bool, len(batch))
	positional := true
	for i, item := range parsed.Data {
		if item.Index != i {
			positional = false
			break
		}
	}
	if positional {
		for i, item := range parsed.Data {
			vecs[i] = item.Embedding
		}
		return vecs, nil
	}
	// The server reordered the results; place them by their index field.
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(batch) {
			return nil, fmt.Errorf("embed: result index %d out of range", item.Index)
		}
		if seen[item.Index] {
			return nil, fmt.Errorf("embed: duplicate result index %d", item.Index)
		}
		seen[item.Index] = true
		vecs[item.Index] = item.Embedding
	}
	return vecs, nil
}

// Health probes the ready path.
func (e *HTTPEmbedder) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.endpoint+e.readyPath, nil)
	if err != nil {
		return fmt.Errorf("embed: build health request: %w", err)
	}
	if e.secret != "" {
		req.Header.Set(HeaderAuth, e.secret)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("embed: health: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("embed: health status %d", resp.StatusCode)
	}
	return nil
}

// errorMessage extracts a human message from an OpenAI-style error body.
func errorMessage(data []byte) string {
	var openai struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &openai) == nil && openai.Error.Message != "" {
		return openai.Error.Message
	}
	if len(data) > 200 {
		data = data[:200]
	}
	return string(bytes.TrimSpace(data))
}

// SortedDims is a helper for tests and diagnostics: the distinct vector
// dimensions seen, sorted ascending.
func SortedDims(vecs [][]float32) []int {
	set := map[int]struct{}{}
	for _, v := range vecs {
		set[len(v)] = struct{}{}
	}
	out := make([]int, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Ints(out)
	return out
}
