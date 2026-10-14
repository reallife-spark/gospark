package server

import (
	"context"
	"crypto/ed25519"
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

	if err := a.setupRegistry(ctx); err != nil {
		return err
	}

	if _, err := a.bus.Subscribe(ctx, a.subj(bus.KindRegister), a.onRegister); err != nil {
		return fmt.Errorf("server: subscribe register: %w", err)
	}
	if _, err := a.bus.Subscribe(ctx, a.subj(bus.KindResult), a.onResult); err != nil {
		return fmt.Errorf("server: subscribe result: %w", err)
	}
	a.log.Info("control plane ready", "node", a.id.NodeID, "root", a.cfg.Bus.SubjectRoot)
	return nil
}

// setupRegistry signs and publishes the authoritative node registry, then
// derives the executor public keys from it. A control plane with no registry
// runs in open mode (any node may be dispatched to); production deployments
// supply one via Options.Registry and the empty-by-default rule applies.
func (a *App) setupRegistry(ctx context.Context) error {
	if a.registryDoc == nil {
		a.log.Warn("no node registry configured; running in open mode (not fail-closed)")
		return nil
	}
	// Sign and install under the registry lock: peers (and RegistryDigest) read
	// the document concurrently, and SignRegistry mutates it in place.
	a.regMu.Lock()
	// Stamp a fresh issue time so peers' rollback guards advance monotonically.
	// An explicit issue time loaded from file is respected.
	if a.registryDoc.IssuedAtUnix == 0 {
		a.registryDoc.IssuedAtUnix = time.Now().Unix()
	}
	if err := authz.SignRegistry(a.id, a.registryDoc); err != nil {
		a.regMu.Unlock()
		return fmt.Errorf("server: sign registry: %w", err)
	}
	a.nodeRegistry = authz.NewRegistry(a.registryDoc)
	reg := a.nodeRegistry
	digest, err := authz.RegistryDigest(a.registryDoc)
	a.regMu.Unlock()
	if err != nil {
		return fmt.Errorf("server: registry digest: %w", err)
	}

	// Merge registry-derived keys over any explicitly supplied executor keys.
	pubs, err := reg.ExecutorPublicKeys(time.Now())
	if err != nil {
		return err
	}
	if a.executorPubs == nil {
		a.executorPubs = map[string]ed25519.PublicKey{}
	}
	for id, pub := range pubs {
		a.executorPubs[id] = pub
	}

	if err := a.publishRegistry(ctx); err != nil {
		return err
	}
	a.log.Info("registry published",
		"nodes", len(reg.IDs()),
		"digest", short(digest),
		"master", a.id.NodeID)
	return nil
}

// publishRegistry sends the signed registry to peers on the registry subject.
func (a *App) publishRegistry(ctx context.Context) error {
	a.regMu.RLock()
	doc := a.registryDoc
	if doc == nil {
		a.regMu.RUnlock()
		return nil
	}
	data, err := json.Marshal(doc)
	a.regMu.RUnlock()
	if err != nil {
		return fmt.Errorf("server: marshal registry: %w", err)
	}
	return a.bus.Publish(ctx, a.subj(bus.KindRegistry), data)
}

// ReSignRegistry updates the authorizations and re-publishes a freshly signed
// registry — the distribute-registry operation. Bumping IssuedAtUnix is what
// lets peers accept the change and refuse an older rollback.
func (a *App) ReSignRegistry(ctx context.Context, authorizations []apiv1.NodeAuthorization) (string, error) {
	a.regMu.Lock()
	if a.registryDoc == nil {
		a.regMu.Unlock()
		return "", fmt.Errorf("server: no registry configured to re-sign")
	}
	a.registryDoc.Authorizations = authorizations
	a.registryDoc.IssuedAtUnix = time.Now().Unix()
	if err := authz.SignRegistry(a.id, a.registryDoc); err != nil {
		a.regMu.Unlock()
		return "", err
	}
	doc := a.registryDoc
	a.nodeRegistry = authz.NewRegistry(doc)
	digest, err := authz.RegistryDigest(doc)
	a.regMu.Unlock()
	if err != nil {
		return "", err
	}
	if err := a.publishRegistry(ctx); err != nil {
		return "", err
	}
	a.log.Info("registry re-signed", "nodes", len(authorizations), "digest", short(digest))
	return digest, nil
}

// RegistryDigest returns the digest of the currently served registry, or "" in
// open mode. The digest is computed under the read lock because it reads the
// document, which ReSignRegistry may be mutating.
func (a *App) RegistryDigest() string {
	a.regMu.RLock()
	defer a.regMu.RUnlock()
	if a.registryDoc == nil {
		return ""
	}
	d, _ := authz.RegistryDigest(a.registryDoc)
	return d
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func (a *App) subj(kind string) string {
	return bus.Subject(a.cfg.Bus.SubjectRoot, kind, "")
}

func (a *App) onRegister(ctx context.Context, _ string, data []byte) error {
	var reg apiv1.RegisterNode
	if err := json.Unmarshal(data, &reg); err != nil {
		return fmt.Errorf("server: register payload: %w", err)
	}
	if reg.Health == nil {
		return fmt.Errorf("server: register without health")
	}
	if want := a.RegistryDigest(); want != "" && reg.RegistryDigest != want {
		// The node adopted a different (stale, missing, or forged) registry.
		// Re-publish ours so a peer that subscribed late picks it up; record its
		// health for visibility, but do not treat it as current.
		a.counters.TasksRefused.Add(1)
		a.log.Warn("node registry stale; re-publishing registry",
			"node", reg.Health.NodeID,
			"node_digest", short(reg.RegistryDigest),
			"control_digest", short(want))
		if err := a.publishRegistry(ctx); err != nil {
			a.log.Warn("registry re-publish failed", "err", err)
		}
		return nil
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
	// A valid signature only proves provenance; it does not prove the node was
	// still authorized when it computed. Check the registry too.
	if err := a.authorizeNode(env.NodeID, ""); err != nil {
		a.counters.TasksRefused.Add(1)
		a.log.Warn("result refused: node not authorized", "node", env.NodeID, "err", err)
		return nil
	}
	a.counters.ResultsAttested.Add(1)
	a.log.Info("result attested", "node", env.NodeID, "task", env.TaskID, "result", env.Result)
	return nil
}

// authorizeNode enforces the registry on the control side. In open mode (no
// registry) it is a no-op.
func (a *App) authorizeNode(nodeID string, cap authz.Capability) error {
	a.regMu.RLock()
	reg := a.nodeRegistry
	a.regMu.RUnlock()
	if reg == nil {
		return nil
	}
	return reg.Authorized(nodeID, cap, time.Now())
}

// DispatchTask publishes a signed task to a node's task subject. The control
// plane does not wait: the result arrives asynchronously on the result subject,
// which is the async boundary the design requires over a WAN.
func (a *App) DispatchTask(ctx context.Context, nodeID string, env apiv1.TaskEnvelope) error {
	if err := a.authorizeNode(nodeID, authz.CapClusterServe); err != nil {
		a.counters.TasksRefused.Add(1)
		a.log.Warn("dispatch refused: node not authorized", "node", nodeID, "err", err)
		return err
	}
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
	reg := apiv1.RegisterNode{Health: &h, RegistryDigest: a.RegistryDigest()}
	data, err := json.Marshal(reg)
	if err != nil {
		return err
	}
	return a.bus.Publish(ctx, a.subj(bus.KindRegister), data)
}
