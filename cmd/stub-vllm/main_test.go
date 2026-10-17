package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(t *testing.T, srv *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestHealthAndModels(t *testing.T) {
	srv := httptest.NewServer(newMux("sparknet-stub"))
	defer srv.Close()

	resp := do(t, srv, http.MethodGet, "/health", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health["status"] != "ok" {
		t.Errorf("health status field = %v", health["status"])
	}

	resp = do(t, srv, http.MethodGet, "/v1/models", "")
	var models modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(models.Data) != 1 || models.Data[0].ID != "sparknet-stub" {
		t.Errorf("models = %+v, want one sparknet-stub", models.Data)
	}
}

func TestChatCompletionEchoesDeterministically(t *testing.T) {
	srv := httptest.NewServer(newMux("sparknet-stub"))
	defer srv.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	var first chatResponse
	resp := do(t, srv, http.MethodPost, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil {
		t.Fatalf("decode chat: %v", err)
	}
	if len(first.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(first.Choices))
	}
	want := echoText("sparknet-stub", digest("hello\n"))
	if got := first.Choices[0].Message.Content; got != want {
		t.Errorf("chat content = %q, want %q", got, want)
	}

	var second chatResponse
	resp = do(t, srv, http.MethodPost, "/v1/chat/completions", body)
	if err := json.NewDecoder(resp.Body).Decode(&second); err != nil {
		t.Fatalf("decode chat 2: %v", err)
	}
	if first.Choices[0].Message.Content != second.Choices[0].Message.Content {
		t.Error("chat content is not deterministic across calls")
	}
}

func TestCompletionsEndpointEchoesPrompt(t *testing.T) {
	srv := httptest.NewServer(newMux("sparknet-stub"))
	defer srv.Close()

	body := `{"model":"m","prompt":"Q: 2+2\nA:","max_tokens":16}`
	resp := do(t, srv, http.MethodPost, "/v1/completions", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("completions status = %d", resp.StatusCode)
	}
	var out completionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode completions: %v", err)
	}
	if out.Object != "text_completion" {
		t.Errorf("object = %q, want text_completion", out.Object)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	want := echoText("sparknet-stub", digest("Q: 2+2\nA:"))
	if got := out.Choices[0].Text; got != want {
		t.Errorf("completion text = %q, want %q", got, want)
	}
}

func TestNonPostIsRejected(t *testing.T) {
	srv := httptest.NewServer(newMux("sparknet-stub"))
	defer srv.Close()

	for _, path := range []string{"/v1/chat/completions", "/v1/completions"} {
		resp := do(t, srv, http.MethodGet, path, "")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s status = %d, want 405", path, resp.StatusCode)
		}
	}
}

func TestBadJSONIsRejected(t *testing.T) {
	srv := httptest.NewServer(newMux("sparknet-stub"))
	defer srv.Close()

	resp := do(t, srv, http.MethodPost, "/v1/completions", "{not json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json status = %d, want 400", resp.StatusCode)
	}
}
