package compute

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewHTTPExecutorValidation(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"empty endpoint", Options{Operations: []string{"infer"}}},
		{"no operations", Options{Endpoint: "http://127.0.0.1:9090"}},
		{"empty operation name", Options{Endpoint: "http://127.0.0.1:9090", Operations: []string{"infer", " "}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewHTTPExecutor(tc.opts); err == nil {
				t.Fatalf("NewHTTPExecutor(%+v) = nil error, want error", tc.opts)
			}
		})
	}
}

func TestHTTPExecutorExecute(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != executePath {
			t.Errorf("path = %q, want %q", r.URL.Path, executePath)
		}
		gotAuth = r.Header.Get(HeaderAuth)
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]string{"result": "hello world"})
	}))
	defer srv.Close()

	ex, err := NewHTTPExecutor(Options{Endpoint: srv.URL + "/", Secret: "s3cr3t", Operations: []string{"infer", "embed"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ex.Execute(context.Background(), "infer", `{"prompt":"hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Fatalf("result = %q, want %q", got, "hello world")
	}
	if gotAuth != "s3cr3t" {
		t.Fatalf("auth header = %q, want %q", gotAuth, "s3cr3t")
	}
	var req executeRequest
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("request body not JSON: %v (%s)", err, gotBody)
	}
	if req.Operation != "infer" || req.ParamsJSON != `{"prompt":"hi"}` {
		t.Fatalf("request body = %+v", req)
	}
}

func TestHTTPExecutorNoSecretNoHeader(t *testing.T) {
	var hadHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hadHeader = r.Header.Get(HeaderAuth) != ""
		_ = json.NewEncoder(w).Encode(map[string]string{"result": "ok"})
	}))
	defer srv.Close()

	ex, err := NewHTTPExecutor(Options{Endpoint: srv.URL, Operations: []string{"infer"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Execute(context.Background(), "infer", "{}"); err != nil {
		t.Fatal(err)
	}
	if hadHeader {
		t.Fatal("auth header sent despite empty secret")
	}
}

func TestHTTPExecutorRejectsUnhandledOperation(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer srv.Close()

	ex, err := NewHTTPExecutor(Options{Endpoint: srv.URL, Operations: []string{"infer"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Execute(context.Background(), "digest", "{}"); err == nil {
		t.Fatal("Execute of unhandled operation = nil error, want error")
	}
	if called {
		t.Fatal("worker was called for an unhandled operation")
	}
}

func TestHTTPExecutorWorkerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "CUDA out of memory"})
	}))
	defer srv.Close()

	ex, err := NewHTTPExecutor(Options{Endpoint: srv.URL, Operations: []string{"infer"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ex.Execute(context.Background(), "infer", "{}")
	if err == nil || !strings.Contains(err.Error(), "CUDA out of memory") {
		t.Fatalf("err = %v, want it to carry the worker's message", err)
	}
}

func TestHTTPExecutorNonJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	defer srv.Close()

	ex, err := NewHTTPExecutor(Options{Endpoint: srv.URL, Operations: []string{"infer"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Execute(context.Background(), "infer", "{}"); err == nil {
		t.Fatal("non-JSON response = nil error, want decode error")
	}
}

func TestHTTPExecutorHealth(t *testing.T) {
	ok := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != healthPath {
			t.Errorf("path = %q, want %q", r.URL.Path, healthPath)
		}
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "active"})
	}))
	defer srv.Close()

	ex, err := NewHTTPExecutor(Options{Endpoint: srv.URL, Operations: []string{"infer"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ex.Health(context.Background()); err != nil {
		t.Fatalf("health = %v, want nil", err)
	}
	ok = false
	if err := ex.Health(context.Background()); err == nil {
		t.Fatal("health of unhealthy worker = nil, want error")
	}
}

func TestHTTPExecutorTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]string{"result": "late"})
	}))
	defer srv.Close()

	ex, err := NewHTTPExecutor(Options{Endpoint: srv.URL, Operations: []string{"infer"}, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Execute(context.Background(), "infer", "{}"); err == nil {
		t.Fatal("slow worker = nil error, want timeout")
	}
}

func TestHTTPExecutorOperationsSorted(t *testing.T) {
	ex, err := NewHTTPExecutor(Options{Endpoint: "http://x", Operations: []string{"z", "a", "m"}})
	if err != nil {
		t.Fatal(err)
	}
	got := ex.Operations()
	want := []string{"a", "m", "z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Operations() = %v, want %v", got, want)
	}
}
