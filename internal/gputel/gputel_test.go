package gputel

import (
	"context"
	"strings"
	"testing"
)

type fakeCollector struct{ snap Snapshot }

func (f fakeCollector) Snapshot(context.Context) Snapshot { return f.snap }

func TestNullReportsUnavailable(t *testing.T) {
	s := Null{}.Snapshot(context.Background())
	if s.Available {
		t.Fatal("Null reported available")
	}
	if s.Source != "null" || s.Reason == "" {
		t.Fatalf("Null snapshot = %+v", s)
	}
	if s.CollectedAtUnix == 0 {
		t.Fatal("Null snapshot has no timestamp")
	}
}

func TestMultiMergesMetricsAndHealth(t *testing.T) {
	metrics := fakeCollector{Snapshot{Source: "nvml", Available: true,
		GPUs: []GPU{{Index: 0, Name: "NVIDIA H100", TempC: 50, MemTotalBytes: 80 << 30}}}}
	health := fakeCollector{Snapshot{Source: "dcgm", Available: true,
		Health: []Health{{Index: 0, Status: "Healthy", Watches: []string{"PCIE: Healthy"}}}}}

	s := Multi{metrics, health}.Snapshot(context.Background())
	if !s.Available {
		t.Fatalf("merged snapshot unavailable: %s", s.Reason)
	}
	if s.Source != "nvml+dcgm" {
		t.Fatalf("source = %q, want nvml+dcgm", s.Source)
	}
	if len(s.GPUs) != 1 || s.GPUs[0].Name != "NVIDIA H100" || s.GPUs[0].TempC != 50 {
		t.Fatalf("merged GPUs = %+v", s.GPUs)
	}
	if len(s.Health) != 1 || s.Health[0].Status != "Healthy" {
		t.Fatalf("merged health = %+v", s.Health)
	}
}

func TestMultiFillsEmptyGPUFieldsAcrossCollectors(t *testing.T) {
	a := fakeCollector{Snapshot{Source: "a", Available: true, GPUs: []GPU{{Index: 0, Name: "H100"}}}}
	b := fakeCollector{Snapshot{Source: "b", Available: true, GPUs: []GPU{{Index: 0, TempC: 42}}}}
	s := Multi{a, b}.Snapshot(context.Background())
	if len(s.GPUs) != 1 {
		t.Fatalf("want one merged GPU, got %+v", s.GPUs)
	}
	if s.GPUs[0].Name != "H100" || s.GPUs[0].TempC != 42 {
		t.Fatalf("fields not merged: %+v", s.GPUs[0])
	}
}

func TestMultiAllUnavailableJoinsReasons(t *testing.T) {
	s := Multi{Null{Reason: "no driver"}, Null{Reason: "no dcgm"}}.Snapshot(context.Background())
	if s.Available {
		t.Fatal("merged snapshot should be unavailable")
	}
	if !strings.Contains(s.Reason, "no driver") || !strings.Contains(s.Reason, "no dcgm") {
		t.Fatalf("reason = %q", s.Reason)
	}
}

func TestWireProjectsGPUAndHealth(t *testing.T) {
	s := Snapshot{
		Source: "nvml+dcgm", Available: true, DriverVersion: "550.0",
		GPUs:   []GPU{{Index: 0, Name: "H100", TempC: 50}},
		Health: []Health{{Index: 0, Status: "Warning"}},
	}
	w := s.Wire()
	if !w.Available || w.Source != "nvml+dcgm" || w.DriverVersion != "550.0" {
		t.Fatalf("wire header = %+v", w)
	}
	if len(w.GPUs) != 1 || w.GPUs[0].TempC != 50 || w.GPUs[0].Health != "Warning" {
		t.Fatalf("wire gpu = %+v", w.GPUs)
	}
}

func TestDefaultWithNoBackendsIsNull(t *testing.T) {
	if len(Names()) != 0 {
		t.Skipf("backends registered in this build: %v", Names())
	}
	if _, ok := Default().(Null); !ok {
		t.Fatalf("Default() = %T, want Null when no backend is registered", Default())
	}
}

func TestRegisterAndGet(t *testing.T) {
	want := fakeCollector{Snapshot{Source: "fake", Available: true}}
	Register("fake-test", func() Collector { return want })
	c, ok := Get("fake-test")
	if !ok {
		t.Fatal("registered backend not found")
	}
	if s := c.Snapshot(context.Background()); s.Source != "fake" {
		t.Fatalf("backend snapshot source = %q, want fake", s.Source)
	}
	found := false
	for _, n := range Names() {
		if n == "fake-test" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Names() = %v, missing fake-test", Names())
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	Register("fake-dup", func() Collector { return Null{} })
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	Register("fake-dup", func() Collector { return Null{} })
}
