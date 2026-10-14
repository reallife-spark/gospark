package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/bus"
	"github.com/reallife-spark/goSpark/internal/identity"
)

// runAgent wires the node-side subscriptions and heartbeats.
func (a *App) runAgent(ctx context.Context) error {
	// Adopt the registry, if one was supplied directly, before serving.
	if a.registryDoc != nil {
		if err := a.adoptRegistry(a.registryDoc); err != nil {
			return fmt.Errorf("server: adopt supplied registry: %w", err)
		}
	}

	subject := bus.Subject(a.cfg.Bus.SubjectRoot, bus.KindTask, a.id.NodeID)
	if _, err := a.bus.Subscribe(ctx, subject, a.onTask); err != nil {
		return fmt.Errorf("server: subscribe task: %w", err)
	}
	if _, err := a.bus.Subscribe(ctx, a.subj(bus.KindRegistry), a.onRegistry); err != nil {
		return fmt.Errorf("server: subscribe registry: %w", err)
	}
	a.log.Info("agent ready", "node", a.id.NodeID, "subject", subject, "ops", OperationNames())

	// Heartbeat health immediately and then on a slow cadence.
	if err := a.PublishHealth(ctx, true); err != nil {
		a.log.Warn("health publish failed", "err", err)
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = a.PublishHealth(ctx, true)
			}
		}
	}()
	return nil
}

// onRegistry adopts a Master-signed registry from the bus. It refuses a bad
// signature, an unknown capability, or an older registry than the one already
// adopted (rollback).
func (a *App) onRegistry(_ context.Context, _ string, data []byte) error {
	var doc apiv1.NodeRegistry
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("server: registry payload: %w", err)
	}
	if err := a.adoptRegistry(&doc); err != nil {
		a.log.Warn("registry refused", "err", err)
		return err
	}
	// Re-announce health so the control plane sees the new digest.
	if err := a.PublishHealth(context.Background(), true); err != nil {
		a.log.Warn("health re-publish failed", "err", err)
	}
	return nil
}

// adoptRegistry verifies a registry against the pinned master key and installs
// it, enforcing the monotonic issue time. It is safe to call concurrently with
// task handling.
func (a *App) adoptRegistry(doc *apiv1.NodeRegistry) error {
	if err := authz.VerifyRegistry(a.masterPub, doc); err != nil {
		return err
	}
	a.regMu.Lock()
	if a.registryDoc != nil && doc.IssuedAtUnix < a.registryDoc.IssuedAtUnix {
		adopted := a.registryDoc.IssuedAtUnix
		a.regMu.Unlock()
		return fmt.Errorf("%w: incoming=%d adopted=%d", authz.ErrRegistryRollback, doc.IssuedAtUnix, adopted)
	}
	a.registryDoc = doc
	a.nodeRegistry = authz.NewRegistry(doc)
	a.regMu.Unlock()

	digest, _ := authz.RegistryDigest(doc)
	a.log.Info("registry adopted", "nodes", len(doc.Authorizations), "issued", doc.IssuedAtUnix, "digest", short(digest))

	if !a.registryAllowsSelf() {
		a.log.Error("this node is revoked or absent in the adopted registry; refusing to serve", "node", a.id.NodeID)
	}
	return nil
}

// registryAllowsSelf reports whether the adopted registry authorizes this node
// for delegated compute. With no registry adopted it is permissive (legacy /
// single-process mode).
func (a *App) registryAllowsSelf() bool {
	a.regMu.RLock()
	reg := a.nodeRegistry
	a.regMu.RUnlock()
	if reg == nil {
		return true
	}
	return reg.Authorized(a.id.NodeID, authz.CapClusterServe, time.Now()) == nil
}

// onTask verifies and executes a signed task, then publishes the attested
// result. Every refusal path returns an error to the bus (a NAK), so a poison
// message is retried or dead-lettered rather than silently dropped.
func (a *App) onTask(_ context.Context, _ string, data []byte) error {
	var env apiv1.TaskEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		a.counters.TasksRefused.Add(1)
		return fmt.Errorf("server: task payload: %w", err)
	}
	// The gating capability for delegated compute. compute-serve was folded
	// into cluster-serve in the fabric vocabulary; goSpark follows that.
	if err := a.verifier.VerifyTask(a.masterPub, &env, authz.CapClusterServe); err != nil {
		a.counters.TasksRefused.Add(1)
		a.log.Warn("task refused", "task", env.TaskID, "err", err)
		return err
	}
	if !a.holdsCapability(authz.CapClusterServe) {
		a.counters.TasksRefused.Add(1)
		a.log.Warn("task refused: node does not hold capability", "cap", authz.CapClusterServe)
		return fmt.Errorf("server: node does not hold %s", authz.CapClusterServe)
	}
	// The registry is the ceiling: a locally held capability the registry never
	// granted does not authorize the work.
	if !a.registryAllowsSelf() {
		a.counters.TasksRefused.Add(1)
		a.log.Warn("task refused: not authorized by registry", "node", a.id.NodeID)
		return fmt.Errorf("server: node %s not authorized by registry", a.id.NodeID)
	}

	result, err := Execute(env.Operation, env.ParamsJSON)
	if err != nil {
		a.counters.TasksRefused.Add(1)
		a.log.Warn("task execution failed", "task", env.TaskID, "op", env.Operation, "err", err)
		return err
	}
	a.counters.TasksVerified.Add(1)

	attested, err := a.attestResult(env, result)
	if err != nil {
		return err
	}
	return a.publishResult(context.Background(), attested)
}

func (a *App) attestResult(task apiv1.TaskEnvelope, result string) (apiv1.ResultEnvelope, error) {
	env := apiv1.ResultEnvelope{
		TaskID:        task.TaskID,
		NodeID:        a.id.NodeID,
		Fingerprint:   a.id.Fingerprint(),
		Operation:     task.Operation,
		Result:        result,
		CompletedUnix: time.Now().Unix(),
	}
	msg, err := apiv1.CanonicalBytes(&env)
	if err != nil {
		return env, fmt.Errorf("server: canonicalise result: %w", err)
	}
	sig, err := a.id.Sign(msg)
	if err != nil {
		return env, fmt.Errorf("server: sign result: %w", err)
	}
	env.SignatureB64 = identity.EncodeSignature(sig)
	return env, nil
}

func (a *App) publishResult(ctx context.Context, env apiv1.ResultEnvelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	a.log.Info("result published", "task", env.TaskID, "result", env.Result)
	return a.bus.Publish(ctx, a.subj(bus.KindResult), data)
}

func (a *App) holdsCapability(c authz.Capability) bool {
	for _, held := range a.cfg.Node.Capabilities {
		if held == string(c) {
			return true
		}
	}
	return false
}
