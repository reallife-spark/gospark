package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/bus"
	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/identity"
	"github.com/reallife-spark/goSpark/internal/placement"
)

// kvDaemon is a fake RadixAttention engine: it reports node-a as holding the
// cached prefix for one prompt and nothing else.
func kvDaemon(t *testing.T, node, prompt string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/kv/lookup", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := ""
		if req.Prompt == prompt {
			n = node
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"node": n, "matched_tokens": 8, "prefix_hash": "h"})
	})
	mux.HandleFunc("/v1/kv/register", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/v1/kv/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "active"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// signedTaskWithParams signs a task carrying an arbitrary params body (here, a
// prompt that the KV seam can match), keeping the arith operation so a
// dispatched node still returns a result.
func signedTaskWithParams(t *testing.T, id *identity.Identity, taskID, paramsJSON string) apiv1.TaskEnvelope {
	t.Helper()
	now := time.Now()
	env := apiv1.TaskEnvelope{
		Fingerprint:        id.Fingerprint(),
		Capabilities:       []string{string(authz.CapClusterServe)},
		TaskID:             taskID,
		Operation:          "arith",
		ParamsJSON:         paramsJSON,
		ContentInSignature: true,
		Nonce:              "nonce-" + taskID,
		IssuedAtUnix:       now.Add(-time.Second).Unix(),
		ExpiresAtUnix:      now.Add(time.Minute).Unix(),
	}
	msg, err := apiv1.CanonicalBytes(&env)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := id.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	env.SignatureB64 = identity.EncodeSignature(sig)
	return env
}

const kvPrompt = "enterprise q3 financial audit protocols and security standards"

// TestKVResidencyPrefersCachedNode proves the seam changes the dispatch
// decision: node-a has less free memory but holds the prompt's cached prefix,
// so it is chosen over node-b.
func TestKVResidencyPrefersCachedNode(t *testing.T) {
	daemon := kvDaemon(t, "node-a", kvPrompt)
	shared := bus.NewMemory()
	defer shared.Close()
	app, cancel := placementControl(t, shared, func(c *config.Config) {
		c.KV.Endpoint = daemon.URL
	})
	defer cancel()
	ctx := context.Background()

	reportUntilEligible(t, ctx, shared, app, []reportSpec{
		{"node-a", gpuTel(70*gib, 80*gib)}, // 10 GiB free, but resident
		{"node-b", gpuTel(10*gib, 80*gib)}, // 70 GiB free
	}, 2)

	// Without the seam, telemetry wins.
	if dec := app.Place(placement.Request{}, authz.CapClusterServe); dec.NodeID != "node-b" {
		t.Fatalf("baseline chose %q, want node-b", dec.NodeID)
	}

	// The seam reports the residency for this prompt.
	res, ok, err := app.KVLookup(ctx, kvPrompt)
	if err != nil || !ok || res.Node != "node-a" {
		t.Fatalf("KVLookup = %+v ok=%t err=%v", res, ok, err)
	}
	// A prompt with no residency reports a miss, not an error.
	if _, ok, err := app.KVLookup(ctx, "unrelated cold prompt"); err != nil || ok {
		t.Fatalf("cold lookup: ok=%t err=%v", ok, err)
	}

	dec := app.Place(placement.Request{Requirement: placement.Requirement{ResidentNode: "node-a"}}, authz.CapClusterServe)
	if dec.NodeID != "node-a" {
		t.Fatalf("resident chose %q, want node-a (%s)", dec.NodeID, dec.Reason)
	}

	// End to end: a task whose params carry the prompt is dispatched to node-a
	// (the resident), not node-b (the emptier node).
	env := signedTaskWithParams(t, app.id, "t-kv", `{"prompt":"`+kvPrompt+`","expression":"6*7"}`)
	node, dec, err := app.PlaceAndDispatch(ctx, env, placement.Requirement{})
	if err != nil {
		t.Fatalf("PlaceAndDispatch: %v", err)
	}
	if node != "node-a" {
		t.Fatalf("dispatched to %q, want node-a (%s)", node, dec.Reason)
	}
}

// TestAdminKVCacheEndpoint drives GET /admin/kvcache with and without a prompt.
func TestAdminKVCacheEndpoint(t *testing.T) {
	daemon := kvDaemon(t, "node-a", kvPrompt)
	shared := bus.NewMemory()
	defer shared.Close()
	app, cancel := placementControl(t, shared, func(c *config.Config) {
		c.KV.Endpoint = daemon.URL
	})
	defer cancel()

	srv := httptest.NewServer(app.adminHandler())
	defer srv.Close()

	var status struct {
		Configured bool   `json:"configured"`
		Healthy    bool   `json:"healthy"`
		Node       string `json:"node"`
		Matched    int    `json:"matched_tokens"`
	}
	resp, err := http.Get(srv.URL + "/admin/kvcache")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !status.Configured || !status.Healthy {
		t.Fatalf("status = %+v, want configured+healthy", status)
	}

	resp, err = http.Get(srv.URL + "/admin/kvcache?prompt=" + "enterprise+q3+financial+audit+protocols+and+security+standards")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if status.Node != "node-a" || status.Matched != 8 {
		t.Fatalf("prompt query = %+v, want node-a/8", status)
	}
}

// TestAdminKVCacheUnconfigured proves the endpoint reports cleanly when no seam
// is set.
func TestAdminKVCacheUnconfigured(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()
	app, cancel := placementControl(t, shared)
	defer cancel()

	srv := httptest.NewServer(app.adminHandler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/admin/kvcache")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status struct {
		Configured bool `json:"configured"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Configured {
		t.Fatal("unconfigured seam reported configured")
	}
}
