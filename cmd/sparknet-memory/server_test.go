package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/reallife-spark/goSpark/internal/memory"
)

func newTestService(t *testing.T) (*service, string, string) {
	t.Helper()
	root := t.TempDir()
	vaultRoot := filepath.Join(root, "vaults")
	if err := os.MkdirAll(vaultRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := memory.NewManager(memory.Config{
		Root:              filepath.Join(root, "memory"),
		NodeID:            "test-node",
		AllowedVaultRoots: []string{vaultRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := newService(Config{
		Listen:            "127.0.0.1:0",
		StateDir:          filepath.Join(root, "memory"),
		NodeID:            "test-node",
		GatewayToken:      "test-token",
		AllowedVaultRoots: []string{vaultRoot},
		MaxRequestBytes:   1 << 20,
		OpenCodeCommand:   []string{"/bin/echo", "{task}"},
	}, manager, nil)
	t.Cleanup(service.Close)
	return service, vaultRoot, filepath.Join(vaultRoot, "peer-alice")
}

func call(t *testing.T, handler http.Handler, method, path string, body any, token string) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	if token != "" {
		request.Header.Set("X-Sparknet-Memory-Token", token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder.Code, decoded
}

func TestUnauthorized(t *testing.T) {
	service, _, _ := newTestService(t)
	status, body := call(t, service.handler(), http.MethodGet, "/v1/memory/status?vault_path=/tmp", nil, "wrong")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

func TestRememberQueryStatusAndForget(t *testing.T) {
	service, _, vaultPath := newTestService(t)
	handler := service.handler()

	status, body := call(t, handler, http.MethodPost, "/v1/memory/remember", map[string]any{
		"vault_path": vaultPath,
		"record": map[string]any{
			"memory_type": "episodic",
			"text":        "the release plan uses signed grants",
			"source_kind": "session",
			"source_ref":  "sessions/2026-11-02.json",
			"scope":       map[string]any{"user_id": "alice", "project_id": "release"},
		},
	}, "test-token")
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("remember = %d %v", status, body)
	}
	record := body["record"].(map[string]any)
	id := record["memory_id"].(string)
	if record["vault_id"] == "" {
		t.Fatal("remember did not return vault_id")
	}
	if record["content_hash"] == "" || record["retention_until"] == "" {
		t.Fatalf("record missing hashes/retention: %v", record)
	}

	status, body = call(t, handler, http.MethodPost, "/v1/memory/query", map[string]any{
		"vault_path": vaultPath,
		"query":      "signed grants",
		"scope":      map[string]any{"user_id": "alice"},
	}, "test-token")
	if status != http.StatusOK || body["count"].(float64) != 1 {
		t.Fatalf("query = %d %v", status, body)
	}

	status, body = call(t, handler, http.MethodGet, "/v1/memory/status?vault_path="+vaultPath, nil, "test-token")
	if status != http.StatusOK || body["records"].(float64) != 1 {
		t.Fatalf("status = %d %v", status, body)
	}

	status, body = call(t, handler, http.MethodPost, "/v1/memory/forget", map[string]any{
		"vault_path": vaultPath,
		"ids":        []string{id},
		"mode":       "delete",
		"reason":     "test erasure",
	}, "test-token")
	if status != http.StatusOK || body["changed"].(float64) != 1 {
		t.Fatalf("forget = %d %v", status, body)
	}
}

func TestPerVaultIsolation(t *testing.T) {
	service, vaultRoot, alicePath := newTestService(t)
	handler := service.handler()
	bobPath := filepath.Join(vaultRoot, "peer-bob")
	if err := os.MkdirAll(bobPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ path, text string }{{alicePath, "alice release"}, {bobPath, "bob release"}} {
		status, body := call(t, handler, http.MethodPost, "/v1/memory/remember", map[string]any{
			"vault_path": entry.path,
			"record":     map[string]any{"memory_type": "episodic", "text": entry.text, "source_kind": "session"},
		}, "test-token")
		if status != http.StatusOK {
			t.Fatalf("remember %s = %d %v", entry.path, status, body)
		}
	}
	status, body := call(t, handler, http.MethodPost, "/v1/memory/query", map[string]any{
		"vault_path": alicePath,
		"query":      "release",
	}, "test-token")
	if status != http.StatusOK || body["count"].(float64) != 1 {
		t.Fatalf("alice query = %d %v", status, body)
	}
	hits := body["hits"].([]any)
	record := hits[0].(map[string]any)["record"].(map[string]any)
	if record["text"] != "alice release" {
		t.Fatalf("alice query leaked: %v", record)
	}
}

func TestDelegatePlanAndRun(t *testing.T) {
	service, _, vaultPath := newTestService(t)
	handler := service.handler()
	status, body := call(t, handler, http.MethodPost, "/v1/delegates/plan", map[string]any{
		"vault_path": vaultPath,
		"plan": map[string]any{
			"project_id": "release",
			"tasks": []any{
				map[string]any{"task_id": "one", "description": "write the report", "harness": "command"},
			},
		},
	}, "test-token")
	if status != http.StatusAccepted || body["ok"] != true {
		t.Fatalf("plan = %d %v", status, body)
	}
	runs := body["runs"].([]any)
	run := runs[0].(map[string]any)
	runID := run["run_id"].(string)
	planID := run["plan_id"].(string)
	_ = planID
	status, body = call(t, handler, http.MethodGet, "/v1/delegates/runs/"+runID+"?vault_path="+vaultPath, nil, "test-token")
	if status != http.StatusOK {
		t.Fatalf("get run = %d %v", status, body)
	}
	if body["run"].(map[string]any)["run_id"] != runID {
		t.Fatalf("wrong run: %v", body)
	}
}

func TestVaultOutsideRootRefused(t *testing.T) {
	service, _, _ := newTestService(t)
	status, body := call(t, service.handler(), http.MethodGet, "/v1/memory/status?vault_path=/etc", nil, "test-token")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d %v", status, body)
	}
}

func TestIndexVaultPages(t *testing.T) {
	service, _, vaultPath := newTestService(t)
	path := filepath.Join(vaultPath, "05_Knowledge", "fact.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# A fact\n\nThe release uses signed grants.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := service.handler()
	status, body := call(t, handler, http.MethodPost, "/v1/memory/index", map[string]any{
		"vault_path": vaultPath,
		"user_id":    "alice",
	}, "test-token")
	if status != http.StatusOK || body["documents"].(float64) != 1 {
		t.Fatalf("index = %d %v", status, body)
	}
	status, body = call(t, handler, http.MethodPost, "/v1/memory/query", map[string]any{
		"vault_path": vaultPath,
		"query":      "signed grants",
		"scope":      map[string]any{"user_id": "alice"},
	}, "test-token")
	if status != http.StatusOK || body["count"].(float64) != 1 {
		t.Fatalf("query = %d %v", status, body)
	}
	hits := body["hits"].([]any)
	record := hits[0].(map[string]any)["record"].(map[string]any)
	if record["memory_type"] != "semantic" || record["source_ref"] != "05_Knowledge/fact.md" {
		t.Fatalf("indexed record = %v", record)
	}
}

func TestExportPruneUsePostBody(t *testing.T) {
	service, _, vaultPath := newTestService(t)
	handler := service.handler()
	call(t, handler, http.MethodPost, "/v1/memory/remember", map[string]any{
		"vault_path": vaultPath,
		"record":     map[string]any{"memory_type": "episodic", "text": "export me", "source_kind": "session"},
	}, "test-token")
	status, body := call(t, handler, http.MethodPost, "/v1/memory/export", map[string]any{"vault_path": vaultPath}, "test-token")
	if status != http.StatusOK || len(body["records"].([]any)) != 1 {
		t.Fatalf("export = %d %v", status, body)
	}
	status, body = call(t, handler, http.MethodPost, "/v1/memory/prune", map[string]any{"vault_path": vaultPath}, "test-token")
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("prune = %d %v", status, body)
	}
}
