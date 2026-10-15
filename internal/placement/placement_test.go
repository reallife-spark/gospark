package placement

import (
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
)

func gpu(total, used uint64, temp, util uint32, health string) apiv1.GpuMetrics {
	return apiv1.GpuMetrics{MemTotalBytes: total, MemUsedBytes: used, TempC: temp, UtilPct: util, Health: health}
}

func tel(at int64, gpus ...apiv1.GpuMetrics) *apiv1.GpuTelemetry {
	return &apiv1.GpuTelemetry{Available: true, CollectedAtUnix: at, GPUs: gpus}
}

const giB = uint64(1) << 30

func TestRanksByFreeMemory(t *testing.T) {
	cands := []Candidate{
		{NodeID: "a", Serving: true, Telemetry: tel(100, gpu(80*giB, 70*giB, 40, 10, "Healthy"))},
		{NodeID: "b", Serving: true, Telemetry: tel(100, gpu(80*giB, 10*giB, 40, 10, "Healthy"))},
		{NodeID: "c", Serving: true, Telemetry: tel(100, gpu(80*giB, 40*giB, 40, 10, "Healthy"))},
	}
	dec := Select(cands, Request{Now: time.Unix(120, 0)})
	if dec.NodeID != "b" {
		t.Fatalf("chose %q, want b", dec.NodeID)
	}
	want := []string{"b", "c", "a"}
	for i, id := range want {
		if dec.Ranked[i].NodeID != id {
			t.Fatalf("rank[%d]=%q, want %q (order %v)", i, dec.Ranked[i].NodeID, id, dec.Ranked)
		}
	}
}

// TestResidentNodePreferred proves a KV-cache residency hint outranks free
// memory: the resident node is chosen even though another node has far more
// free accelerator memory, because its prefill is already cached.
func TestResidentNodePreferred(t *testing.T) {
	cands := []Candidate{
		{NodeID: "node-a", Serving: true, Telemetry: tel(100, gpu(80*giB, 70*giB, 40, 10, "Healthy"))}, // 10 GiB free
		{NodeID: "node-b", Serving: true, Telemetry: tel(100, gpu(80*giB, 10*giB, 40, 10, "Healthy"))}, // 70 GiB free
	}
	dec := Select(cands, Request{Now: time.Unix(120, 0), Requirement: Requirement{ResidentNode: "node-a"}})
	if dec.NodeID != "node-a" {
		t.Fatalf("chose %q, want node-a (resident)", dec.NodeID)
	}
	if !dec.Ranked[0].Resident {
		t.Fatalf("chosen node not marked resident: %+v", dec.Ranked[0])
	}
	if dec.Reason != "KV-cache prefix resident; prefill already cached" {
		t.Fatalf("reason = %q", dec.Reason)
	}
}

// TestResidentHintIgnoredWhenNotCandidate proves the hint is advisory: a
// resident node that is not a candidate changes nothing.
func TestResidentHintIgnoredWhenNotCandidate(t *testing.T) {
	cands := []Candidate{
		{NodeID: "node-a", Serving: true, Telemetry: tel(100, gpu(80*giB, 70*giB, 40, 10, "Healthy"))},
		{NodeID: "node-b", Serving: true, Telemetry: tel(100, gpu(80*giB, 10*giB, 40, 10, "Healthy"))},
	}
	dec := Select(cands, Request{Now: time.Unix(120, 0), Requirement: Requirement{ResidentNode: "node-z"}})
	if dec.NodeID != "node-b" {
		t.Fatalf("chose %q, want node-b (resident hint for absent node ignored)", dec.NodeID)
	}
}

func TestTelemetryBeatsNoTelemetry(t *testing.T) {
	cands := []Candidate{
		{NodeID: "blind", Serving: true},
		{NodeID: "gpu", Serving: true, Telemetry: tel(100, gpu(80*giB, 79*giB, 60, 90, "Healthy"))},
	}
	dec := Select(cands, Request{Now: time.Unix(120, 0)})
	if dec.NodeID != "gpu" {
		t.Fatalf("chose %q, want gpu (a reading must beat none)", dec.NodeID)
	}
}

func TestMinFreeMemFilters(t *testing.T) {
	cands := []Candidate{
		{NodeID: "small", Serving: true, Telemetry: tel(100, gpu(80*giB, 79*giB, 40, 10, "Healthy"))},
		{NodeID: "big", Serving: true, Telemetry: tel(100, gpu(80*giB, 10*giB, 40, 10, "Healthy"))},
		{NodeID: "blind", Serving: true},
	}
	dec := Select(cands, Request{Now: time.Unix(120, 0), Requirement: Requirement{MinFreeMemBytes: 32 * giB}})
	if dec.NodeID != "big" {
		t.Fatalf("chose %q, want big", dec.NodeID)
	}
	if dec.Eligible != 1 {
		t.Fatalf("eligible = %d, want 1", dec.Eligible)
	}
	// The reineligibles carry a reason.
	for _, s := range dec.Ranked {
		if !s.Eligible && s.Reason == "" {
			t.Fatalf("ineligible %s has no reason", s.NodeID)
		}
	}
}

func TestMaxTempAndHealthy(t *testing.T) {
	cands := []Candidate{
		{NodeID: "hot", Serving: true, Telemetry: tel(100, gpu(80*giB, 10*giB, 85, 10, "Healthy"))},
		{NodeID: "sick", Serving: true, Telemetry: tel(100, gpu(80*giB, 10*giB, 40, 10, "Critical"))},
		{NodeID: "cool", Serving: true, Telemetry: tel(100, gpu(80*giB, 20*giB, 40, 10, "Healthy"))},
	}
	dec := Select(cands, Request{Now: time.Unix(120, 0), Requirement: Requirement{MaxTempC: 80, RequireHealthy: true}})
	if dec.NodeID != "cool" {
		t.Fatalf("chose %q, want cool", dec.NodeID)
	}
	if dec.Eligible != 1 {
		t.Fatalf("eligible = %d, want 1", dec.Eligible)
	}
}

func TestNotServingExcluded(t *testing.T) {
	cands := []Candidate{
		{NodeID: "off", Serving: false, Telemetry: tel(100, gpu(80*giB, 0, 40, 10, "Healthy"))},
		{NodeID: "on", Serving: true, Telemetry: tel(100, gpu(80*giB, 70*giB, 40, 10, "Healthy"))},
	}
	dec := Select(cands, Request{Now: time.Unix(120, 0)})
	if dec.NodeID != "on" {
		t.Fatalf("chose %q, want on", dec.NodeID)
	}
}

func TestStaleTelemetryCannotSatisfyRequirement(t *testing.T) {
	cands := []Candidate{
		{NodeID: "old", Serving: true, Telemetry: tel(10, gpu(80*giB, 0, 40, 10, "Healthy"))},
	}
	req := Request{Now: time.Unix(1000, 0), MaxTelemetryAge: 30 * time.Second, Requirement: Requirement{MinFreeMemBytes: 1}}
	dec := Select(cands, req)
	if dec.NodeID != "" {
		t.Fatalf("chose %q, want none (stale)", dec.NodeID)
	}
	if dec.Ranked[0].Reason != "telemetry stale" {
		t.Fatalf("reason = %q", dec.Ranked[0].Reason)
	}
}

func TestFreshTelemetrySatisfiesRequirement(t *testing.T) {
	cands := []Candidate{
		{NodeID: "fresh", Serving: true, Telemetry: tel(995, gpu(80*giB, 0, 40, 10, "Healthy"))},
	}
	req := Request{Now: time.Unix(1000, 0), MaxTelemetryAge: 30 * time.Second, Requirement: Requirement{MinFreeMemBytes: 1}}
	if dec := Select(cands, req); dec.NodeID != "fresh" {
		t.Fatalf("chose %q, want fresh", dec.NodeID)
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	cands := []Candidate{
		{NodeID: "b", Serving: true, Telemetry: tel(100, gpu(80*giB, 10*giB, 40, 10, "Healthy"))},
		{NodeID: "a", Serving: true, Telemetry: tel(100, gpu(80*giB, 10*giB, 40, 10, "Healthy"))},
	}
	dec := Select(cands, Request{Now: time.Unix(120, 0)})
	if dec.NodeID != "a" {
		t.Fatalf("chose %q, want a (node id tie-break)", dec.NodeID)
	}
}

func TestNoCandidates(t *testing.T) {
	dec := Select(nil, Request{})
	if dec.NodeID != "" || dec.Eligible != 0 {
		t.Fatalf("empty select = %+v", dec)
	}
}

func TestMultipleGPUsAggregate(t *testing.T) {
	cands := []Candidate{
		{NodeID: "twin", Serving: true, Telemetry: tel(100,
			gpu(80*giB, 60*giB, 40, 10, "Healthy"),
			gpu(80*giB, 10*giB, 55, 90, "Healthy"))},
	}
	dec := Select(cands, Request{Now: time.Unix(120, 0)})
	s := dec.Ranked[0]
	if s.FreeMemBytes != 90*giB {
		t.Fatalf("free = %d, want %d", s.FreeMemBytes, 90*giB)
	}
	if s.MaxTempC != 55 {
		t.Fatalf("max temp = %d, want 55", s.MaxTempC)
	}
	if s.MaxUtilPct != 90 {
		t.Fatalf("max util = %d, want 90", s.MaxUtilPct)
	}
}

func TestUnavailableAcceleratorCannotSatisfyRequirement(t *testing.T) {
	absent := &apiv1.GpuTelemetry{
		Available: false, CollectedAtUnix: 100,
		Reason: "dcgm: libdcgm.so.4 not found",
	}
	cands := []Candidate{
		{NodeID: "nogpu", Serving: true, Telemetry: absent},
		{NodeID: "blind", Serving: true},
	}
	dec := Select(cands, Request{Now: time.Unix(120, 0), Requirement: Requirement{MinFreeMemBytes: 1}})
	if dec.NodeID != "" || dec.Eligible != 0 {
		t.Fatalf("chose %q eligible=%d, want none", dec.NodeID, dec.Eligible)
	}
	reasons := map[string]string{}
	for _, s := range dec.Ranked {
		reasons[s.NodeID] = s.Reason
	}
	if reasons["nogpu"] != "accelerator unavailable" {
		t.Fatalf("nogpu reason = %q, want accelerator unavailable", reasons["nogpu"])
	}
	if reasons["blind"] != "no telemetry for requirement" {
		t.Fatalf("blind reason = %q, want no telemetry for requirement", reasons["blind"])
	}
}
