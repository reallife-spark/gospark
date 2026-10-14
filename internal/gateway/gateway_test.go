package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/reallife-spark/goSpark/internal/gateway"
	"github.com/reallife-spark/goSpark/internal/router"
)

func newGateway(upstream string) *gateway.Gateway {
	return gateway.New(upstream, router.New(router.Config{}))
}

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(newGateway("").Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status body = %v", body)
	}
}

func TestRouteDecision(t *testing.T) {
	srv := httptest.NewServer(newGateway("").Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/route?prompt=" + "hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var dec map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&dec); err != nil {
		t.Fatal(err)
	}
	if dec["target"] != "edge" {
		t.Fatalf("target = %v", dec["target"])
	}
}

func TestRouteRequiresPrompt(t *testing.T) {
	srv := httptest.NewServer(newGateway("").Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/route")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestProxyRefusesWithoutUpstream(t *testing.T) {
	srv := httptest.NewServer(newGateway("").Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestProxyStreamsUpstream(t *testing.T) {
	// A fake vLLM that emits a short SSE stream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, line := range []string{"data: hello\n\n", "data: world\n\n", "data: [DONE]\n\n"} {
			_, _ = io.WriteString(w, line)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	srv := httptest.NewServer(newGateway(upstream.URL).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "data: hello") || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("stream body = %q", body)
	}
}

func TestReadyWithoutUpstream(t *testing.T) {
	srv := httptest.NewServer(newGateway("").Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["ready"] != false {
		t.Fatalf("ready = %v, want false", body["ready"])
	}
}

func TestReadyProbesUpstream(t *testing.T) {
	ready := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer upstream.Close()

	gw := newGateway(upstream.URL)
	gw.Model = "Qwen/Qwen2.5-32B-Instruct"
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["ready"] != true || body["model"] != "Qwen/Qwen2.5-32B-Instruct" {
		t.Fatalf("body = %v", body)
	}

	ready = false
	resp2, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status with engine down = %d, want 503", resp2.StatusCode)
	}
}

func TestModelsProxiesUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"real-model"}]}`)
	}))
	defer upstream.Close()

	srv := httptest.NewServer(newGateway(upstream.URL).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "real-model") {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestModelsFallbackDeclaredModel(t *testing.T) {
	gw := newGateway("")
	gw.Model = "Qwen/Qwen2.5-32B-Instruct"
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(body.Data) != 1 || body.Data[0].ID != "Qwen/Qwen2.5-32B-Instruct" {
		t.Fatalf("status=%d body=%+v", resp.StatusCode, body)
	}
}

func TestModelsNoUpstreamNoModel(t *testing.T) {
	srv := httptest.NewServer(newGateway("").Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestStreamingMarksUnbuffered(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: x\n\n")
	}))
	defer upstream.Close()

	srv := httptest.NewServer(newGateway(upstream.URL).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
}
