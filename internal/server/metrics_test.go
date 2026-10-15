package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/bus"
)

// TestAdminMetricsRendersControlState publishes GPU telemetry, bumps a counter,
// and reads both back through /admin/metrics.
func TestAdminMetricsRendersControlState(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()
	app := testControlApp(t, shared)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := shared.Publish(ctx, bus.Subject("sparknet", bus.KindHealth, ""),
			gpuHealthPayload(t, "node-a")); err == nil {
			if len(app.GPUSnapshot().Nodes) == 1 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("control plane never recorded GPU telemetry")
		}
		time.Sleep(10 * time.Millisecond)
	}
	app.counters.TasksVerified.Add(4)

	rec := httptest.NewRecorder()
	app.adminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/metrics status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"gospark_tasks_verified_total 4",
		`gospark_node_serving{node="node-a"}`,
		`gospark_gpu_temperature_celsius{gpu="0",name="NVIDIA H100",node="node-a"} 50`,
		`gospark_gpu_healthy{gpu="0",name="NVIDIA H100",node="node-a"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

// TestAdminMetricsReadOnly rejects a non-GET method.
func TestAdminMetricsReadOnly(t *testing.T) {
	app := testControlApp(t, bus.NewMemory())
	rec := httptest.NewRecorder()
	app.adminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /admin/metrics status = %d, want 405", rec.Code)
	}
}

// TestStandaloneMetricsHandler covers the optional scrape surface.
func TestStandaloneMetricsHandler(t *testing.T) {
	app := testControlApp(t, bus.NewMemory())

	rec := httptest.NewRecorder()
	app.metricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gospark_build_info") {
		t.Fatalf("GET /metrics status = %d body = %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	app.metricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("GET /healthz status = %d body = %q", rec.Code, rec.Body.String())
	}
}
