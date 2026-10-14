package server

import (
	"net/http"
	"sort"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
)

// healthSnapshot is the control plane's view of every node it knows about: what
// the node last reported, whether the registry still authorizes it, and the last
// GPU snapshot. It joins three sources that are otherwise separate — the health
// registry, the authorization registry and the telemetry map — into one
// read-only observability surface.
type healthSnapshot struct {
	Nodes []healthNodeView `json:"nodes"`
}

type healthNodeView struct {
	NodeID         string   `json:"node_id"`
	Serving        bool     `json:"serving"`
	LoadPermille   uint32   `json:"load_permille"`
	Capabilities   []string `json:"capabilities,omitempty"`
	OverlayAddress string   `json:"overlay_address,omitempty"`
	ReportedAtUnix int64    `json:"reported_at_unix,omitempty"`
	AgeSeconds     int64    `json:"age_seconds,omitempty"`
	// Authorized is nil in open mode (no registry configured), otherwise whether
	// the node is present, unrevoked and unexpired.
	Authorized *bool    `json:"authorized,omitempty"`
	Revoked    bool     `json:"revoked,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	GPU        *gpuView `json:"gpu,omitempty"`
}

type gpuView struct {
	Available       bool   `json:"available"`
	Source          string `json:"source,omitempty"`
	Reason          string `json:"reason,omitempty"`
	GPUs            int    `json:"gpus"`
	FreeMemBytes    uint64 `json:"free_mem_bytes,omitempty"`
	MaxTempC        uint32 `json:"max_temp_c,omitempty"`
	CollectedAtUnix int64  `json:"collected_at_unix,omitempty"`
	AgeSeconds      int64  `json:"age_seconds,omitempty"`
}

// healthSnapshot assembles the view. The node set is the union of the health
// registry, the telemetry map and (in fail-closed mode) the authorization
// registry, so a node that has reported telemetry but not health, or is
// authorized but silent, still appears.
func (a *App) healthSnapshot() healthSnapshot {
	now := time.Now()
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}

	if a.registry != nil {
		for _, id := range a.registry.IDs() {
			add(id)
		}
	}
	a.gpuMu.RLock()
	for id := range a.gpu {
		add(id)
	}
	a.gpuMu.RUnlock()
	a.regMu.RLock()
	reg := a.nodeRegistry
	a.regMu.RUnlock()
	if reg != nil {
		for _, id := range reg.IDs() {
			add(id)
		}
	}
	sort.Strings(ids)

	out := healthSnapshot{Nodes: make([]healthNodeView, 0, len(ids))}
	for _, id := range ids {
		out.Nodes = append(out.Nodes, a.healthNode(id, reg, now))
	}
	return out
}

func (a *App) healthNode(id string, reg *authz.Registry, now time.Time) healthNodeView {
	v := healthNodeView{NodeID: id}

	if h, ok := a.registry.Get(id); ok {
		v.Serving = h.Serving
		v.LoadPermille = h.LoadPermille
		v.Capabilities = h.Capabilities
		v.OverlayAddress = h.OverlayAddress
		v.ReportedAtUnix = h.ReportedAtUnix
		if h.ReportedAtUnix > 0 {
			v.AgeSeconds = now.Unix() - h.ReportedAtUnix
			if v.AgeSeconds < 0 {
				v.AgeSeconds = 0
			}
		}
	}

	var g *gpuView
	if t, ok := a.gpuTelemetry(id); ok {
		g = gpuSummary(t, now)
	} else if h, ok := a.registry.Get(id); ok && h.GPUTelemetry != nil {
		g = gpuSummary(h.GPUTelemetry, now)
	}
	v.GPU = g

	if reg != nil {
		entry, ok := reg.Get(id)
		authorized := false
		if ok {
			v.Revoked = entry.Revoked
			v.ExpiresAt = entry.ExpiresAt
			authorized = !entry.Revoked && !expired(entry.ExpiresAt, now)
		} else {
			v.Revoked = false
		}
		v.Authorized = &authorized
	}
	return v
}

// gpuSummary reduces a telemetry snapshot to the numbers an operator wants.
func gpuSummary(t *apiv1.GpuTelemetry, now time.Time) *gpuView {
	if t == nil {
		return nil
	}
	v := &gpuView{
		Available:       t.Available,
		Source:          t.Source,
		Reason:          t.Reason,
		GPUs:            len(t.GPUs),
		CollectedAtUnix: t.CollectedAtUnix,
	}
	if t.CollectedAtUnix > 0 {
		v.AgeSeconds = now.Unix() - t.CollectedAtUnix
		if v.AgeSeconds < 0 {
			v.AgeSeconds = 0
		}
	}
	for _, g := range t.GPUs {
		if g.MemTotalBytes > g.MemUsedBytes {
			v.FreeMemBytes += g.MemTotalBytes - g.MemUsedBytes
		}
		if g.TempC > v.MaxTempC {
			v.MaxTempC = g.TempC
		}
	}
	return v
}

// expired reports whether an RFC3339 timestamp is in the past. An unparseable
// timestamp is treated as not expired so a formatting quirk never hides a node.
func expired(ts string, now time.Time) bool {
	if ts == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return false
		}
	}
	return now.After(t)
}

// handleAdminHealth serves GET /admin/health: a read-only join of node health,
// authorization state and GPU telemetry.
func (a *App) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeAdminJSON(w, http.StatusOK, a.healthSnapshot())
}
