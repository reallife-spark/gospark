package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/bus"
	"github.com/reallife-spark/goSpark/internal/config"
	"github.com/reallife-spark/goSpark/internal/identity"
)

// TestRegistryGovernsDispatchAndRevocation proves the registry, not the node, is
// the authority: a task is refused once the Master revokes the node, and the
// agent itself refuses to execute even if a task bypasses the control plane.
func TestRegistryGovernsDispatchAndRevocation(t *testing.T) {
	shared := bus.NewMemory()
	defer shared.Close()

	controlID, _ := identity.Generate("control", identity.RoleMaster)
	agentID, _ := identity.Generate("node-a", identity.RolePeer)

	// The control plane is the Master here, so it signs the entry itself; the
	// test only supplies the identity and the capability to grant.
	entry := apiv1.NodeAuthorization{
		Identity:     agentID.Projection(),
		Capabilities: []string{string(authz.CapClusterServe)},
	}
	doc := &apiv1.NodeRegistry{
		Authorizations: []apiv1.NodeAuthorization{entry},
	}

	controlCfg := config.Default()
	controlCfg.Gateway.Listen = "127.0.0.1:0"
	controlCfg.Transport.Listen = "127.0.0.1:0"
	agentCfg := config.Default()
	agentCfg.Gateway.Listen = "127.0.0.1:0"
	agentCfg.Transport.Listen = "127.0.0.1:0"
	agentCfg.Node.Capabilities = []string{string(authz.CapClusterServe)}

	control, err := New(Options{Config: controlCfg, Identity: controlID, Role: RoleControl, Registry: doc, Bus: shared})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{Config: agentCfg, Identity: agentID, Role: RoleAgent, MasterPublic: controlID.Public, Bus: shared})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = control.Run(ctx) }()
	go func() { _ = agent.Run(ctx) }()

	// Wait for the agent to adopt the registry and echo the matching digest.
	waitDigestMatch(t, control, agent)

	// A task before revocation is executed and attested.
	results := watchResults(ctx, t, shared)
	if err := control.DispatchTask(ctx, agentID.NodeID, signedTask(t, controlID, "t-ok")); err != nil {
		t.Fatalf("dispatch before revoke: %v", err)
	}
	expectResult(t, results, "42")

	// Revoke node-a: re-sign the registry with revoked=true and a newer issue time.
	revoked := entry
	revoked.Revoked = true
	if _, err := control.ReSignRegistry(ctx, []apiv1.NodeAuthorization{revoked}); err != nil {
		t.Fatalf("re-sign: %v", err)
	}
	waitAgentRevoked(t, agent)

	// The control plane now refuses to dispatch to the revoked node.
	err = control.DispatchTask(ctx, agentID.NodeID, signedTask(t, controlID, "t-after"))
	if !errors.Is(err, authz.ErrRegistryRevoked) {
		t.Fatalf("dispatch after revoke err = %v, want ErrRegistryRevoked", err)
	}

	// Defense in depth: a task published straight onto the bus (bypassing the
	// control plane's check) is refused by the agent, so no result appears. The
	// in-memory bus surfaces the handler's refusal error, which is expected.
	_ = publishTask(ctx, shared, agentID.NodeID, signedTask(t, controlID, "t-bypass"))
	select {
	case env := <-results:
		t.Fatalf("revoked agent still produced a result: %+v", env)
	case <-time.After(300 * time.Millisecond):
	}
}

func waitDigestMatch(t *testing.T, control, agent *App) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if d := control.RegistryDigest(); d != "" && d == agent.RegistryDigest() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never adopted the registry (control=%s agent=%s)", short(control.RegistryDigest()), short(agent.RegistryDigest()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitAgentRevoked(t *testing.T, agent *App) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if !agent.registryAllowsSelf() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("agent never observed its own revocation")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func watchResults(ctx context.Context, t *testing.T, b bus.Bus) chan apiv1.ResultEnvelope {
	t.Helper()
	ch := make(chan apiv1.ResultEnvelope, 4)
	if _, err := b.Subscribe(ctx, bus.Subject("sparknet", bus.KindResult, ""),
		func(_ context.Context, _ string, data []byte) error {
			var env apiv1.ResultEnvelope
			if json.Unmarshal(data, &env) == nil {
				select {
				case ch <- env:
				default:
				}
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	return ch
}

func expectResult(t *testing.T, results chan apiv1.ResultEnvelope, want string) {
	t.Helper()
	select {
	case env := <-results:
		if env.Result != want {
			t.Fatalf("result = %q, want %q", env.Result, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no result")
	}
}

func publishTask(ctx context.Context, b bus.Bus, nodeID string, env apiv1.TaskEnvelope) error {
	env.ContentInSignature = true
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return b.Publish(ctx, bus.Subject("sparknet", bus.KindTask, nodeID), data)
}

func signedTask(t *testing.T, id *identity.Identity, taskID string, target ...string) apiv1.TaskEnvelope {
	t.Helper()
	targetNode := "node-a"
	if len(target) > 0 && target[0] != "" {
		targetNode = target[0]
	}
	now := time.Now()
	env := apiv1.TaskEnvelope{
		Fingerprint:        id.Fingerprint(),
		Capabilities:       []string{string(authz.CapClusterServe)},
		TaskID:             taskID,
		TargetNodeID:       targetNode,
		Operation:          "arith",
		ParamsJSON:         `{"expression":"6*7"}`,
		ContentInSignature: true,
		Nonce:              "nonce-" + taskID,
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

func TestAgentRegistryAdoptionRejectsPerNodeRollbackAndEmptyWipe(t *testing.T) {
	control, err := identity.Generate("control", identity.RoleMaster)
	if err != nil {
		t.Fatal(err)
	}
	node, err := identity.Generate("node-a", identity.RolePeer)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	newer, err := authz.NewAuthorization(control, node.Projection(), []string{string(authz.CapClusterServe)}, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	older, err := authz.NewAuthorization(control, node.Projection(), []string{string(authz.CapClusterServe)}, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{Config: config.Default(), Identity: node, Role: RoleAgent, MasterPublic: control.Public, Bus: bus.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	fresh := &apiv1.NodeRegistry{SchemaVersion: apiv1.RegistrySchemaVersion, MasterPublicKeyB64: control.PublicKeyB64(), Authorizations: []apiv1.NodeAuthorization{newer}}
	if err := agent.adoptRegistry(fresh); err != nil {
		t.Fatalf("fresh registry: %v", err)
	}
	old := &apiv1.NodeRegistry{SchemaVersion: apiv1.RegistrySchemaVersion, MasterPublicKeyB64: control.PublicKeyB64(), Authorizations: []apiv1.NodeAuthorization{older}}
	if err := agent.adoptRegistry(old); !errors.Is(err, authz.ErrRegistryRollback) {
		t.Fatalf("older per-node entry err = %v, want ErrRegistryRollback", err)
	}
	empty := &apiv1.NodeRegistry{SchemaVersion: apiv1.RegistrySchemaVersion, MasterPublicKeyB64: control.PublicKeyB64(), Authorizations: []apiv1.NodeAuthorization{}}
	if err := agent.adoptRegistry(empty); !errors.Is(err, authz.ErrRegistryRollback) {
		t.Fatalf("empty registry err = %v, want ErrRegistryRollback", err)
	}
}
