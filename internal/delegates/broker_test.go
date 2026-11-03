package delegates

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeExecutor struct {
	name  Harness
	mu    sync.Mutex
	calls []string
	fail  map[string]bool
}

func (f *fakeExecutor) Name() Harness { return f.name }

func (f *fakeExecutor) Run(ctx context.Context, run *Run, emit func(Event)) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, run.Task.ID)
	f.mu.Unlock()
	emit(Event{Type: "command", Data: map[string]any{"command": "fake " + run.Task.ID}})
	if f.fail[run.Task.ID] {
		return Result{Error: "planned failure"}, errors.New("planned failure")
	}
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-time.After(5 * time.Millisecond):
	}
	return Result{Summary: "done " + run.Task.ID, Verified: true}, nil
}

func TestPlanRejectsCyclesAndUnknownDependencies(t *testing.T) {
	cycle := Plan{Tasks: []Task{
		{ID: "a", Description: "a", DependsOn: []string{"b"}},
		{ID: "b", Description: "b", DependsOn: []string{"a"}},
	}}
	if _, err := cycle.Validate(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
	missing := Plan{Tasks: []Task{{ID: "a", Description: "a", DependsOn: []string{"nope"}}}}
	if _, err := missing.Validate(); err == nil {
		t.Fatal("expected unknown dependency refusal")
	}
}

func TestBrokerRunsDependenciesInOrder(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeExecutor{name: HarnessCommand}
	broker := NewBroker(store, []Executor{executor})
	runs, err := broker.Start(context.Background(), Plan{Tasks: []Task{
		{ID: "write", Description: "write the report", Harness: HarnessCommand},
		{ID: "review", Description: "review the report", Harness: HarnessCommand, DependsOn: []string{"write"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Wait(context.Background(), runs[0].PlanID); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 2 || executor.calls[0] != "write" || executor.calls[1] != "review" {
		t.Fatalf("execution order = %v", executor.calls)
	}
	first, _ := store.Get(runs[0].RunID)
	second, _ := store.Get(runs[1].RunID)
	if first.Status != Succeeded || second.Status != Succeeded {
		t.Fatalf("statuses = %s, %s", first.Status, second.Status)
	}
	if len(first.Events) == 0 || first.Events[0].Type != "status" {
		t.Fatalf("events = %+v", first.Events)
	}
}

func TestFailedDependencyBlocksConsumers(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	executor := &fakeExecutor{name: HarnessCommand, fail: map[string]bool{"write": true}}
	broker := NewBroker(store, []Executor{executor})
	runs, err := broker.Start(context.Background(), Plan{Tasks: []Task{
		{ID: "write", Description: "fail", Harness: HarnessCommand},
		{ID: "review", Description: "blocked", Harness: HarnessCommand, DependsOn: []string{"write"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Wait(context.Background(), runs[0].PlanID); err != nil {
		t.Fatal(err)
	}
	blocked, _ := store.Get(runs[1].RunID)
	if blocked.Status != Blocked {
		t.Fatalf("consumer status = %s, want blocked", blocked.Status)
	}
}

func TestBrokerCancelMarksPendingTasks(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	executor := &fakeExecutor{name: HarnessCommand}
	broker := NewBroker(store, []Executor{executor})
	runs, err := broker.Start(context.Background(), Plan{Tasks: []Task{
		{ID: "one", Description: "one", Harness: HarnessCommand},
		{ID: "two", Description: "two", Harness: HarnessCommand},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !broker.Cancel(runs[0].PlanID) {
		t.Fatal("cancel returned false")
	}
	_ = broker.Wait(context.Background(), runs[0].PlanID)
	one, _ := store.Get(runs[0].RunID)
	two, _ := store.Get(runs[1].RunID)
	if one.Status != Canceled && one.Status != Succeeded {
		t.Fatalf("first run = %s", one.Status)
	}
	if two.Status != Canceled {
		t.Fatalf("second run = %s, want canceled", two.Status)
	}
}

func TestWallTimeBudget(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	slow := &slowExecutor{}
	broker := NewBroker(store, []Executor{slow})
	runs, err := broker.Start(context.Background(), Plan{Tasks: []Task{{
		ID:          "slow",
		Description: "sleep",
		Harness:     HarnessCommand,
		Budget:      Budget{MaxWallTimeSeconds: 1},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Wait(context.Background(), runs[0].PlanID); err != nil {
		t.Fatal(err)
	}
	run, _ := store.Get(runs[0].RunID)
	if run.Status != BudgetExceeded {
		t.Fatalf("status = %s, want budget_exceeded", run.Status)
	}
}

type slowExecutor struct{}

func (s *slowExecutor) Name() Harness { return HarnessCommand }
func (s *slowExecutor) Run(ctx context.Context, run *Run, emit func(Event)) (Result, error) {
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-time.After(3 * time.Second):
		return Result{}, nil
	}
}
