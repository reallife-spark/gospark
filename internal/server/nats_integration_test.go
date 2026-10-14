package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/bus"
	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/identity"
	"github.com/reallife-spark/goSpark/internal/testutil"
)

// TestControlAgentOverNATS is the cross-process equivalent of the in-memory
// end-to-end test: control and agent each hold their own NATS connection, so a
// task crosses a real network boundary (the loopback server) rather than an
// in-process function call.
func TestControlAgentOverNATS(t *testing.T) {
	url := testutil.NATSServer(t)

	controlID, err := identity.Generate("control", identity.RoleMaster)
	if err != nil {
		t.Fatal(err)
	}
	agentID, err := identity.Generate("node-a", identity.RolePeer)
	if err != nil {
		t.Fatal(err)
	}

	// A per-test stream and subject root keep parallel tests from colliding.
	stream, root := uniqueNames(t)

	controlCfg := config.Default()
	controlCfg.Gateway.Listen = "127.0.0.1:0"
	controlCfg.Transport.Listen = "127.0.0.1:0"
	controlCfg.Bus.URL = url
	controlCfg.Bus.Stream = stream
	controlCfg.Bus.SubjectRoot = root

	agentCfg := config.Default()
	agentCfg.Gateway.Listen = "127.0.0.1:0"
	agentCfg.Transport.Listen = "127.0.0.1:0"
	agentCfg.Bus.URL = url
	agentCfg.Bus.Stream = stream
	agentCfg.Bus.SubjectRoot = root
	agentCfg.Node.Capabilities = []string{string(authz.CapClusterServe)}

	executors := map[string]ed25519.PublicKey{agentID.NodeID: agentID.Public}

	control, err := New(Options{
		Config:         controlCfg,
		Identity:       controlID,
		Role:           RoleControl,
		ExecutorPublic: executors,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{
		Config:       agentCfg,
		Identity:     agentID,
		Role:         RoleAgent,
		MasterPublic: controlID.Public,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = control.Run(ctx) }()
	go func() { _ = agent.Run(ctx) }()

	// Probe connection to observe health and results independently.
	probe, err := bus.Connect(bus.NATSOptions{URL: url, Name: "probe", Stream: stream, Subjects: []string{root + ".>"}})
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()

	health := make(chan apiv1.NodeHealth, 1)
	if _, err := probe.Subscribe(ctx, bus.Subject(root, bus.KindRegister, ""),
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
	case <-time.After(10 * time.Second):
		t.Fatal("agent never reported health over NATS")
	}

	results := make(chan apiv1.ResultEnvelope, 1)
	if _, err := probe.Subscribe(ctx, bus.Subject(root, bus.KindResult, ""),
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

	select {
	case env := <-results:
		if env.Result != "42" {
			t.Fatalf("result = %q, want 42", env.Result)
		}
		if err := authz.VerifyResult(agentID.Public, &env); err != nil {
			t.Fatalf("result signature did not verify: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no result over NATS")
	}

	deadline := time.Now().Add(5 * time.Second)
	for control.counters.Snapshot().ResultsAttested < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("control never attested over NATS: %+v", control.counters.Snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func uniqueNames(t *testing.T) (stream, root string) {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(b[:])
	return "SPT_" + suffix, "spt" + suffix
}
