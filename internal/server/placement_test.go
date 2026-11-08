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

const gib = uint64(1) << 30

// placementControl builds a control plane whose registry authorizes node-a and
// node-b for cluster-serve, runs it, and returns it with a cancel func. Any
// mutate funcs are applied to the config before the app is built.
func placementControl(t *testing.T, shared bus.Bus, mutate ...func(*config.Config)) (*App, context.CancelFunc) {
	t.Helper()
	controlID, _ := identity.Generate("control", identity.RoleMaster)
	agentA, _ := identity.Generate("node-a", identity.RolePeer)
	agentB, _ := identity.Generate("node-b", identity.RolePeer)
	doc := &apiv1.NodeRegistry{Authorizations: []apiv1.NodeAuthorization{
		{Identity: agentA.Projection(), Capabilities: []string{string(authz.CapClusterServe)}},
		{Identity: agentB.Projection(), Capabilities: []string{string(authz.CapClusterServe)}},
	}}
	cfg := config.Default()
	cfg.Gateway.Listen = "127.0.0.1:0"
	cfg.Transport.Listen = "127.0.0.1:0"
	for _, m := range mutate {
		m(&cfg)
	}
	app, err := New(Options{Config: cfg, Identity: controlID, Role: RoleControl, Registry: doc, Bus: shared})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Wait until the control plane has published its signed registry: that
	// proves runControl has installed the health registry and armed the
	// register subscription, without touching App fields from the test.
	published := make(chan struct{}, 1)
	if _, err := shared.Subscribe(ctx, bus.Subject("sparknet", bus.KindRegistry, ""),
		func(_ context.Context, _ string, _ []byte) error {
			select {
			case published <- struct{}{}:
			default:
			}
			return nil
		}); err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() { _ = app.Run(ctx) }()
	select {
	case <-published:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("control plane never published its registry")
	}
	return app, cancel
}

// report publishes a register handshake carrying GPU telemetry (the shape the
// agent adopts), so both Serving and telemetry land in the control plane.
func report(t *testing.T, ctx context.Context, shared bus.Bus, app *App, node string, tel *apiv1.GpuTelemetry) {
	t.Helper()
	h := apiv1.NodeHealth{NodeID: node, Serving: true, GPUTelemetry: tel}
	data, _ := json.Marshal(apiv1.RegisterNode{Health: &h, RegistryDigest: app.RegistryDigest()})
	if err := shared.Publish(ctx, bus.Subject("sparknet", bus.KindRegister, ""), data); err != nil {
		t.Fatalf("publish register: %v", err)
	}
}

type reportSpec struct {
	node string
	tel  *apiv1.GpuTelemetry
}

// reportUntilEligible republishes the handshakes until the control plane sees
// the wanted number of eligible candidates. The control plane publishes its
// registry (the readiness signal the test waits on) just before it arms the
// register subscription, so the first reports can race ahead of that
// subscription and be dropped; a real telemetry loop is at-least-once.
func reportUntilEligible(t *testing.T, ctx context.Context, shared bus.Bus, app *App, specs []reportSpec, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, s := range specs {
			report(t, ctx, shared, app, s.node, s.tel)
		}
		if app.Place(placement.Request{}, authz.CapClusterServe).Eligible >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d eligible candidates seen, want %d",
				app.Place(placement.Request{}, authz.CapClusterServe).Eligible, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func gpuTel(freeUsed, total uint64) *apiv1.GpuTelemetry {
	return &apiv1.GpuTelemetry{
		Available: true, CollectedAtUnix: time.Now().Unix(),
		GPUs: []apiv1.GpuMetrics{{MemTotalBytes: total, MemUsedBytes: freeUsed, Health: "Healthy"}},
	}
}

// TestPlaceSelectsByFreeMemory proves the control plane runs telemetry through
// the placement engine: node-b has more free accelerator memory and is chosen.
func TestPlaceSelectsByFreeMemory(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()
	app, cancel := placementControl(t, shared)
	defer cancel()
	ctx := context.Background()

	reportUntilEligible(t, ctx, shared, app, []reportSpec{
		{"node-a", gpuTel(70*gib, 80*gib)},
		{"node-b", gpuTel(10*gib, 80*gib)},
	}, 2)

	dec := app.Place(placement.Request{}, authz.CapClusterServe)
	if dec.NodeID != "node-b" {
		t.Fatalf("chose %q, want node-b (%+v)", dec.NodeID, dec)
	}
}

// TestPlaceRespectsRequirement proves a resource floor excludes a node and that
// an unsatisfiable floor returns no choice.
func TestPlaceRespectsRequirement(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()
	app, cancel := placementControl(t, shared)
	defer cancel()
	ctx := context.Background()

	reportUntilEligible(t, ctx, shared, app, []reportSpec{
		{"node-a", gpuTel(70*gib, 80*gib)}, // 10 GiB free
		{"node-b", gpuTel(10*gib, 80*gib)}, // 70 GiB free
	}, 2)

	dec := app.Place(placement.Request{Requirement: placement.Requirement{MinFreeMemBytes: 32 * gib}}, authz.CapClusterServe)
	if dec.NodeID != "node-b" || dec.Eligible != 1 {
		t.Fatalf("floor 32GiB: chose %q eligible=%d, want node-b/1", dec.NodeID, dec.Eligible)
	}
	dec = app.Place(placement.Request{Requirement: placement.Requirement{MinFreeMemBytes: 75 * gib}}, authz.CapClusterServe)
	if dec.NodeID != "" || dec.Eligible != 0 {
		t.Fatalf("floor 75GiB: chose %q eligible=%d, want none/0", dec.NodeID, dec.Eligible)
	}
}

// TestAdminPlacementEndpoint drives GET /admin/placement over HTTP.
func TestAdminPlacementEndpoint(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()
	app, cancel := placementControl(t, shared)
	defer cancel()
	ctx := context.Background()

	reportUntilEligible(t, ctx, shared, app, []reportSpec{
		{"node-a", gpuTel(70*gib, 80*gib)},
		{"node-b", gpuTel(10*gib, 80*gib)},
	}, 2)

	srv := httptest.NewServer(app.adminHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/placement")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/placement status = %d, want 200", resp.StatusCode)
	}
	var dec placement.Decision
	if err := json.NewDecoder(resp.Body).Decode(&dec); err != nil {
		t.Fatal(err)
	}
	if dec.NodeID != "node-b" {
		t.Fatalf("endpoint chose %q, want node-b", dec.NodeID)
	}

	// An unknown capability is refused.
	resp, err = http.Get(srv.URL + "/admin/placement?caps=nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown capability status = %d, want 400", resp.StatusCode)
	}
}

// TestPlaceAndDispatchToChosenNode proves the whole path: telemetry selects a
// node, and the task is dispatched to exactly that node and executed there.
func TestPlaceAndDispatchToChosenNode(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()

	controlID, _ := identity.Generate("control", identity.RoleMaster)
	agentA, _ := identity.Generate("node-a", identity.RolePeer)
	agentB, _ := identity.Generate("node-b", identity.RolePeer)
	doc := &apiv1.NodeRegistry{Authorizations: []apiv1.NodeAuthorization{
		{Identity: agentA.Projection(), Capabilities: []string{string(authz.CapClusterServe)}},
		{Identity: agentB.Projection(), Capabilities: []string{string(authz.CapClusterServe)}},
	}}

	newCfg := func() config.Config {
		c := config.Default()
		c.Gateway.Listen = "127.0.0.1:0"
		c.Transport.Listen = "127.0.0.1:0"
		return c
	}
	control, err := New(Options{Config: newCfg(), Identity: controlID, Role: RoleControl, Registry: doc, Bus: shared})
	if err != nil {
		t.Fatal(err)
	}
	agentACfg := newCfg()
	agentACfg.Node.Capabilities = []string{string(authz.CapClusterServe)}
	agentBCfg := newCfg()
	agentBCfg.Node.Capabilities = []string{string(authz.CapClusterServe)}
	appA, err := New(Options{Config: agentACfg, Identity: agentA, Role: RoleAgent, MasterPublic: controlID.Public, Bus: shared})
	if err != nil {
		t.Fatal(err)
	}
	appB, err := New(Options{Config: agentBCfg, Identity: agentB, Role: RoleAgent, MasterPublic: controlID.Public, Bus: shared})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = control.Run(ctx) }()
	go func() { _ = appA.Run(ctx) }()
	go func() { _ = appB.Run(ctx) }()
	waitDigestMatch(t, control, appA)
	waitDigestMatch(t, control, appB)

	req := placement.Request{Requirement: placement.Requirement{MinFreeMemBytes: 32 * gib}}
	specs := []reportSpec{
		{"node-a", gpuTel(70*gib, 80*gib)}, // 10 GiB free
		{"node-b", gpuTel(10*gib, 80*gib)}, // 70 GiB free
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, s := range specs {
			report(t, ctx, shared, control, s.node, s.tel)
		}
		if control.Place(req, authz.CapClusterServe).NodeID == "node-b" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("placement never saw node-b; decision = %+v", control.Place(req, authz.CapClusterServe))
		}
		time.Sleep(20 * time.Millisecond)
	}

	results := watchResults(ctx, t, shared)
	node, dec, err := control.PlaceAndDispatch(ctx, signedTask(t, controlID, "t-auto", "node-b"), req.Requirement)
	if err != nil {
		t.Fatalf("PlaceAndDispatch: %v", err)
	}
	if node != "node-b" {
		t.Fatalf("dispatched to %q, want node-b (%s)", node, dec.Reason)
	}
	expectResult(t, results, "42")
}
