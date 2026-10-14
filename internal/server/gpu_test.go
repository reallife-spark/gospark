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
	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/identity"
)

func testControlApp(t *testing.T, shared bus.Bus) *App {
	t.Helper()
	id, err := identity.Generate("control", identity.RoleMaster)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Gateway.Listen = "127.0.0.1:0"
	cfg.Transport.Listen = "127.0.0.1:0"
	cfg.Admin.Listen = ""
	app, err := New(Options{Config: cfg, Identity: id, Role: RoleControl, Bus: shared})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func gpuHealthPayload(t *testing.T, node string) []byte {
	t.Helper()
	data, err := json.Marshal(apiv1.RegisterNode{
		Health: &apiv1.NodeHealth{
			NodeID: node,
			GPUTelemetry: &apiv1.GpuTelemetry{
				Source: "nvml+dcgm", Available: true, DriverVersion: "550.0",
				GPUs: []apiv1.GpuMetrics{{Index: 0, Name: "NVIDIA H100", TempC: 50, Health: "Healthy"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestGPUTelemetryIgnoredWithoutPayload checks the intake does not record a
// health report that carries no GPU telemetry (an ordinary agent handshake).
func TestGPUTelemetryIgnoredWithoutPayload(t *testing.T) {
	app := testControlApp(t, bus.NewMemory())
	data, _ := json.Marshal(apiv1.RegisterNode{Health: &apiv1.NodeHealth{NodeID: "node-a"}})
	if err := app.onGPUHealth(context.Background(), "sparknet.health", data); err != nil {
		t.Fatalf("onGPUHealth: %v", err)
	}
	if got := app.GPUSnapshot(); len(got.Nodes) != 0 {
		t.Fatalf("recorded telemetry-less report: %+v", got)
	}
}

// TestGPUTelemetryIntakeAndAdminView publishes a sidecar-style health report on
// the running control plane's health subject and reads it back through the
// admin surface, proving the bus-to-view path end to end.
func TestGPUTelemetryIntakeAndAdminView(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()
	app := testControlApp(t, shared)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Run(ctx) }()

	// Wait for the control plane to arm its health subscription, then publish.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := shared.Publish(ctx, bus.Subject("sparknet", bus.KindHealth, ""),
			gpuHealthPayload(t, "node-a")); err == nil {
			if len(app.GPUSnapshot().Nodes) == 1 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("control plane never recorded GPU telemetry: %+v", app.GPUSnapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}

	snap := app.GPUSnapshot()
	if snap.Nodes[0].NodeID != "node-a" {
		t.Fatalf("node_id = %q, want node-a", snap.Nodes[0].NodeID)
	}
	if len(snap.Nodes[0].Telemetry.GPUs) != 1 || snap.Nodes[0].Telemetry.GPUs[0].Name != "NVIDIA H100" {
		t.Fatalf("telemetry = %+v", snap.Nodes[0].Telemetry)
	}

	// The admin surface serves the same view.
	rec := httptest.NewRecorder()
	app.adminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/gpu", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/gpu status = %d, want 200", rec.Code)
	}
	var view struct {
		Nodes []gpuNodeView `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode admin body: %v", err)
	}
	if len(view.Nodes) != 1 || view.Nodes[0].NodeID != "node-a" {
		t.Fatalf("admin view = %+v", view)
	}

	// It is read-only.
	rec = httptest.NewRecorder()
	app.adminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/gpu", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /admin/gpu status = %d, want 405", rec.Code)
	}
}
