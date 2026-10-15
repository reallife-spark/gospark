package kvrouter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/kvrouter"
)

// daemon is a fake RadixAttention engine.
type daemon struct {
	mu       sync.Mutex
	resident map[string]string // prefix -> node
	secret   string
	fail     bool
	regs     int
}

func (d *daemon) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if d.secret != "" && r.Header.Get(kvrouter.HeaderAuth) != d.secret {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": "bad token"})
			return false
		}
		return true
	}
	mux.HandleFunc("/v1/kv/lookup", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if d.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		node := d.resident[req.Prompt]
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"node": node, "matched_tokens": 12, "prefix_hash": "abc"})
	})
	mux.HandleFunc("/v1/kv/register", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var req struct {
			Prefix string `json:"prefix"`
			Node   string `json:"node"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.resident[req.Prefix] = req.Node
		d.regs++
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/v1/kv/health", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "active"})
	})
	return mux
}

func newClient(t *testing.T, d *daemon, secret string) *kvrouter.HTTPRouter {
	t.Helper()
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	r, err := kvrouter.NewHTTPRouter(kvrouter.Options{Endpoint: srv.URL, Secret: secret, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLookupHit(t *testing.T) {
	d := &daemon{resident: map[string]string{"hello world": "node-a"}}
	r := newClient(t, d, "")
	res, ok, err := r.Lookup(context.Background(), "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || res.Node != "node-a" || res.MatchedTokens != 12 {
		t.Fatalf("lookup = %+v ok=%t", res, ok)
	}
}

func TestLookupMiss(t *testing.T) {
	d := &daemon{resident: map[string]string{}}
	r := newClient(t, d, "")
	if _, ok, err := r.Lookup(context.Background(), "cold prompt"); err != nil || ok {
		t.Fatalf("miss: ok=%t err=%v", ok, err)
	}
}

func TestLookupAuth(t *testing.T) {
	d := &daemon{resident: map[string]string{}, secret: "s3cr3t"}
	r := newClient(t, d, "s3cr3t")
	if _, _, err := r.Lookup(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	bad := newClient(t, d, "wrong")
	if _, _, err := bad.Lookup(context.Background(), "x"); err == nil {
		t.Fatal("expected auth error")
	}
}

func TestLookupError(t *testing.T) {
	d := &daemon{resident: map[string]string{}, fail: true}
	r := newClient(t, d, "")
	if _, _, err := r.Lookup(context.Background(), "x"); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestRegisterAndHealth(t *testing.T) {
	d := &daemon{resident: map[string]string{}}
	r := newClient(t, d, "")
	if err := r.Register(context.Background(), "p", "node-b"); err != nil {
		t.Fatal(err)
	}
	if err := r.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, ok, err := r.Lookup(context.Background(), "p")
	if err != nil || !ok || res.Node != "node-b" {
		t.Fatalf("after register: %+v ok=%t err=%v", res, ok, err)
	}
}

func TestRegisterRejectsEmpty(t *testing.T) {
	d := &daemon{resident: map[string]string{}}
	r := newClient(t, d, "")
	if err := r.Register(context.Background(), "", "node-b"); err == nil {
		t.Fatal("expected error for empty prefix")
	}
	if err := r.Register(context.Background(), "p", ""); err == nil {
		t.Fatal("expected error for empty node")
	}
}

func TestNewHTTPRouterRequiresEndpoint(t *testing.T) {
	if _, err := kvrouter.NewHTTPRouter(kvrouter.Options{}); err == nil {
		t.Fatal("expected error for empty endpoint")
	}
}
