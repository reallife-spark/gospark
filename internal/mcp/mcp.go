// Package mcp is a minimal Model Context Protocol server over stdio. It exposes
// goSpark's read-only observability surface — the loopback admin routes — as MCP
// tools, so an agent or IDE that speaks MCP can inspect a running goSpark
// control plane without a bespoke client.
//
// It is deliberately small and dependency-free: JSON-RPC 2.0 messages are read
// and written as a stream of JSON values (MCP's stdio transport). Only the
// read-only tools are exposed; the mutating admin routes (authorize, revoke,
// drain) are intentionally *not* reachable over MCP.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2024-11-05"

// Version is the server version reported at initialize. A build may stamp it.
var Version = "dev"

// Request is a JSON-RPC 2.0 request or notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Server answers MCP requests against a running control plane's admin surface.
type Server struct {
	// AdminBase is the base URL of the control plane's loopback admin surface,
	// e.g. http://127.0.0.1:8899.
	AdminBase string
	// AdminToken is sent as a bearer token; the admin surface requires it.
	AdminToken string
	// Client is the HTTP client; nil uses a client with Timeout.
	Client *http.Client
	// Timeout bounds one admin request when Client is nil.
	Timeout time.Duration
}

// toolDef is one exposed tool.
type toolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
	run         func(ctx context.Context, s *Server, args map[string]any) (string, error)
}

func (s *Server) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	t := s.Timeout
	if t <= 0 {
		t = 10 * time.Second
	}
	return &http.Client{Timeout: t}
}

// Serve reads JSON-RPC messages from in and writes responses to out until in is
// exhausted or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("mcp: decode: %w", err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A notification (no id) is acknowledged with silence.
		if len(req.ID) == 0 {
			continue
		}
		if err := enc.Encode(s.Handle(ctx, req)); err != nil {
			return fmt.Errorf("mcp: encode: %w", err)
		}
	}
}

// Handle answers one request. It never writes; Serve does.
func (s *Server) Handle(ctx context.Context, req Request) Response {
	resp := Response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "gospark-mcp", "version": Version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolList()}
	case "tools/call":
		result, err := s.callTool(ctx, req.Params)
		if err != nil {
			resp.Error = &RPCError{Code: -32602, Message: err.Error()}
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &RPCError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

// callTool dispatches a tools/call. A tool's own failure is reported as MCP
// content with isError, not as a JSON-RPC error, per the MCP convention.
func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("tools/call params: %w", err)
	}
	for _, t := range toolList() {
		if t.Name != p.Name {
			continue
		}
		text, err := t.run(ctx, s, p.Arguments)
		if err != nil {
			return textContent(err.Error(), true), nil
		}
		return textContent(text, false), nil
	}
	return nil, fmt.Errorf("unknown tool %q", p.Name)
}

func textContent(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// toolList returns the read-only tools. It is rebuilt per call so the tool set
// is one place to read.
func toolList() []toolDef {
	empty := map[string]any{"type": "object", "properties": map[string]any{}}
	return []toolDef{
		{
			Name:        "gospark_health",
			Description: "Joined node health: last report, authorization state and GPU telemetry per node.",
			InputSchema: empty,
			run: func(ctx context.Context, s *Server, _ map[string]any) (string, error) {
				return s.get(ctx, "/admin/health", nil)
			},
		},
		{
			Name:        "gospark_metrics",
			Description: "The control plane's Prometheus text exposition (counters, node and GPU gauges).",
			InputSchema: empty,
			run: func(ctx context.Context, s *Server, _ map[string]any) (string, error) {
				return s.get(ctx, "/admin/metrics", nil)
			},
		},
		{
			Name:        "gospark_registry",
			Description: "The served node registry: authorizations, capabilities and revocation state.",
			InputSchema: empty,
			run: func(ctx context.Context, s *Server, _ map[string]any) (string, error) {
				return s.get(ctx, "/admin/registry", nil)
			},
		},
		{
			Name:        "gospark_gpu",
			Description: "Per-node GPU telemetry as the control plane sees it.",
			InputSchema: empty,
			run: func(ctx context.Context, s *Server, _ map[string]any) (string, error) {
				return s.get(ctx, "/admin/gpu", nil)
			},
		},
		{
			Name:        "gospark_placement",
			Description: "Rank dispatch candidates and report which node would be chosen, without dispatching.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"caps":            map[string]any{"type": "string", "description": "capability to require"},
					"min_free_mem":    map[string]any{"type": "integer", "description": "minimum free accelerator bytes"},
					"max_temp":        map[string]any{"type": "integer", "description": "maximum reported GPU temperature (C)"},
					"require_healthy": map[string]any{"type": "boolean", "description": "require a Healthy verdict on every GPU"},
					"max_age":         map[string]any{"type": "string", "description": "treat telemetry older than this (Go duration) as stale"},
				},
			},
			run: placementTool,
		},
	}
}

func placementTool(ctx context.Context, s *Server, args map[string]any) (string, error) {
	q := url.Values{}
	if v, ok := args["caps"].(string); ok && v != "" {
		q.Set("caps", v)
	}
	if v, ok := args["min_free_mem"]; ok {
		q.Set("min_free_mem", numString(v))
	}
	if v, ok := args["max_temp"]; ok {
		q.Set("max_temp", numString(v))
	}
	if v, ok := args["require_healthy"].(bool); ok && v {
		q.Set("require_healthy", "1")
	}
	if v, ok := args["max_age"].(string); ok && v != "" {
		q.Set("max_age", v)
	}
	return s.get(ctx, "/admin/placement", q)
}

// numString renders a JSON number (which decodes as float64) as an integer
// string when it is whole, so the admin surface parses it.
func numString(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case string:
		return n
	default:
		return fmt.Sprint(v)
	}
}

// get performs one admin GET and returns the response body, turning a non-200
// into an error carrying the server's message.
func (s *Server) get(ctx context.Context, path string, q url.Values) (string, error) {
	u := s.AdminBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	if s.AdminToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AdminToken)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("admin %s: %s: %s", path, resp.Status, string(data))
	}
	return string(data), nil
}
