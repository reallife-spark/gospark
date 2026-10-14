package router_test

import (
	"strings"
	"testing"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/router"
)

func TestDecideEdgeForShortPrompt(t *testing.T) {
	r := router.New(router.Config{})
	d := r.Decide("what colour is the sky?", []string{"cluster-dispatch"})
	if d.Target != apiv1.TargetEdge {
		t.Fatalf("target = %s, want edge (%s)", d.Target, d.Reason)
	}
	if d.AsyncRequired {
		t.Fatal("edge decisions must not require async")
	}
}

func TestDecideDGXForKeyword(t *testing.T) {
	r := router.New(router.Config{})
	d := r.Decide("please synthesize a cross-domain architecture", nil)
	if d.Target != apiv1.TargetDGX {
		t.Fatalf("target = %s, want dgx", d.Target)
	}
	if !d.AsyncRequired {
		t.Fatal("remote escalation must require async")
	}
}

func TestDecideClusterDegradesWithoutDispatch(t *testing.T) {
	r := router.New(router.Config{EdgeTokenCeiling: 10, ClusterTokenFloor: 20})
	long := strings.Repeat("token ", 40) // ~> cluster floor
	withCap := r.Decide(long, []string{"cluster-dispatch"})
	if withCap.Target != apiv1.TargetCluster {
		t.Fatalf("with dispatch: target = %s, want cluster", withCap.Target)
	}
	without := r.Decide(long, nil)
	if without.Target != apiv1.TargetDGX {
		t.Fatalf("without dispatch: target = %s, want dgx (degraded)", without.Target)
	}
}

func TestEstimateTokensMonotonic(t *testing.T) {
	short := router.EstimateTokens("hello world")
	long := router.EstimateTokens(strings.Repeat("hello world ", 100))
	if long <= short {
		t.Fatalf("estimate should grow: short=%d long=%d", short, long)
	}
}

func TestPlanSubTasksDeterministic(t *testing.T) {
	r := router.New(router.Config{MaxNodes: 2})
	prompt := "first part\n\nsecond part\n\nthird part"
	nodes := []string{"node-a", "node-b"}
	a := r.PlanSubTasks(prompt, nodes)
	b := r.PlanSubTasks(prompt, nodes)
	if len(a) != len(b) {
		t.Fatalf("nondeterministic count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].NodeID != b[i].NodeID || a[i].SubtaskID != b[i].SubtaskID || a[i].Instruction != b[i].Instruction {
			t.Fatalf("subtask %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
	if len(a) == 0 {
		t.Fatal("expected at least one subtask")
	}
}

func TestPlanSubTasksNoNodes(t *testing.T) {
	r := router.New(router.Config{})
	if got := r.PlanSubTasks("anything", nil); got != nil {
		t.Fatalf("expected nil for no nodes, got %v", got)
	}
}

func TestModeForShardOnVeryLong(t *testing.T) {
	r := router.New(router.Config{EdgeTokenCeiling: 10, ClusterTokenFloor: 100})
	long := strings.Repeat("word ", 2000)
	if m := r.ModeFor(long); m != router.ModeShard {
		t.Fatalf("mode = %s, want shard", m)
	}
}
