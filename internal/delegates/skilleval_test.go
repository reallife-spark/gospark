package delegates

import (
	"context"
	"strings"
	"testing"
)

func validSkillEval() map[string]any {
	return map[string]any{
		"schema":      "sparknet-skill-eval/1",
		"eval_id":     "review-1",
		"skill":       map[string]any{"name": "code-review", "origin": "mattpocock"},
		"subject":     map[string]any{"kind": "delegate-run", "ref": "run-1"},
		"verdict":     "pass",
		"findings":    []any{},
		"evaluator":   map[string]any{"kind": "agent", "id": "project-reviewer"},
		"created_at":  "2026-11-11T00:00:00Z",
		"body_sha256": strings.Repeat("a", 64),
	}
}

func TestValidateSkillEval(t *testing.T) {
	tests := map[string]func(map[string]any){
		"unknown verdict": func(record map[string]any) { record["verdict"] = "maybe" },
		"unknown subject": func(record map[string]any) { record["subject"] = map[string]any{"kind": "vibes", "ref": "x"} },
		"wrong schema":    func(record map[string]any) { record["schema"] = "sparknet-skill-eval/0" },
		"bad digest":      func(record map[string]any) { record["body_sha256"] = "nope" },
		"missing ref":     func(record map[string]any) { record["subject"] = map[string]any{"kind": "plan"} },
		"authority field": func(record map[string]any) {
			record["findings"] = []any{map[string]any{"severity": "warn", "rule": "x", "message": "y", "grant": "all"}}
		},
	}
	if err := ValidateSkillEval(validSkillEval()); err != nil {
		t.Fatalf("valid record refused: %v", err)
	}
	if err := ValidateSkillEval(nil); err != nil {
		t.Fatalf("absent record refused: %v", err)
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			record := validSkillEval()
			mutate(record)
			if err := ValidateSkillEval(record); err == nil {
				t.Fatalf("expected a refusal for %s", name)
			}
		})
	}
}

type reviewingExecutor struct {
	result Result
}

func (r *reviewingExecutor) Name() Harness { return HarnessCommand }

func (r *reviewingExecutor) Run(context.Context, *Run, func(Event)) (Result, error) {
	return r.result, nil
}

func runOne(t *testing.T, result Result) *Run {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	broker := NewBroker(store, []Executor{&reviewingExecutor{result: result}})
	runs, err := broker.Start(context.Background(), Plan{Tasks: []Task{
		{ID: "review", Description: "review the result", Harness: HarnessCommand},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Wait(context.Background(), runs[0].PlanID); err != nil {
		t.Fatal(err)
	}
	run, err := store.Get(runs[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestBrokerStoresAValidSkillEval(t *testing.T) {
	run := runOne(t, Result{Summary: "done", Verified: true, SkillEval: validSkillEval()})
	if run.Status != Succeeded {
		t.Fatalf("status = %s (%s)", run.Status, run.Error)
	}
	if run.Result.SkillEval == nil || run.Result.SkillEval["verdict"] != "pass" {
		t.Fatalf("skill eval not persisted: %+v", run.Result.SkillEval)
	}
}

func TestBrokerFailsAnInvalidSkillEval(t *testing.T) {
	record := validSkillEval()
	record["verdict"] = "maybe"
	run := runOne(t, Result{Summary: "done", Verified: true, SkillEval: record})
	if run.Status != Failed {
		t.Fatalf("status = %s, want failed", run.Status)
	}
	if !strings.Contains(run.Error, "skill eval") {
		t.Fatalf("error = %q", run.Error)
	}
	if run.Result.ErrorCode != "skill_eval_invalid" {
		t.Fatalf("error code = %q", run.Result.ErrorCode)
	}
}
