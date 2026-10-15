package sim

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/reallife-spark/goSpark/internal/gputel"
)

func TestSimReadsFixtureFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gpu.json")
	cfg := `{"source":"sim","available":true,"driver_version":"mock-550.0","gpus":[{"index":0,"name":"GB10-SIM","temp_c":50,"mem_total_bytes":85899345920}],"health":[{"index":0,"status":"Healthy"}]}`
	if err := os.WriteFile(p, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	c := collector{filename: p}
	s := c.Snapshot(context.Background())
	if !s.Available {
		t.Fatalf("want available, got %+v", s)
	}
	if s.GPUs[0].Name != "GB10-SIM" {
		t.Fatalf("unexpected gpu: %+v", s.GPUs[0])
	}
}

func TestSimUnavailableWithoutFixture(t *testing.T) {
	c := collector{}
	s := c.Snapshot(context.Background())
	if s.Available {
		t.Fatal("should be unavailable")
	}
	if s.Source != "sim" {
		t.Fatalf("source=%q", s.Source)
	}
}

func TestSetFixtureRegistersBackend(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gpu.json")
	cfg := `{"source":"sim","available":true,"gpus":[{"index":0,"name":"SIM"}]}`
	if err := os.WriteFile(p, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	SetFixture(p)
	c, ok := gputel.Get("sim")
	if !ok {
		t.Fatal("SetFixture did not register the sim backend")
	}
	s := c.Snapshot(context.Background())
	if !s.Available || len(s.GPUs) != 1 || s.GPUs[0].Name != "SIM" {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
}

func TestSetFixtureIdempotent(t *testing.T) {
	SetFixture(filepath.Join(t.TempDir(), "a.json"))
	SetFixture(filepath.Join(t.TempDir(), "b.json")) // must not panic on duplicate registration
}

func TestSimReadsInlineConfig(t *testing.T) {
	c := collector{raw: `{"source":"sim","available":true,"gpus":[{"index":0}]}`}
	if s := c.Snapshot(context.Background()); !s.Available {
		t.Fatalf("want available: %+v", s)
	}
}

func TestSimBadFixture(t *testing.T) {
	c := collector{raw: `{not json`}
	s := c.Snapshot(context.Background())
	if s.Available || s.Reason == "" {
		t.Fatalf("want unavailable with reason: %+v", s)
	}
}
