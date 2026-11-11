// Package delegates implements the local delegate broker: a validated task
// graph, a durable run/event store, bounded execution, and handoff records.
//
// The broker runs work; it does not authorize it. A session grant is verified
// by the Python gateway before a plan reaches this package, and a delegate
// never writes a vault page or signs a note.
package delegates

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
)

// Harness names a delegate runtime.
type Harness string

const (
	HarnessOpenCode   Harness = "opencode"
	HarnessLocalModel Harness = "local-model"
	HarnessCluster    Harness = "cluster"
	HarnessCommand    Harness = "command"
)

// ValidHarness reports whether h is supported by this broker.
func ValidHarness(h Harness) bool {
	switch h {
	case HarnessOpenCode, HarnessLocalModel, HarnessCluster, HarnessCommand:
		return true
	}
	return false
}

// Status is the run lifecycle.
type Status string

const (
	Pending        Status = "pending"
	Running        Status = "running"
	Waiting        Status = "waiting"
	Succeeded      Status = "succeeded"
	Failed         Status = "failed"
	Blocked        Status = "blocked"
	Canceled       Status = "canceled"
	BudgetExceeded Status = "budget_exceeded"
)

// Terminal reports whether a run can no longer change.
func (s Status) Terminal() bool {
	switch s {
	case Succeeded, Failed, Blocked, Canceled, BudgetExceeded:
		return true
	}
	return false
}

// Permissions are the local execution boundary.
type Permissions struct {
	WorkspaceMode  string   `json:"workspace_mode"` // read_only | direct
	Network        string   `json:"network"`        // none | restricted | full
	AllowedTools   []string `json:"allowed_tools,omitempty"`
	DeniedTools    []string `json:"denied_tools,omitempty"`
	AllowedEnv     []string `json:"allowed_env,omitempty"`
	AllowSubagents bool     `json:"allow_subagents,omitempty"`
}

// Budget bounds one task.
type Budget struct {
	MaxWallTimeSeconds int     `json:"max_wall_time_seconds"`
	MaxSteps           int     `json:"max_steps"`
	MaxCostUSD         float64 `json:"max_cost_usd,omitempty"`
	MaxInputTokens     int     `json:"max_input_tokens,omitempty"`
	MaxOutputTokens    int     `json:"max_output_tokens,omitempty"`
}

// Task is one delegate assignment.
type Task struct {
	ID            string         `json:"task_id"`
	Description   string         `json:"description"`
	Harness       Harness        `json:"harness"`
	AssignedAgent string         `json:"assigned_agent,omitempty"`
	DependsOn     []string       `json:"dependencies,omitempty"`
	Workspace     string         `json:"workspace,omitempty"`
	Permissions   Permissions    `json:"permissions,omitempty"`
	Budget        Budget         `json:"budget,omitempty"`
	Verification  string         `json:"verification,omitempty"`
	OutputSchema  map[string]any `json:"output_schema,omitempty"`
}

// Plan is a validated delegate graph.
type Plan struct {
	PlanID     string `json:"plan_id,omitempty"`
	ProjectID  string `json:"project_id,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
	Tasks      []Task `json:"tasks"`
}

// Result is what one delegate returns to the shared blackboard.
type Result struct {
	Summary      string         `json:"summary,omitempty"`
	Output       string         `json:"output,omitempty"`
	FilesChanged []string       `json:"files_changed,omitempty"`
	Commands     []string       `json:"commands,omitempty"`
	Verification string         `json:"verification,omitempty"`
	Verified     bool           `json:"verified"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	SkillEval    map[string]any `json:"skill_eval,omitempty"`
	Error        string         `json:"error,omitempty"`
	ErrorCode    string         `json:"error_code,omitempty"`
}

// Event is one normalized run event.
type Event struct {
	Sequence int            `json:"sequence"`
	Type     string         `json:"type"`
	At       string         `json:"at"`
	Data     map[string]any `json:"data,omitempty"`
}

// Run is the durable record of one task.
type Run struct {
	Schema      string  `json:"schema"`
	RunID       string  `json:"run_id"`
	PlanID      string  `json:"plan_id"`
	TaskID      string  `json:"task_id"`
	ProjectID   string  `json:"project_id,omitempty"`
	UserID      string  `json:"user_id,omitempty"`
	WorkflowID  string  `json:"workflow_id,omitempty"`
	ParentRunID string  `json:"parent_run_id,omitempty"`
	Harness     Harness `json:"harness"`
	Task        Task    `json:"task"`
	Status      Status  `json:"status"`
	Workspace   string  `json:"workspace,omitempty"`
	Result      Result  `json:"result,omitempty"`
	Error       string  `json:"error,omitempty"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	StartedAt   string  `json:"started_at,omitempty"`
	FinishedAt  string  `json:"finished_at,omitempty"`
	Events      []Event `json:"events,omitempty"`
}

// Validate normalizes and checks a plan. It returns a stable execution order.
func (p *Plan) Validate() ([]Task, error) {
	if len(p.Tasks) == 0 {
		return nil, fmt.Errorf("delegates: a plan needs at least one task")
	}
	if p.PlanID == "" {
		p.PlanID = newID()
	}
	byID := make(map[string]Task, len(p.Tasks))
	for i := range p.Tasks {
		task := &p.Tasks[i]
		if strings.TrimSpace(task.ID) == "" {
			return nil, fmt.Errorf("delegates: task %d has no task_id", i)
		}
		if strings.TrimSpace(task.Description) == "" {
			return nil, fmt.Errorf("delegates: task %s has no description", task.ID)
		}
		if task.Harness == "" {
			task.Harness = HarnessOpenCode
		}
		if !ValidHarness(task.Harness) {
			return nil, fmt.Errorf("delegates: task %s has unknown harness %q", task.ID, task.Harness)
		}
		if _, exists := byID[task.ID]; exists {
			return nil, fmt.Errorf("delegates: duplicate task_id %q", task.ID)
		}
		if task.Permissions.WorkspaceMode == "" {
			task.Permissions.WorkspaceMode = "read_only"
		}
		if task.Permissions.WorkspaceMode != "read_only" && task.Permissions.WorkspaceMode != "direct" {
			return nil, fmt.Errorf("delegates: task %s has invalid workspace mode", task.ID)
		}
		if task.Permissions.Network == "" {
			task.Permissions.Network = "none"
		}
		switch task.Permissions.Network {
		case "none", "restricted", "full":
		default:
			return nil, fmt.Errorf("delegates: task %s has invalid network mode", task.ID)
		}
		if task.Workspace != "" && !safePath(task.Workspace) {
			return nil, fmt.Errorf("delegates: task %s has an unsafe workspace", task.ID)
		}
		if task.Budget.MaxWallTimeSeconds <= 0 {
			task.Budget.MaxWallTimeSeconds = 900
		}
		if task.Budget.MaxWallTimeSeconds > 86_400 {
			task.Budget.MaxWallTimeSeconds = 86_400
		}
		if task.Budget.MaxSteps <= 0 {
			task.Budget.MaxSteps = 80
		}
		byID[task.ID] = *task
	}
	for _, task := range p.Tasks {
		for _, dep := range task.DependsOn {
			if dep == task.ID {
				return nil, fmt.Errorf("delegates: task %s depends on itself", task.ID)
			}
			if _, ok := byID[dep]; !ok {
				return nil, fmt.Errorf("delegates: task %s depends on unknown task %q", task.ID, dep)
			}
		}
	}
	order, err := topological(p.Tasks)
	if err != nil {
		return nil, err
	}
	return order, nil
}

func topological(tasks []Task) ([]Task, error) {
	byID := make(map[string]Task, len(tasks))
	state := make(map[string]int, len(tasks)) // 0 unvisited, 1 visiting, 2 done
	for _, task := range tasks {
		byID[task.ID] = task
	}
	var order []Task
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("delegates: dependency cycle at %q", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, dep := range byID[id].DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		order = append(order, byID[id])
		return nil
	}
	for _, task := range tasks {
		if err := visit(task.ID); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func safePath(value string) bool {
	if strings.ContainsRune(value, 0) || strings.HasPrefix(value, "/") {
		return false
	}
	cleaned := path.Clean(strings.ReplaceAll(value, "\\", "/"))
	return cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func newID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}
