package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func render(t *testing.T, snap Snapshot) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, snap); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.String()
}

func TestWriteCounters(t *testing.T) {
	out := render(t, Snapshot{
		Build:  "1.2.3",
		Role:   "control",
		NodeID: "ctrl-1",
		Counters: Counters{
			TasksVerified:  3,
			BlocksStreamed: 5,
		},
	})
	for _, want := range []string{
		"# TYPE gospark_tasks_verified_total counter",
		"gospark_tasks_verified_total 3",
		"gospark_blocks_streamed_total 5",
		"gospark_tasks_refused_total 0",
		`gospark_build_info{node="ctrl-1",role="control",version="1.2.3"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestWriteNodeAndGPU(t *testing.T) {
	yes := true
	out := render(t, Snapshot{
		Nodes: []Node{{
			NodeID:       "node-a",
			Serving:      true,
			LoadPermille: 420,
			Authorized:   &yes,
			GPU: &GPU{
				Available: true,
				Devices: []Device{{
					Index:           0,
					Name:            "NVIDIA GB10",
					TempC:           61,
					MemUsedBytes:    1024,
					MemTotalBytes:   2048,
					UtilPct:         77,
					MemUtilPct:      55,
					PowerMilliwatts: 2500,
					Health:          "Healthy",
				}},
			},
		}},
	})
	for _, want := range []string{
		`gospark_node_serving{node="node-a"} 1`,
		`gospark_node_load_permille{node="node-a"} 420`,
		`gospark_node_authorized{node="node-a"} 1`,
		`gospark_node_gpu_available{node="node-a"} 1`,
		`gospark_gpu_memory_used_bytes{gpu="0",name="NVIDIA GB10",node="node-a"} 1024`,
		`gospark_gpu_utilization_percent{gpu="0",name="NVIDIA GB10",node="node-a"} 77`,
		`gospark_gpu_power_watts{gpu="0",name="NVIDIA GB10",node="node-a"} 2.5`,
		`gospark_gpu_healthy{gpu="0",name="NVIDIA GB10",node="node-a"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestWriteOmitsUnauthorizedAndUnavailable(t *testing.T) {
	out := render(t, Snapshot{
		Nodes: []Node{{
			NodeID:  "node-b",
			Serving: false,
			// Authorized nil: open mode, so the metric is absent.
			GPU: &GPU{Available: false},
		}},
	})
	if strings.Contains(out, "gospark_node_authorized") {
		t.Errorf("authorized metric should be absent in open mode:\n%s", out)
	}
	if !strings.Contains(out, `gospark_node_serving{node="node-b"} 0`) {
		t.Errorf("serving = 0 metric missing:\n%s", out)
	}
	if !strings.Contains(out, `gospark_node_gpu_available{node="node-b"} 0`) {
		t.Errorf("gpu available = 0 metric missing:\n%s", out)
	}
	if strings.Contains(out, "gospark_gpu_memory_used_bytes") {
		t.Errorf("device metrics should be absent when no devices:\n%s", out)
	}
}

func TestWriteEscapesLabelValues(t *testing.T) {
	out := render(t, Snapshot{
		Nodes: []Node{{
			NodeID: `we"ird\name`,
			GPU: &GPU{Devices: []Device{{
				Index:        0,
				Name:         "line\nbreak",
				MemUsedBytes: 1,
			}}},
		}},
	})
	if !strings.Contains(out, `gospark_node_serving{node="we\"ird\\name"} 0`) {
		t.Errorf("node label not escaped:\n%s", out)
	}
	if !strings.Contains(out, `name="line\nbreak"`) {
		t.Errorf("device name label not escaped:\n%s", out)
	}
}

func TestWriteLabelOrderDeterministic(t *testing.T) {
	snap := func() Snapshot {
		return Snapshot{Nodes: []Node{{
			NodeID: "n",
			GPU:    &GPU{Devices: []Device{{Index: 1, Name: "g", MemUsedBytes: 9}}},
		}}}
	}
	if a, b := render(t, snap()), render(t, snap()); a != b {
		t.Fatalf("non-deterministic output:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}
