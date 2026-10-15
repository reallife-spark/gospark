package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadyReportsDraining(t *testing.T) {
	g := newGateway("http://127.0.0.1:8000")
	g.Model = "test-model"
	g.Draining = func() bool { return true }
	srv := httptest.NewServer(g.Handler())
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
	if body["ready"] != false || body["reason"] != "draining" {
		t.Fatalf("body = %v, want ready=false reason=draining", body)
	}
}

func TestProxyRefusesWhenDraining(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	g := newGateway(upstream.URL)
	g.Draining = func() bool { return true }
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if called {
		t.Fatal("upstream was called while draining")
	}
}
