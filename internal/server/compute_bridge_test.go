package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/bus"
	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/identity"
)

// computeWorker is a stand-in for the Python/CUDA bridge: it answers /v1/health
// and echoes a fixed result for /v1/execute, recording the operations it saw.
func computeWorker(t *testing.T, result string) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "active"})
		case "/v1/execute":
			var req struct {
				Operation  string `json:"operation"`
				ParamsJSON string `json:"params_json"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			seen = append(seen, req.Operation)
			_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv, &seen
}

// TestDispatchOperationRoutes proves the routing rule: local sandbox operations
// run in-process, explicitly delegated operations go to the worker, and nothing
// else is accepted.
func TestDispatchOperationRoutes(t *testing.T) {
	srv, seen := computeWorker(t, "gpu-output")
	defer srv.Close()

	cfg := config.Default()
	cfg.Compute = config.ComputeConfig{Endpoint: srv.URL, Operations: []string{"infer"}}
	cfg.Bus.URL = ""
	id, err := identity.Generate("node-a", identity.RolePeer)
	if err != nil {
		t.Fatal(err)
	}
	master, err := identity.Generate("control", identity.RoleMaster)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(Options{Config: cfg, Identity: id, Role: RoleAgent, MasterPublic: master.Public})
	if err != nil {
		t.Fatal(err)
	}
	app.bus = bus.NewMemory()
	defer app.bus.Close()

	// Local operation stays local and does not touch the worker.
	got, err := app.dispatchOperation(context.Background(), "arith", `{"expression":"1+2"}`)
	if err != nil || got != "3" {
		t.Fatalf("arith = %q, %v; want 3, nil", got, err)
	}
	if len(*seen) != 0 {
		t.Fatalf("worker called for a local operation: %v", *seen)
	}

	// Delegated operation goes to the worker.
	got, err = app.dispatchOperation(context.Background(), "infer", `{"prompt":"hi"}`)
	if err != nil || got != "gpu-output" {
		t.Fatalf("infer = %q, %v; want gpu-output, nil", got, err)
	}
	if len(*seen) != 1 || (*seen)[0] != "infer" {
		t.Fatalf("worker saw %v, want [infer]", *seen)
	}

	// A local sandbox name can never be shadowed by delegation, and an unknown
	// name is refused.
	if _, err := app.dispatchOperation(context.Background(), "bogus", "{}"); err == nil {
		t.Fatal("unknown operation = nil error, want refusal")
	}
}

// TestComputeOperationRefusedWithoutWorker proves the sandbox stays closed: an
// operation that would be delegated is unknown unless a worker is configured.
func TestComputeOperationRefusedWithoutWorker(t *testing.T) {
	cfg := config.Default()
	id, _ := identity.Generate("node-a", identity.RolePeer)
	master, _ := identity.Generate("control", identity.RoleMaster)
	app, err := New(Options{Config: cfg, Identity: id, Role: RoleAgent, MasterPublic: master.Public})
	if err != nil {
		t.Fatal(err)
	}
	app.bus = bus.NewMemory()
	defer app.bus.Close()
	if _, err := app.dispatchOperation(context.Background(), "infer", "{}"); err == nil {
		t.Fatal("infer with no worker configured = nil error, want refusal")
	}
}

// TestAgentDelegatesTaskToComputeWorker is the end-to-end bridge proof: a signed
// task with a delegated operation crosses the bus, the agent hands execution to
// the compute-plane worker, and the agent's attested result carries the worker's
// output.
func TestAgentDelegatesTaskToComputeWorker(t *testing.T) {
	srv, seen := computeWorker(t, "42-delegated")
	defer srv.Close()

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
	agentCfg.Node.Capabilities = []string{"cluster-serve"}
	agentCfg.Compute = config.ComputeConfig{Endpoint: srv.URL, Operations: []string{"infer"}}

	control, err := New(Options{
		Config:         controlCfg,
		Identity:       controlID,
		Role:           RoleControl,
		ExecutorPublic: map[string]ed25519.PublicKey{agentID.NodeID: agentID.Public},
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

	// Arm the health subscription before the agent starts so its startup report
	// cannot race ahead of us (see TestControlAgentEndToEnd).
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

	go func() { _ = control.Run(ctx) }()
	go func() { _ = agent.Run(ctx) }()

	select {
	case <-health:
	case <-time.After(3 * time.Second):
		t.Fatal("agent never reported health")
	}

	task := signedControlTask(t, controlID, "infer", `{"prompt":"hello gpu"}`)
	if err := control.DispatchTask(ctx, agentID.NodeID, task); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	select {
	case env := <-results:
		if env.Operation != "infer" {
			t.Fatalf("result operation = %q, want infer", env.Operation)
		}
		if env.Result != "42-delegated" {
			t.Fatalf("result = %q, want 42-delegated", env.Result)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no result; worker saw %v", *seen)
	}
	if len(*seen) != 1 || (*seen)[0] != "infer" {
		t.Fatalf("worker saw %v, want exactly one infer", *seen)
	}
}
