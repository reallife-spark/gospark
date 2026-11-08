package server

import (
	"bytes"
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
)

func TestRequireLoopback(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:9000", "localhost:9000", "[::1]:9000"} {
		if err := requireLoopback(ok); err != nil {
			t.Errorf("requireLoopback(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:9000", ":9000", "192.168.1.5:9000", "10.0.0.1:80", "bad-addr"} {
		if err := requireLoopback(bad); err == nil {
			t.Errorf("requireLoopback(%q) = nil, want error", bad)
		}
	}
}

// TestAdminSurfaceRevokeShowReplace drives the control plane's live admin
// handler: it snapshots the served registry, revokes a node (digest advances,
// flag set), refuses an unknown node, reinstates, and replaces the whole
// authorization set — all without restarting the process.
func TestAdminSurfaceRevokeShowReplace(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()

	controlID, _ := identity.Generate("control", identity.RoleMaster)
	agentID, _ := identity.Generate("node-a", identity.RolePeer)

	entry := apiv1.NodeAuthorization{
		Identity:     agentID.Projection(),
		Capabilities: []string{string(authz.CapClusterServe)},
	}
	doc := &apiv1.NodeRegistry{Authorizations: []apiv1.NodeAuthorization{entry}}

	cfg := config.Default()
	cfg.Gateway.Listen = "127.0.0.1:0"
	cfg.Transport.Listen = "127.0.0.1:0"
	control, err := New(Options{Config: cfg, Identity: controlID, Role: RoleControl, Registry: doc, Bus: shared})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Wait for the control plane to come up and publish its signed registry:
	// that proves the bus is wired, so the admin surface can re-publish.
	published := make(chan struct{}, 1)
	if _, err := shared.Subscribe(ctx, bus.Subject("sparknet", bus.KindRegistry, ""),
		func(_ context.Context, _ string, _ []byte) error {
			select {
			case published <- struct{}{}:
			default:
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = control.Run(ctx) }()
	select {
	case <-published:
	case <-time.After(3 * time.Second):
		t.Fatal("control plane never published its registry")
	}

	srv := httptest.NewServer(control.adminHandler())
	defer srv.Close()

	view := getRegistry(t, srv.URL)
	if view.Digest == "" {
		t.Fatal("snapshot digest is empty")
	}
	if len(view.Nodes) != 1 || view.Nodes[0].NodeID != "node-a" || view.Nodes[0].Revoked {
		t.Fatalf("snapshot = %+v, want one active node-a", view.Nodes)
	}
	firstDigest := view.Digest

	// Revoke: the flag flips and the digest advances (fresh stamp).
	body := postJSON(t, srv.URL+"/admin/registry/revoke", `{"node_id":"node-a"}`)
	if body["revoked"] != true {
		t.Fatalf("revoke response = %+v, want revoked=true", body)
	}
	if body["digest"] == firstDigest {
		t.Fatal("digest did not advance after revoke")
	}
	view = getRegistry(t, srv.URL)
	if len(view.Nodes) != 1 || !view.Nodes[0].Revoked {
		t.Fatalf("snapshot after revoke = %+v, want node-a revoked", view.Nodes)
	}

	// Unknown node is a 404.
	resp, err := http.Post(srv.URL+"/admin/registry/revoke", "application/json",
		bytes.NewReader([]byte(`{"node_id":"ghost"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke unknown node status = %d, want 404", resp.StatusCode)
	}

	// Reinstatement.
	body = postJSON(t, srv.URL+"/admin/registry/revoke", `{"node_id":"node-a","revoked":false}`)
	if body["revoked"] != false {
		t.Fatalf("reinstatement response = %+v, want revoked=false", body)
	}
	view = getRegistry(t, srv.URL)
	if view.Nodes[0].Revoked {
		t.Fatalf("snapshot after reinstate = %+v, want node-a active", view.Nodes)
	}

	// Replace the whole set: node-a is dropped, node-b added.
	agentB, _ := identity.Generate("node-b", identity.RolePeer)
	repl := map[string]any{
		"authorizations": []apiv1.NodeAuthorization{{
			Identity:     agentB.Projection(),
			Capabilities: []string{string(authz.CapClusterServe)},
		}},
	}
	raw, _ := json.Marshal(repl)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/admin/registry", bytes.NewReader(raw))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replace status = %d, want 200", resp.StatusCode)
	}
	view = getRegistry(t, srv.URL)
	if len(view.Nodes) != 1 || view.Nodes[0].NodeID != "node-b" {
		t.Fatalf("snapshot after replace = %+v, want only node-b", view.Nodes)
	}
}

// TestAdminRefusesNonLoopback proves a misconfigured control plane fails closed
// rather than exposing full registry authority on a public interface.
func TestAdminRefusesNonLoopback(t *testing.T) {
	cfg := config.Default()
	cfg.Gateway.Listen = "127.0.0.1:0"
	cfg.Transport.Listen = "127.0.0.1:0"
	cfg.Admin.Listen = "0.0.0.0:0"

	id, _ := identity.Generate("control", identity.RoleMaster)
	control, err := New(Options{Config: cfg, Identity: id, Role: RoleControl, Bus: bus.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := control.Run(ctx); err == nil {
		t.Fatal("Run accepted a non-loopback admin listen, want error")
	}
}

// TestAdminRevokeAgentRefused pins that the admin op is control-plane only.
func TestAdminRevokeAgentRefused(t *testing.T) {
	cfg := config.Default()
	cfg.Gateway.Listen = "127.0.0.1:0"
	cfg.Transport.Listen = "127.0.0.1:0"
	id, _ := identity.Generate("node-a", identity.RolePeer)
	control, _ := identity.Generate("control", identity.RoleMaster)
	agent, err := New(Options{Config: cfg, Identity: id, Role: RoleAgent, MasterPublic: control.Public, Bus: bus.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.AdminRevoke(context.Background(), "x", true); err == nil {
		t.Fatal("AdminRevoke on an agent returned nil, want error")
	}
}

// TestAdminAuthorize exercises the sign-authorization right: a new node is
// granted capabilities, an unknown capability is refused, and re-authorizing an
// existing node upserts (replaces) its capabilities rather than duplicating it.
func TestAdminAuthorize(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()

	controlID, _ := identity.Generate("control", identity.RoleMaster)
	agentA, _ := identity.Generate("node-a", identity.RolePeer)
	doc := &apiv1.NodeRegistry{Authorizations: []apiv1.NodeAuthorization{{
		Identity:     agentA.Projection(),
		Capabilities: []string{string(authz.CapClusterServe)},
	}}}

	cfg := config.Default()
	cfg.Gateway.Listen = "127.0.0.1:0"
	cfg.Transport.Listen = "127.0.0.1:0"
	control, err := New(Options{Config: cfg, Identity: controlID, Role: RoleControl, Registry: doc, Bus: shared})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	published := make(chan struct{}, 1)
	if _, err := shared.Subscribe(ctx, bus.Subject("sparknet", bus.KindRegistry, ""),
		func(_ context.Context, _ string, _ []byte) error {
			select {
			case published <- struct{}{}:
			default:
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = control.Run(ctx) }()
	select {
	case <-published:
	case <-time.After(3 * time.Second):
		t.Fatal("control plane never published its registry")
	}

	srv := httptest.NewServer(control.adminHandler())
	defer srv.Close()

	agentB, _ := identity.Generate("node-b", identity.RolePeer)
	authorizeNode(t, srv.URL, agentB.Projection(), []string{string(authz.CapClusterServe)}, 3600)
	view := getRegistry(t, srv.URL)
	if len(view.Nodes) != 2 || findNode(view, "node-b") == nil {
		t.Fatalf("after authorize nodes = %+v, want node-a and node-b", view.Nodes)
	}

	// An unknown capability is refused (400) and changes nothing.
	raw, _ := json.Marshal(map[string]any{"identity": agentB.Projection(), "capabilities": []string{"not-a-cap"}})
	resp, err := http.Post(srv.URL+"/admin/registry/authorize", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown capability status = %d, want 400", resp.StatusCode)
	}

	// Re-authorizing upserts: node-b's capabilities are replaced, not appended.
	authorizeNode(t, srv.URL, agentB.Projection(), []string{string(authz.CapTelemetry)}, 3600)
	view = getRegistry(t, srv.URL)
	n := findNode(view, "node-b")
	if n == nil || len(n.Capabilities) != 1 || n.Capabilities[0] != string(authz.CapTelemetry) {
		t.Fatalf("node-b after upsert = %+v, want [telemetry]", n)
	}
	if len(view.Nodes) != 2 {
		t.Fatalf("authorize duplicated the node: %+v", view.Nodes)
	}

	// Every entry the admin surface serves — including the one it just minted —
	// must carry a valid Master signature.
	control.regMu.RLock()
	served := control.registryDoc
	control.regMu.RUnlock()
	if err := authz.VerifyRegistry(controlID.Public, served); err != nil {
		t.Fatalf("served registry does not verify against the master key: %v", err)
	}
}

func authorizeNode(t *testing.T, base string, proj apiv1.NodeIdentity, caps []string, ttl int64) map[string]any {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"identity": proj, "capabilities": caps, "ttl_seconds": ttl})
	if err != nil {
		t.Fatal(err)
	}
	return postJSON(t, base+"/admin/registry/authorize", string(raw))
}

func findNode(v adminRegistryView, id string) *adminNodeView {
	for i := range v.Nodes {
		if v.Nodes[i].NodeID == id {
			return &v.Nodes[i]
		}
	}
	return nil
}

func getRegistry(t *testing.T, base string) adminRegistryView {
	t.Helper()
	resp, err := http.Get(base + "/admin/registry")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/registry status = %d, want 200", resp.StatusCode)
	}
	var view adminRegistryView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	return view
}

func postJSON(t *testing.T, url, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAdminSurfaceRequiresBearerToken(t *testing.T) {
	controlID, _ := identity.Generate("control", identity.RoleMaster)
	cfg := config.Default()
	cfg.Admin.Token = "test-admin-token"
	control, err := New(Options{Config: cfg, Identity: controlID, Role: RoleControl, Bus: bus.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(control.adminHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/registry")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin without token status = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/registry", nil)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin with token status = %d, want 200", resp.StatusCode)
	}
}

func TestAdminRunRefusesListenerWithoutToken(t *testing.T) {
	cfg := config.Default()
	cfg.Gateway.Listen = "127.0.0.1:0"
	cfg.Transport.Listen = "127.0.0.1:0"
	cfg.Admin.Listen = "127.0.0.1:0"
	controlID, _ := identity.Generate("control", identity.RoleMaster)
	control, err := New(Options{Config: cfg, Identity: controlID, Role: RoleControl, Bus: bus.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	if err := control.Run(context.Background()); err == nil {
		t.Fatal("Run enabled the admin listener without a token")
	}
}
