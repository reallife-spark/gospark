// Package kvrouter is the seam between goSpark's routing decision and the
// compute plane's RadixAttention engine.
//
// The prefix tree and the prefetch policy are compute-plane: only the engine
// knows which KV-cache blocks it actually holds. But *which node to send a
// prompt to* is a routing decision, and that is goSpark's job. This package is
// the client that asks the engine, over a small JSON contract, whether a prompt
// has an existing KV-cache prefix resident on some node — so the router can send
// the work where the prefill has already been paid for instead of recomputing
// it.
//
// It links no model, no attention kernel and no CUDA; it moves a prompt out and
// a residency answer back.
package kvrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Residency is the engine's answer: the node that already holds the longest
// matching cached prefix of a prompt.
type Residency struct {
	// Node is the node id/host holding the cached prefix. Empty means no match.
	Node string `json:"node"`
	// MatchedTokens is the length of the matched prefix, in tokens.
	MatchedTokens int `json:"matched_tokens"`
	// PrefixHash is the engine's stable hash of the matched prefix.
	PrefixHash string `json:"prefix_hash"`
}

// Router queries the compute-plane RadixAttention engine.
type Router interface {
	Name() string
	// Lookup returns the node holding the longest resident cached prefix of
	// prompt. ok is false when nothing matches (the common case on a cold
	// cache); an error means the engine could not be reached.
	Lookup(ctx context.Context, prompt string) (Residency, bool, error)
	// Register tells the engine a node now holds the KV cache for prefix.
	Register(ctx context.Context, prefix, node string) error
	Health(ctx context.Context) error
}

// HeaderAuth is the shared-secret header the compute plane uses.
const HeaderAuth = "X-Sparknet-Auth"

const (
	lookupPath   = "/v1/kv/lookup"
	registerPath = "/v1/kv/register"
	healthPath   = "/v1/kv/health"
)

// DefaultTimeout bounds a lookup. A lookup sits on the routing path, so it is
// deliberately short: if the engine cannot answer quickly the router falls back
// to telemetry placement rather than stalling a dispatch.
const DefaultTimeout = 2 * time.Second

// HTTPRouter calls a compute-plane RadixAttention daemon.
type HTTPRouter struct {
	endpoint string
	secret   string
	client   *http.Client
}

// Options configures an HTTPRouter.
type Options struct {
	// Endpoint is the RadixAttention daemon base URL.
	Endpoint string
	// Secret is sent as X-Sparknet-Auth. Empty sends no header.
	Secret string
	// Timeout bounds one call. Zero uses DefaultTimeout.
	Timeout time.Duration
}

// NewHTTPRouter validates Options and builds a router.
func NewHTTPRouter(o Options) (*HTTPRouter, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(o.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("kvrouter: endpoint is required")
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &HTTPRouter{
		endpoint: endpoint,
		secret:   o.Secret,
		client:   &http.Client{Timeout: timeout},
	}, nil
}

// Name identifies the router in logs.
func (r *HTTPRouter) Name() string { return "http:" + r.endpoint }

// ErrNoResidency is returned by Register/Lookup callers that treat "no match"
// as an error; Lookup itself reports a miss as ok=false.
var ErrNoResidency = errors.New("kvrouter: no resident prefix")

type lookupRequest struct {
	Prompt string `json:"prompt"`
}

type registerRequest struct {
	Prefix string `json:"prefix"`
	Node   string `json:"node"`
}

// Lookup asks the engine where a prompt's prefix is resident.
func (r *HTTPRouter) Lookup(ctx context.Context, prompt string) (Residency, bool, error) {
	body, err := json.Marshal(lookupRequest{Prompt: prompt})
	if err != nil {
		return Residency{}, false, fmt.Errorf("kvrouter: encode lookup: %w", err)
	}
	resp, err := r.do(ctx, http.MethodPost, lookupPath, body)
	if err != nil {
		return Residency{}, false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Residency{}, false, fmt.Errorf("kvrouter: lookup status %d: %s", resp.StatusCode, errorMessage(resp.Body))
	}
	var res Residency
	if err := json.Unmarshal(resp.Body, &res); err != nil {
		return Residency{}, false, fmt.Errorf("kvrouter: decode lookup: %w", err)
	}
	if res.Node == "" {
		return Residency{}, false, nil
	}
	return res, true, nil
}

// Register records that node holds the KV cache for prefix.
func (r *HTTPRouter) Register(ctx context.Context, prefix, node string) error {
	if strings.TrimSpace(prefix) == "" || strings.TrimSpace(node) == "" {
		return fmt.Errorf("kvrouter: prefix and node are required")
	}
	body, err := json.Marshal(registerRequest{Prefix: prefix, Node: node})
	if err != nil {
		return fmt.Errorf("kvrouter: encode register: %w", err)
	}
	resp, err := r.do(ctx, http.MethodPost, registerPath, body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("kvrouter: register status %d: %s", resp.StatusCode, errorMessage(resp.Body))
	}
	return nil
}

// Health probes the engine.
func (r *HTTPRouter) Health(ctx context.Context) error {
	resp, err := r.do(ctx, http.MethodGet, healthPath, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("kvrouter: health status %d", resp.StatusCode)
	}
	return nil
}

type rawResponse struct {
	StatusCode int
	Body       []byte
}

func (r *HTTPRouter) do(ctx context.Context, method, path string, body []byte) (rawResponse, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.endpoint+path, reader)
	if err != nil {
		return rawResponse{}, fmt.Errorf("kvrouter: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.secret != "" {
		req.Header.Set(HeaderAuth, r.secret)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return rawResponse{}, fmt.Errorf("kvrouter: request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return rawResponse{}, fmt.Errorf("kvrouter: read response: %w", err)
	}
	return rawResponse{StatusCode: resp.StatusCode, Body: data}, nil
}

// errorMessage extracts a human message from a JSON error body.
func errorMessage(data []byte) string {
	var e struct {
		Detail string `json:"detail"`
		Error  string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil {
		switch {
		case e.Detail != "":
			return e.Detail
		case e.Error != "":
			return e.Error
		}
	}
	if len(data) > 200 {
		data = data[:200]
	}
	return string(bytes.TrimSpace(data))
}
