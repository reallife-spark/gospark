package server

import (
	"net/http"

	"github.com/reallife-spark/goSpark/internal/metrics"
)

// Version is the goSpark build version reported by /metrics. A build may stamp
// it with -ldflags "-X github.com/reallife-spark/goSpark/internal/server.Version=...".
var Version = "dev"

// handleAdminMetrics serves GET /admin/metrics: the Prometheus text-exposition
// view. It is read-only, like every other /admin route.
func (a *App) handleAdminMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", metrics.ContentType)
	_ = metrics.Write(w, a.metricsSnapshot())
}

// metricsHandler is the standalone scrape surface: /metrics plus a trivial
// /healthz. It carries no authority, so it needs no loopback guard — the
// operator decides where it listens.
func (a *App) metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", a.handleAdminMetrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// metricsSnapshot projects the App's state into the metrics Snapshot. It reuses
// healthSnapshot so the node set and authorization verdicts are identical to
// /admin/health, then attaches the full per-device accelerator telemetry.
func (a *App) metricsSnapshot() metrics.Snapshot {
	cs := a.counters.Snapshot()
	snap := metrics.Snapshot{
		Build:  Version,
		Role:   string(a.role),
		NodeID: a.id.NodeID,
		Counters: metrics.Counters{
			TasksVerified:   cs.TasksVerified,
			TasksRefused:    cs.TasksRefused,
			ResultsAttested: cs.ResultsAttested,
			BlocksStreamed:  cs.BlocksStreamed,
			BlocksReceived:  cs.BlocksReceived,
			BytesStreamed:   cs.BytesStreamed,
		},
	}
	hs := a.healthSnapshot()
	snap.Nodes = make([]metrics.Node, 0, len(hs.Nodes))
	for _, n := range hs.Nodes {
		snap.Nodes = append(snap.Nodes, metrics.Node{
			NodeID:       n.NodeID,
			Serving:      n.Serving,
			LoadPermille: n.LoadPermille,
			Authorized:   n.Authorized,
			GPU:          a.metricsGPU(n.NodeID, n.GPU),
		})
	}
	return snap
}

// metricsGPU prefers the full telemetry snapshot; it falls back to the reduced
// summary (available/unavailable) when only that is known.
func (a *App) metricsGPU(nodeID string, summary *gpuView) *metrics.GPU {
	if t, ok := a.gpuTelemetry(nodeID); ok {
		g := &metrics.GPU{Available: t.Available}
		for _, d := range t.GPUs {
			g.Devices = append(g.Devices, metrics.Device{
				Index:           d.Index,
				Name:            d.Name,
				TempC:           d.TempC,
				MemUsedBytes:    d.MemUsedBytes,
				MemTotalBytes:   d.MemTotalBytes,
				UtilPct:         d.UtilPct,
				MemUtilPct:      d.MemUtilPct,
				PowerMilliwatts: d.PowerMilliwatts,
				Health:          d.Health,
			})
		}
		return g
	}
	if summary != nil {
		return &metrics.GPU{Available: summary.Available}
	}
	return nil
}
