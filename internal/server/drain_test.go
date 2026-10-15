package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/bus"
)

func TestBeginDrainIdempotent(t *testing.T) {
	app := testControlApp(t, bus.NewMemory())
	if app.Draining() {
		t.Fatal("a fresh app should not be draining")
	}
	app.BeginDrain()
	app.BeginDrain()
	if !app.Draining() {
		t.Fatal("Draining() = false after BeginDrain")
	}
}

func TestDispatchRefusedWhileDraining(t *testing.T) {
	app := testControlApp(t, bus.NewMemory())
	app.BeginDrain()
	err := app.DispatchTask(context.Background(), "node-a", apiv1.TaskEnvelope{TaskID: "t1", Operation: "arith"})
	if err == nil || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("DispatchTask err = %v, want a draining refusal", err)
	}
}

func TestTaskRefusedWhileDraining(t *testing.T) {
	app := testControlApp(t, bus.NewMemory())
	app.BeginDrain()
	data, _ := json.Marshal(apiv1.TaskEnvelope{TaskID: "t1"})
	if err := app.onTask(context.Background(), "sparknet.task.node-a", data); err == nil {
		t.Fatal("onTask accepted work while draining")
	}
}

func TestAdminDrainRoute(t *testing.T) {
	app := testControlApp(t, bus.NewMemory())

	rec := httptest.NewRecorder()
	app.adminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/drain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /admin/drain status = %d, want 200", rec.Code)
	}
	if !app.Draining() {
		t.Fatal("app not draining after POST /admin/drain")
	}

	rec = httptest.NewRecorder()
	app.adminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/drain", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /admin/drain status = %d, want 405", rec.Code)
	}
}

func TestMetricsReportDraining(t *testing.T) {
	app := testControlApp(t, bus.NewMemory())
	app.BeginDrain()
	rec := httptest.NewRecorder()
	app.adminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/metrics", nil))
	if !strings.Contains(rec.Body.String(), "gospark_draining 1") {
		t.Fatalf("metrics missing draining gauge:\n%s", rec.Body.String())
	}
}
