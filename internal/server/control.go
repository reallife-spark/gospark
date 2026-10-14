package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/bus"
)

// Registry holds the last health report per node.
type Registry struct {
	mu    sync.RWMutex
	nodes map[string]apiv1.NodeHealth
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{nodes: make(map[string]apiv1.NodeHealth)}
}

// Put stores a health report.
func (r *Registry) Put(h apiv1.NodeHealth) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[h.NodeID] = h
}

// Get returns a node's report.
func (r *Registry) Get(id string) (apiv1.NodeHealth, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.nodes[id]
	return h, ok
}

// IDs returns known node ids.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.nodes))
	for id := range r.nodes {
		out = append(out, id)
	}
	return out
}

// runControl wires the control-plane subscriptions.
func (a *App) runControl(ctx context.Context) error {
	a.registry = NewRegistry()

	if _, err := a.bus.Subscribe(ctx, a.subj(bus.KindRegister), a.onRegister); err != nil {
		return fmt.Errorf("server: subscribe register: %w", err)
	}
	if _, err := a.bus.Subscribe(ctx, a.subj(bus.KindResult), a.onResult); err != nil {
		return fmt.Errorf("server: subscribe result: %w", err)
	}
	a.log.Info("control plane ready", "node", a.id.NodeID, "root", a.cfg.Bus.SubjectRoot)
	return nil
}

func (a *App) subj(kind string) string {
	return bus.Subject(a.cfg.Bus.SubjectRoot, kind, "")
}

func (a *App) onRegister(_ context.Context, _ string, data []byte) error {
	var reg apiv1.RegisterNode
	if err := json.Unmarshal(data, &reg); err != nil {
		return fmt.Errorf("server: register payload: %w", err)
	}
	if reg.Health == nil {
		return fmt.Errorf("server: register without health")
	}
	a.registry.Put(*reg.Health)
	a.log.Info("node registered", "node", reg.Health.NodeID, "serving", reg.Health.Serving, "caps", reg.Health.Capabilities)
	return nil
}

func (a *App) onResult(_ context.Context, _ string, data []byte) error {
	var env apiv1.ResultEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("server: result payload: %w", err)
	}
	pub, ok := a.executorPubs[env.NodeID]
	if !ok {
		a.counters.TasksRefused.Add(1)
		a.log.Warn("result from unknown executor", "node", env.NodeID)
		return nil
	}
	if err := authz.VerifyResult(pub, &env); err != nil {
		a.counters.TasksRefused.Add(1)
		a.log.Warn("result signature refused", "node", env.NodeID, "err", err)
		return nil
	}
	a.counters.ResultsAttested.Add(1)
	a.log.Info("result attested", "node", env.NodeID, "task", env.TaskID, "result", env.Result)
	return nil
}

// DispatchTask publishes a signed task to a node's task subject. The control
// plane does not wait: the result arrives asynchronously on the result subject,
// which is the async boundary the design requires over a WAN.
func (a *App) DispatchTask(ctx context.Context, nodeID string, env apiv1.TaskEnvelope) error {
	env.ContentInSignature = true
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	subject := bus.Subject(a.cfg.Bus.SubjectRoot, bus.KindTask, nodeID)
	a.log.Info("dispatching task", "node", nodeID, "task", env.TaskID, "op", env.Operation)
	return a.bus.Publish(ctx, subject, data)
}

// PublishHealth sends this node's health to the control plane.
func (a *App) PublishHealth(ctx context.Context, serving bool) error {
	h := apiv1.NodeHealth{
		NodeID:         a.id.NodeID,
		Fingerprint:    a.id.Fingerprint(),
		OverlayAddress: a.cfg.Node.OverlayAddress,
		Capabilities:   a.cfg.Node.Capabilities,
		Serving:        serving,
		ReportedAtUnix: time.Now().Unix(),
	}
	reg := apiv1.RegisterNode{Health: &h}
	data, err := json.Marshal(reg)
	if err != nil {
		return err
	}
	return a.bus.Publish(ctx, a.subj(bus.KindRegister), data)
}
