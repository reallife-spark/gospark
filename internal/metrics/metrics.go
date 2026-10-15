// Package metrics renders goSpark's operational state in the Prometheus text
// exposition format (version 0.0.4) — the `/metrics` surface the seed blueprint
// asks for ("Expose a /metrics route ... tracking GPU memory consumption, core
// utilization percentages, and API request latency").
//
// It is pure Go with no vendor client: the format is a handful of lines, and
// pulling in a client library would put a schema and a dependency inside the
// control plane for no gain. The renderer takes a plain-value Snapshot, so the
// policy of what to expose lives in `internal/server` and this package only
// formats.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ContentType is the exposition media type Prometheus sends.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Counters is the control-plane counter set (a plain copy of
// telemetry.Snapshot so this package does not import the server's world).
type Counters struct {
	TasksVerified   uint64
	TasksRefused    uint64
	ResultsAttested uint64
	BlocksStreamed  uint64
	BlocksReceived  uint64
	BytesStreamed   uint64
}

// Snapshot is everything /metrics exposes.
type Snapshot struct {
	// Build is the version string reported by gospark_build_info.
	Build string
	// Role is "control" or "agent".
	Role string
	// NodeID is the local node's id.
	NodeID   string
	Counters Counters
	Nodes    []Node
}

// Node is one node's health and capacity.
type Node struct {
	NodeID       string
	Serving      bool
	LoadPermille uint32
	// Authorized is nil in open mode (no registry configured), otherwise
	// whether the registry still authorizes the node.
	Authorized *bool
	GPU        *GPU
}

// GPU is a node's last accelerator telemetry.
type GPU struct {
	Available bool
	Devices   []Device
}

// Device is one accelerator's reading.
type Device struct {
	Index           uint32
	Name            string
	TempC           uint32
	MemUsedBytes    uint64
	MemTotalBytes   uint64
	UtilPct         uint32
	MemUtilPct      uint32
	PowerMilliwatts uint32
	Health          string
}

// Write renders the snapshot in the text exposition format.
func Write(w io.Writer, snap Snapshot) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	build := snap.Build
	if build == "" {
		build = "unknown"
	}
	help(bw, "gospark_build_info", "Build and role of this goSpark process.", "gauge")
	metric(bw, "gospark_build_info", labels{"version": build, "role": snap.Role, "node": snap.NodeID}, 1)
	bw.WriteByte('\n')

	// Counters: every one is monotonic, so they are `counter` typed.
	counters := []struct {
		name string
		help string
		val  uint64
	}{
		{"gospark_tasks_verified_total", "Tasks verified against the master key.", snap.Counters.TasksVerified},
		{"gospark_tasks_refused_total", "Tasks refused (bad signature, replay, missing capability).", snap.Counters.TasksRefused},
		{"gospark_results_attested_total", "Results attested and accepted by the control plane.", snap.Counters.ResultsAttested},
		{"gospark_blocks_streamed_total", "KV blocks streamed out over the data plane.", snap.Counters.BlocksStreamed},
		{"gospark_blocks_received_total", "KV blocks received over the data plane.", snap.Counters.BlocksReceived},
		{"gospark_bytes_streamed_total", "Packed KV bytes moved over the data plane.", snap.Counters.BytesStreamed},
	}
	for _, c := range counters {
		help(bw, c.name, c.help, "counter")
		metric(bw, c.name, nil, c.val)
	}
	bw.WriteByte('\n')

	if len(snap.Nodes) > 0 {
		hasAuthorized, hasGPU, hasDevices := false, false, false
		for _, n := range snap.Nodes {
			if n.Authorized != nil {
				hasAuthorized = true
			}
			if n.GPU != nil {
				hasGPU = true
				if len(n.GPU.Devices) > 0 {
					hasDevices = true
				}
			}
		}

		help(bw, "gospark_node_serving", "Whether the node last reported itself serving (1) or not (0).", "gauge")
		help(bw, "gospark_node_load_permille", "The node's last reported load, in permille of capacity.", "gauge")
		if hasAuthorized {
			help(bw, "gospark_node_authorized", "Whether the registry still authorizes the node (1) or not (0).", "gauge")
		}
		if hasGPU {
			help(bw, "gospark_node_gpu_available", "Whether the node last reported an available accelerator (1) or not (0).", "gauge")
		}
		if hasDevices {
			help(bw, "gospark_gpu_memory_used_bytes", "Accelerator memory in use, in bytes.", "gauge")
			help(bw, "gospark_gpu_memory_total_bytes", "Accelerator memory total, in bytes.", "gauge")
			help(bw, "gospark_gpu_utilization_percent", "Accelerator core utilization, in percent.", "gauge")
			help(bw, "gospark_gpu_memory_utilization_percent", "Accelerator memory-controller utilization, in percent.", "gauge")
			help(bw, "gospark_gpu_temperature_celsius", "Accelerator temperature, in degrees Celsius.", "gauge")
			help(bw, "gospark_gpu_power_watts", "Accelerator power draw, in watts.", "gauge")
			help(bw, "gospark_gpu_healthy", "Whether the accelerator's health verdict is healthy (1) or not (0).", "gauge")
		}

		for _, n := range snap.Nodes {
			nl := labels{"node": n.NodeID}
			metric(bw, "gospark_node_serving", nl, boolVal(n.Serving))
			metric(bw, "gospark_node_load_permille", nl, n.LoadPermille)
			if n.Authorized != nil {
				metric(bw, "gospark_node_authorized", nl, boolVal(*n.Authorized))
			}
			if n.GPU == nil {
				continue
			}
			metric(bw, "gospark_node_gpu_available", nl, boolVal(n.GPU.Available))
			for _, d := range n.GPU.Devices {
				dl := labels{"node": n.NodeID, "gpu": strconv.FormatUint(uint64(d.Index), 10), "name": d.Name}
				if d.MemUsedBytes > 0 {
					metric(bw, "gospark_gpu_memory_used_bytes", dl, d.MemUsedBytes)
				}
				if d.MemTotalBytes > 0 {
					metric(bw, "gospark_gpu_memory_total_bytes", dl, d.MemTotalBytes)
				}
				if d.UtilPct > 0 {
					metric(bw, "gospark_gpu_utilization_percent", dl, d.UtilPct)
				}
				if d.MemUtilPct > 0 {
					metric(bw, "gospark_gpu_memory_utilization_percent", dl, d.MemUtilPct)
				}
				if d.TempC > 0 {
					metric(bw, "gospark_gpu_temperature_celsius", dl, d.TempC)
				}
				if d.PowerMilliwatts > 0 {
					metric(bw, "gospark_gpu_power_watts", dl, floatVal(float64(d.PowerMilliwatts)/1000.0))
				}
				if d.Health != "" {
					metric(bw, "gospark_gpu_healthy", dl, boolVal(strings.EqualFold(d.Health, "healthy")))
				}
			}
		}
	}
	return bw.Flush()
}

type labels map[string]string

func help(w *bufio.Writer, name, text, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, escapeHelp(text))
	fmt.Fprintf(w, "# TYPE %s %s\n", name, typ)
}

func metric(w *bufio.Writer, name string, lbl labels, value any) {
	io.WriteString(w, name)
	if len(lbl) > 0 {
		keys := make([]string, 0, len(lbl))
		for k := range lbl {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				w.WriteByte(',')
			}
			w.WriteString(k)
			w.WriteByte('=')
			w.WriteByte('"')
			w.WriteString(escapeLabel(lbl[k]))
			w.WriteByte('"')
		}
		w.WriteByte('}')
	}
	switch v := value.(type) {
	case uint64:
		fmt.Fprintf(w, " %d\n", v)
	case uint32:
		fmt.Fprintf(w, " %d\n", v)
	case float64:
		fmt.Fprintf(w, " %s\n", strconv.FormatFloat(v, 'g', -1, 64))
	default:
		fmt.Fprintf(w, " %v\n", v)
	}
}

func boolVal(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

func floatVal(f float64) float64 { return f }

func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	return strings.ReplaceAll(s, "\n", "\\n")
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return strings.ReplaceAll(s, "\"", "\\\"")
}
