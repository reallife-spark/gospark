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
	subject := bus.Subject(a.cfg.Bus.SubjectRoot, bus.KindTask, a.id.NodeID)
	if _, err := a.bus.Subscribe(ctx, subject, a.onTask); err != nil {
		return fmt.Errorf("server: subscribe task: %w", err)
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
