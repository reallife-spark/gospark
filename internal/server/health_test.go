package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/bus"
)

// TestAdminHealthJoinsSources proves the health view merges the three sources:
// a node that reported health, a node seen only through telemetry, and
// authorization state.
func TestAdminHealthJoinsSources(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()
	app, cancel := placementControl(t, shared)
	defer cancel()
	ctx := context.Background()

	// node-a reports full health with telemetry.
	reportUntilEligible(t, ctx, shared, app, []reportSpec{
		{"node-a", gpuTel(10*gib, 80*gib)}, // 70 GiB free
	}, 1)

	// node-b is seen only through the telemetry subject (no register health).
	tel := gpuTel(40*gib, 80*gib)
	h := apiv1.NodeHealth{NodeID: "node-b", ReportedAtUnix: time.Now().Unix(), GPUTelemetry: tel}
	data, _ := json.Marshal(apiv1.RegisterNode{Health: &h})
	if err := shared.Publish(ctx, bus.Subject("sparknet", bus.KindHealth, ""), data); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(app.GPUSnapshot().Nodes) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("telemetry for node-b never recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}

	srv := httptest.NewServer(app.adminHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/health status = %d, want 200", resp.StatusCode)
	}
	var snap healthSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	byID := map[string]healthNodeView{}
	for _, n := range snap.Nodes {
		byID[n.NodeID] = n
	}
	a, ok := byID["node-a"]
	if !ok {
		t.Fatalf("node-a missing from health view: %+v", snap.Nodes)
	}
	if !a.Serving || a.GPU == nil || a.GPU.FreeMemBytes != 70*gib {
		t.Fatalf("node-a view = %+v", a)
	}
	if a.Authorized == nil || !*a.Authorized {
		t.Fatalf("node-a authorized = %v, want true", a.Authorized)
	}

	b, ok := byID["node-b"]
	if !ok {
		t.Fatalf("node-b (telemetry-only) missing from health view: %+v", snap.Nodes)
	}
	if b.Serving {
		t.Fatalf("node-b serving = true, want false (no health report)")
	}
	if b.GPU == nil || b.GPU.FreeMemBytes != 40*gib {
		t.Fatalf("node-b gpu = %+v", b.GPU)
	}
}

// TestAdminHealthReflectsRevocation proves the authorization column tracks the
// live registry.
func TestAdminHealthReflectsRevocation(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()
	app, cancel := placementControl(t, shared)
	defer cancel()
	ctx := context.Background()

	reportUntilEligible(t, ctx, shared, app, []reportSpec{{"node-a", gpuTel(10*gib, 80*gib)}}, 1)

	srv := httptest.NewServer(app.adminHandler())
	defer srv.Close()

	// Revoke node-a through the admin surface.
	postJSON(t, srv.URL+"/admin/registry/revoke", `{"node_id":"node-a"}`)

	resp, err := http.Get(srv.URL + "/admin/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var snap healthSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	for _, n := range snap.Nodes {
		if n.NodeID != "node-a" {
			continue
		}
		if !n.Revoked {
			t.Fatalf("node-a revoked = false, want true: %+v", n)
		}
		if n.Authorized == nil || *n.Authorized {
			t.Fatalf("node-a authorized = %v, want false after revoke", n.Authorized)
		}
		return
	}
	t.Fatal("node-a missing from health view")
}
