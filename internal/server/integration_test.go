package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/bus"
	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/identity"
)

// TestControlAgentEndToEnd exercises the full path a task takes: the control
// plane signs it, the agent verifies and executes it, signs the result, and the
// control plane attests that result. One in-memory bus is shared so this runs
// with no external services.
func TestControlAgentEndToEnd(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()

	controlID, err := identity.Generate("control", identity.RoleMaster)
	if err != nil {
		t.Fatal(err)
	}
	agentID, err := identity.Generate("node-a", identity.RolePeer)
	if err != nil {
		t.Fatal(err)
	}

	controlCfg := config.Default()
	controlCfg.Gateway.Listen = "127.0.0.1:0"
	controlCfg.Transport.Listen = "127.0.0.1:0"

	agentCfg := config.Default()
	agentCfg.Gateway.Listen = "127.0.0.1:0"
	agentCfg.Transport.Listen = "127.0.0.1:0"
	agentCfg.Node.Capabilities = []string{string(authz.CapClusterServe)}

	executors := map[string]ed25519.PublicKey{agentID.NodeID: agentID.Public}

	control, err := New(Options{
		Config:         controlCfg,
		Identity:       controlID,
		Role:           RoleControl,
		ExecutorPublic: executors,
		Bus:            shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{
		Config:       agentCfg,
		Identity:     agentID,
		Role:         RoleAgent,
		MasterPublic: controlID.Public,
		Bus:          shared,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = control.Run(ctx) }()
	go func() { _ = agent.Run(ctx) }()

	// Wait until the agent is subscribed: it publishes health only after it has
	// registered its task subscription.
	health := make(chan apiv1.NodeHealth, 1)
	if _, err := shared.Subscribe(ctx, bus.Subject("sparknet", bus.KindRegister, ""),
		func(_ context.Context, _ string, data []byte) error {
			var reg apiv1.RegisterNode
			if json.Unmarshal(data, &reg) == nil && reg.Health != nil {
				select {
				case health <- *reg.Health:
				default:
				}
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	select {
	case h := <-health:
		if h.NodeID != agentID.NodeID {
			t.Fatalf("health from %q, want %q", h.NodeID, agentID.NodeID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent never reported health")
	}
	// Give the control plane's result subscription a beat to arm.
	time.Sleep(50 * time.Millisecond)

	// Observe the attested result on the bus independently of the control plane.
	results := make(chan apiv1.ResultEnvelope, 1)
	if _, err := shared.Subscribe(ctx, bus.Subject("sparknet", bus.KindResult, ""),
		func(_ context.Context, _ string, data []byte) error {
			var env apiv1.ResultEnvelope
			if json.Unmarshal(data, &env) == nil {
				select {
				case results <- env:
				default:
				}
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}

	task := signedControlTask(t, controlID, "arith", `{"expression":"6*7"}`)
	if err := control.DispatchTask(ctx, agentID.NodeID, task); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// The in-memory bus delivers synchronously, so the result is already in.
	select {
	case env := <-results:
		if env.TaskID != task.TaskID {
			t.Fatalf("result task id = %q, want %q", env.TaskID, task.TaskID)
		}
		if env.Result != "42" {
			t.Fatalf("result = %q, want 42", env.Result)
		}
		if err := authz.VerifyResult(agentID.Public, &env); err != nil {
			t.Fatalf("result signature did not verify: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no result published")
	}

	// And the control plane counted it as attested.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if control.counters.Snapshot().ResultsAttested >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control plane never attested the result: %+v", control.counters.Snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func signedControlTask(t *testing.T, id *identity.Identity, op, params string) apiv1.TaskEnvelope {
	t.Helper()
	now := time.Now()
	env := apiv1.TaskEnvelope{
		Fingerprint:        id.Fingerprint(),
		Capabilities:       []string{string(authz.CapClusterServe)},
		TaskID:             "task-integration",
		Operation:          op,
		ParamsJSON:         params,
		ContentInSignature: true,
		Nonce:              "nonce-integration",
		IssuedAtUnix:       now.Add(-time.Second).Unix(),
		ExpiresAtUnix:      now.Add(time.Minute).Unix(),
	}
	msg, err := apiv1.CanonicalBytes(&env)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := id.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	env.SignatureB64 = identity.EncodeSignature(sig)
	return env
}
