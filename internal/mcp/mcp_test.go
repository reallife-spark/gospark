package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(t *testing.T, h http.Handler) *Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Server{AdminBase: srv.URL}
}

func TestInitialize(t *testing.T) {
	s := &Server{AdminBase: "http://127.0.0.1:1"}
	resp := s.Handle(context.Background(), Request{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "initialize"})
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
	m, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T", resp.Result)
	}
	if m["protocolVersion"] != ProtocolVersion {
		t.Fatalf("protocolVersion = %v", m["protocolVersion"])
	}
	info, _ := m["serverInfo"].(map[string]any)
	if info["name"] != "gospark-mcp" {
		t.Fatalf("serverInfo = %v", info)
	}
}

func TestToolsList(t *testing.T) {
	s := &Server{}
	resp := s.Handle(context.Background(), Request{Method: "tools/list", ID: json.RawMessage("2")})
	m := resp.Result.(map[string]any)
	tools := m["tools"].([]toolDef)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"gospark_health", "gospark_metrics", "gospark_registry", "gospark_gpu", "gospark_placement"} {
		if !names[want] {
			t.Errorf("tool %q missing from tools/list", want)
		}
	}
}

func TestCallHealthReturnsAdminBody(t *testing.T) {
	s := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"nodes":[{"node_id":"node-a"}]}`))
	}))
	params := json.RawMessage(`{"name":"gospark_health","arguments":{}}`)
	resp := s.Handle(context.Background(), Request{Method: "tools/call", ID: json.RawMessage("3"), Params: params})
	if resp.Error != nil {
		t.Fatalf("tools/call error: %+v", resp.Error)
	}
	m := resp.Result.(map[string]any)
	if m["isError"] != false {
		t.Fatalf("isError = %v, want false", m["isError"])
	}
	content := m["content"].([]map[string]any)
	if !strings.Contains(content[0]["text"].(string), "node-a") {
		t.Fatalf("content = %v", content)
	}
}

func TestCallPlacementBuildsQuery(t *testing.T) {
	var gotQuery string
	s := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"node_id":"node-b"}`))
	}))
	params := json.RawMessage(`{"name":"gospark_placement","arguments":{"caps":"cluster-serve","min_free_mem":1024,"max_temp":80,"require_healthy":true,"max_age":"30s"}}`)
	resp := s.Handle(context.Background(), Request{Method: "tools/call", ID: json.RawMessage("4"), Params: params})
	if resp.Error != nil {
		t.Fatalf("tools/call error: %+v", resp.Error)
	}
	for _, want := range []string{"caps=cluster-serve", "min_free_mem=1024", "max_temp=80", "require_healthy=1", "max_age=30s"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestCallToolErrorIsContent(t *testing.T) {
	s := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	params := json.RawMessage(`{"name":"gospark_health","arguments":{}}`)
	resp := s.Handle(context.Background(), Request{Method: "tools/call", ID: json.RawMessage("5"), Params: params})
	if resp.Error != nil {
		t.Fatalf("a tool failure must be content, not a JSON-RPC error: %+v", resp.Error)
	}
	m := resp.Result.(map[string]any)
	if m["isError"] != true {
		t.Fatalf("isError = %v, want true", m["isError"])
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	s := &Server{}
	resp := s.Handle(context.Background(), Request{Method: "tools/call", ID: json.RawMessage("6"), Params: json.RawMessage(`{"name":"nope"}`)})
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("unknown tool error = %+v", resp.Error)
	}
	resp = s.Handle(context.Background(), Request{Method: "bogus", ID: json.RawMessage("7")})
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("unknown method error = %+v", resp.Error)
	}
}

func TestServeRoundTrip(t *testing.T) {
	s := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"gospark_metrics","arguments":{}}}` + "\n")
	var out strings.Builder
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d responses, want 2 (the notification is silent):\n%s", len(lines), out.String())
	}
	var second Response
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if string(second.ID) != "2" {
		t.Fatalf("second id = %s, want 2", second.ID)
	}
}
