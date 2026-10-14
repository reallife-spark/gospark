package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/bus"
)

// gpuSnapshot is the admin surface's view of the last GPU telemetry per node.
type gpuSnapshot struct {
	Nodes []gpuNodeView `json:"nodes"`
}

type gpuNodeView struct {
	NodeID    string             `json:"node_id"`
	Telemetry apiv1.GpuTelemetry `json:"gpu_telemetry"`
}

// subscribeGPU wires the control plane's GPU-telemetry intake. The sidecar
// publishes a RegisterNode whose health carries only GPUTelemetry; unlike the
// agent handshake this is not checked against the registry digest, because
// telemetry carries no authority — it is recorded for visibility only.
func (a *App) subscribeGPU(ctx context.Context) error {
	if _, err := a.bus.Subscribe(ctx, a.subj(bus.KindHealth), a.onGPUHealth); err != nil {
		return fmt.Errorf("server: subscribe health: %w", err)
	}
	return nil
}

// onGPUHealth records a GPU telemetry report keyed by node id.
func (a *App) onGPUHealth(_ context.Context, _ string, data []byte) error {
	var reg apiv1.RegisterNode
	if err := json.Unmarshal(data, &reg); err != nil {
		return fmt.Errorf("server: health payload: %w", err)
	}
	if reg.Health == nil || reg.Health.GPUTelemetry == nil {
		return nil
	}
	a.gpuMu.Lock()
	if a.gpu == nil {
		a.gpu = map[string]apiv1.GpuTelemetry{}
	}
	a.gpu[reg.Health.NodeID] = *reg.Health.GPUTelemetry
	a.gpuMu.Unlock()
	a.log.Debug("gpu telemetry recorded",
		"node", reg.Health.NodeID,
		"source", reg.Health.GPUTelemetry.Source,
		"available", reg.Health.GPUTelemetry.Available,
		"gpus", len(reg.Health.GPUTelemetry.GPUs))
	return nil
}

// GPUSnapshot returns the last GPU telemetry per node, sorted by node id.
func (a *App) GPUSnapshot() gpuSnapshot {
	a.gpuMu.RLock()
	defer a.gpuMu.RUnlock()
	out := gpuSnapshot{Nodes: make([]gpuNodeView, 0, len(a.gpu))}
	for id, tel := range a.gpu {
		out.Nodes = append(out.Nodes, gpuNodeView{NodeID: id, Telemetry: tel})
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].NodeID < out.Nodes[j].NodeID })
	return out
}
