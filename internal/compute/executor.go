// Package compute is the seam between the Go control plane and the Python/CUDA
// compute plane.
//
// goSpark deliberately runs no model, tensor, tokenizer or CUDA code: the
// closed operation sandbox in internal/server executes only pure-Go operations.
// Heavy work belongs beside a GPU, so an operator may point a node at a
// compute-plane worker — the blueprint's sparknet_bridge.py — and name the
// operations that worker is trusted to run. The Go plane still verifies the
// Master-signed task and still signs the attested result; only the execution of
// the named operations is delegated.
//
// This is not a general remote-code endpoint: the delegatable operation set is
// explicit config, the endpoint is operator-chosen, and the worker authenticates
// the caller with a shared secret. An empty configuration disables the seam and
// restores the pure sandbox.
package compute

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

// Executor is the compute-plane side of the seam. Handles reports which
// operations this executor is responsible for; Execute runs one of them.
type Executor interface {
	Name() string
	Handles(operation string) bool
	// Operations lists the delegated operation names for display.
	Operations() []string
	Execute(ctx context.Context, operation, paramsJSON string) (string, error)
	Health(ctx context.Context) error
}

// HeaderAuth is the shared-secret header the blueprint's bridge uses.
const HeaderAuth = "X-Sparknet-Auth"

// executePath and healthPath are the worker's routes, mirroring the blueprint's
// FastAPI daemon.
const (
	executePath = "/v1/execute"
	healthPath  = "/v1/health"
)

// HTTPExecutor delegates named operations to a compute-plane worker over HTTP.
type HTTPExecutor struct {
	endpoint string
	secret   string
	client   *http.Client
	ops      map[string]struct{}
}

// Options configures an HTTPExecutor.
type Options struct {
	// Endpoint is the worker base URL, e.g. http://127.0.0.1:9090.
	Endpoint string
	// Secret is sent as X-Sparknet-Auth. Empty sends no header (worker must
	// then accept anonymous callers, which is only sane on loopback).
	Secret string
	// Operations is the explicit set of operations delegated to this worker.
	// It must be non-empty; each name is rejected if it collides with a local
	// sandbox operation (the caller enforces that).
	Operations []string
	// Timeout bounds a single Execute. Zero uses DefaultTimeout.
	Timeout time.Duration
}

// DefaultTimeout bounds a single delegated operation when Options.Timeout is 0.
const DefaultTimeout = 60 * time.Second

// NewHTTPExecutor validates Options and builds an executor.
func NewHTTPExecutor(o Options) (*HTTPExecutor, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(o.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("compute: endpoint is required")
	}
	if len(o.Operations) == 0 {
		return nil, fmt.Errorf("compute: at least one operation must be delegated")
	}
	ops := make(map[string]struct{}, len(o.Operations))
	for _, op := range o.Operations {
		op = strings.TrimSpace(op)
		if op == "" {
			return nil, fmt.Errorf("compute: empty operation name")
		}
		ops[op] = struct{}{}
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &HTTPExecutor{
		endpoint: endpoint,
		secret:   o.Secret,
		client:   &http.Client{Timeout: timeout},
		ops:      ops,
	}, nil
}

// Name identifies the executor in logs.
func (e *HTTPExecutor) Name() string { return "http:" + e.endpoint }

// Handles reports whether op is delegated to this worker.
func (e *HTTPExecutor) Handles(operation string) bool {
	_, ok := e.ops[operation]
	return ok
}

// Operations returns the delegated operation names, sorted.
func (e *HTTPExecutor) Operations() []string {
	out := make([]string, 0, len(e.ops))
	for op := range e.ops {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

type executeRequest struct {
	NodeID     string `json:"node_id"`
	Operation  string `json:"operation"`
	ParamsJSON string `json:"params_json"`
}

type executeResponse struct {
	Result string `json:"result"`
	Error  string `json:"error"`
}

// Execute posts one operation to the worker and returns its result string. The
// operation must be one this executor Handles; anything else is refused before
// a request is made.
func (e *HTTPExecutor) Execute(ctx context.Context, operation, paramsJSON string) (string, error) {
	if !e.Handles(operation) {
		return "", fmt.Errorf("compute: operation %q not delegated to %s", operation, e.Name())
	}
	body, err := json.Marshal(executeRequest{Operation: operation, ParamsJSON: paramsJSON})
	if err != nil {
		return "", fmt.Errorf("compute: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint+executePath, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("compute: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.secret != "" {
		req.Header.Set(HeaderAuth, e.secret)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("compute: worker call: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("compute: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var er executeResponse
		if json.Unmarshal(data, &er) == nil && er.Error != "" {
			return "", fmt.Errorf("compute: worker %d: %s", resp.StatusCode, er.Error)
		}
		return "", fmt.Errorf("compute: worker status %d", resp.StatusCode)
	}
	var out executeResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("compute: decode response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("compute: worker error: %s", out.Error)
	}
	return out.Result, nil
}

// Health probes the worker's /v1/health route.
func (e *HTTPExecutor) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.endpoint+healthPath, nil)
	if err != nil {
		return fmt.Errorf("compute: build health request: %w", err)
	}
	if e.secret != "" {
		req.Header.Set(HeaderAuth, e.secret)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("compute: health call: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("compute: worker health status %d", resp.StatusCode)
	}
	return nil
}
