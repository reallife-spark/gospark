package delegates

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Executor runs one task. Implementations must honor ctx cancellation between
// their own steps; the broker cannot kill an arbitrary child safely.
type Executor interface {
	Name() Harness
	Run(ctx context.Context, run *Run, emit func(Event)) (Result, error)
}

// Broker owns the in-flight plans and their durable runs.
type Broker struct {
	store     *Store
	executors map[Harness]Executor
	now       func() time.Time

	mu      sync.Mutex
	rootCtx context.Context
	cancel  context.CancelFunc
	plans   map[string]context.CancelFunc
	done    map[string]chan struct{}
	stopped bool
}

// NewBroker builds a broker. An executor is required for every harness a plan
// names; an unknown harness fails the plan before any run starts.
func NewBroker(store *Store, executors []Executor) *Broker {
	if store == nil {
		panic("delegates: nil store")
	}
	rootCtx, cancel := context.WithCancel(context.Background())
	b := &Broker{
		store:     store,
		executors: map[Harness]Executor{},
		now:       time.Now,
		rootCtx:   rootCtx,
		cancel:    cancel,
		plans:     map[string]context.CancelFunc{},
		done:      map[string]chan struct{}{},
	}
	for _, executor := range executors {
		if executor != nil {
			b.executors[executor.Name()] = executor
		}
	}
	return b
}

// Start validates a plan, creates its runs and launches execution.
func (b *Broker) Start(ctx context.Context, plan Plan) ([]Run, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	ordered, err := plan.Validate()
	if err != nil {
		return nil, err
	}
	for _, task := range ordered {
		if _, ok := b.executors[task.Harness]; !ok {
			return nil, fmt.Errorf("delegates: no executor for harness %q", task.Harness)
		}
	}
	runCtx, cancel := context.WithCancel(b.rootCtx)
	runs := make([]*Run, 0, len(ordered))
	byTask := make(map[string]*Run, len(ordered))
	stamp := b.now().UTC().Format(time.RFC3339Nano)
	for _, task := range ordered {
		run := &Run{
			Schema:     "sparknet-delegate-run/1",
			RunID:      newID(),
			PlanID:     plan.PlanID,
			TaskID:     task.ID,
			ProjectID:  plan.ProjectID,
			UserID:     plan.UserID,
			WorkflowID: plan.WorkflowID,
			Harness:    task.Harness,
			Task:       task,
			Status:     Pending,
			Workspace:  task.Workspace,
			CreatedAt:  stamp,
			UpdatedAt:  stamp,
		}
		if err := b.store.Save(run); err != nil {
			cancel()
			return nil, err
		}
		runs = append(runs, run)
		byTask[task.ID] = run
	}
	done := make(chan struct{})
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		cancel()
		return nil, errors.New("delegates: broker is stopped")
	}
	b.plans[plan.PlanID] = cancel
	b.done[plan.PlanID] = done
	b.mu.Unlock()

	// Return value snapshots. The broker goroutine owns the live run objects;
	// handing a caller the same pointers would make every HTTP response a data
	// race against the first status/event update.
	snapshots := make([]Run, 0, len(runs))
	for _, run := range runs {
		snapshots = append(snapshots, *run)
	}
	go b.execute(runCtx, plan.PlanID, ordered, byTask, done)
	return snapshots, nil
}

// Wait blocks until every run in a plan settles or ctx is done.
func (b *Broker) Wait(ctx context.Context, planID string) error {
	b.mu.Lock()
	done := b.done[planID]
	b.mu.Unlock()
	if done == nil {
		return fmt.Errorf("delegates: unknown plan %q", planID)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cancel stops a plan. Runs that have not started are marked canceled; the
// executor is expected to observe its context.
func (b *Broker) Cancel(planID string) bool {
	b.mu.Lock()
	cancel := b.plans[planID]
	b.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// Close cancels every in-flight plan and waits for their goroutines to settle.
// Waiting matters to callers that remove the run store immediately afterwards:
// a cancelled executor may still be persisting its final state.
func (b *Broker) Close() {
	b.mu.Lock()
	b.stopped = true
	cancels := make([]context.CancelFunc, 0, len(b.plans))
	for _, cancel := range b.plans {
		cancels = append(cancels, cancel)
	}
	done := make([]chan struct{}, 0, len(b.done))
	for _, ch := range b.done {
		done = append(done, ch)
	}
	b.cancel()
	b.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, ch := range done {
		<-ch
	}
}

func (b *Broker) execute(ctx context.Context, planID string, ordered []Task, byTask map[string]*Run, done chan struct{}) {
	defer close(done)
	defer func() {
		b.mu.Lock()
		delete(b.plans, planID)
		delete(b.done, planID)
		b.mu.Unlock()
	}()
	settled := map[string]Status{}
	for _, task := range ordered {
		run := byTask[task.ID]
		if ctx.Err() != nil {
			run.Status = Canceled
			run.Error = "plan canceled"
			b.finish(run)
			settled[task.ID] = Canceled
			continue
		}
		blocked := false
		for _, dep := range task.DependsOn {
			switch settled[dep] {
			case Succeeded:
			case Canceled:
				run.Status = Canceled
				run.Error = "dependency canceled: " + dep
				blocked = true
			default:
				run.Status = Blocked
				run.Error = "dependency did not succeed: " + dep
				blocked = true
			}
			if blocked {
				break
			}
		}
		if blocked {
			b.finish(run)
			settled[task.ID] = run.Status
			continue
		}
		b.runTask(ctx, run)
		settled[task.ID] = run.Status
	}
}

func (b *Broker) runTask(ctx context.Context, run *Run) {
	executor := b.executors[run.Harness]
	if executor == nil {
		run.Status = Failed
		run.Error = "no executor for harness " + string(run.Harness)
		b.finish(run)
		return
	}
	taskCtx, cancel := context.WithTimeout(ctx, time.Duration(run.Task.Budget.MaxWallTimeSeconds)*time.Second)
	defer cancel()
	run.Status = Running
	run.StartedAt = b.now().UTC().Format(time.RFC3339Nano)
	b.emit(run, "status", map[string]any{"status": string(run.Status)})
	steps := 0
	budgetHit := false
	emit := func(event Event) {
		if event.Type == "command" || event.Type == "tool_call" || event.Type == "file_change" {
			steps++
			if steps > run.Task.Budget.MaxSteps {
				budgetHit = true
				cancel()
			}
		}
		b.emit(run, event.Type, event.Data)
	}
	result, err := executor.Run(taskCtx, run, emit)
	run.Result = result
	if evalErr := ValidateSkillEval(result.SkillEval); evalErr != nil {
		run.Result.Error = evalErr.Error()
		run.Result.ErrorCode = "skill_eval_invalid"
		err = evalErr
	}
	switch {
	case budgetHit:
		run.Status = BudgetExceeded
		run.Error = "task budget exceeded"
	case errors.Is(taskCtx.Err(), context.DeadlineExceeded):
		run.Status = BudgetExceeded
		run.Error = "wall-time budget exceeded"
	case errors.Is(ctx.Err(), context.Canceled):
		run.Status = Canceled
		run.Error = "plan canceled"
	case err != nil:
		run.Status = Failed
		run.Error = err.Error()
	case run.Task.Verification != "" && !result.Verified:
		run.Status = Failed
		run.Error = "verification did not pass"
	default:
		run.Status = Succeeded
	}
	b.finish(run)
}

func (b *Broker) emit(run *Run, kind string, data map[string]any) {
	sequence := len(run.Events) + 1
	event := Event{
		Sequence: sequence,
		Type:     kind,
		At:       b.now().UTC().Format(time.RFC3339Nano),
		Data:     data,
	}
	run.Events = append(run.Events, event)
	_ = b.store.Save(run)
}

func (b *Broker) finish(run *Run) {
	run.FinishedAt = b.now().UTC().Format(time.RFC3339Nano)
	run.UpdatedAt = run.FinishedAt
	b.emit(run, "status", map[string]any{"status": string(run.Status), "error": run.Error})
}

// CommandExecutor runs a configured command with a safe environment. It is the
// seam for an installed harness CLI (for example `opencode run`); it does not
// parse vendor JSON and does not grant the child any authority.
type CommandExecutor struct {
	HarnessName Harness
	Command     []string
	Env         []string
}

// Name identifies the harness.
func (e *CommandExecutor) Name() Harness {
	if e.HarnessName == "" {
		return HarnessCommand
	}
	return e.HarnessName
}

// Run expands placeholders and executes one task.
func (e *CommandExecutor) Run(ctx context.Context, run *Run, emit func(Event)) (Result, error) {
	if len(e.Command) == 0 {
		return Result{}, errors.New("delegates: command executor has no command")
	}
	args := make([]string, 0, len(e.Command)-1)
	for _, arg := range e.Command[1:] {
		args = append(args, expand(arg, run))
	}
	cmd := exec.CommandContext(ctx, e.Command[0], args...)
	cmd.Dir = run.Task.Workspace
	env := safeEnvironment(e.Env, run.Task.Permissions.AllowedEnv)
	cmd.Env = env
	emit(Event{Type: "command", Data: map[string]any{"command": strings.Join(append([]string{e.Command[0]}, args...), " ")}})
	var output boundedBuffer
	output.max = 1 << 20
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	text := output.String()
	result := Result{
		Output:  text,
		Summary: firstLines(text, 20),
	}
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() != 0 {
		result.Error = errString(err)
		result.ErrorCode = "exit_nonzero"
		emit(Event{Type: "error", Data: map[string]any{"error": result.Error}})
		return result, fmt.Errorf("delegates: command failed: %s", result.Error)
	}
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	emit(Event{Type: "message", Data: map[string]any{"text": text}})
	return result, nil
}

func expand(value string, run *Run) string {
	replacements := map[string]string{
		"{task}":      run.Task.Description,
		"{workspace}": run.Task.Workspace,
		"{agent}":     run.Task.AssignedAgent,
		"{run_id}":    run.RunID,
	}
	for key, replacement := range replacements {
		value = strings.ReplaceAll(value, key, replacement)
	}
	return value
}

func safeEnvironment(extra, allowed []string) []string {
	allow := map[string]struct{}{}
	for _, name := range allowed {
		allow[name] = struct{}{}
	}
	for _, name := range extra {
		allow[name] = struct{}{}
	}
	// The child needs a usable PATH and a temporary directory; every other
	// inherited variable is withheld unless the task explicitly names it.
	base := map[string]struct{}{"PATH": {}, "HOME": {}, "TMPDIR": {}, "TEMP": {}, "TMP": {}, "LANG": {}, "LC_ALL": {}}
	var out []string
	for _, entry := range os.Environ() {
		name := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			name = entry[:index]
		}
		if _, ok := base[name]; ok {
			out = append(out, entry)
			continue
		}
		if _, ok := allow[name]; ok && !secretEnvName(name) {
			out = append(out, entry)
		}
	}
	return out
}

func firstLines(text string, limit int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > limit {
		lines = lines[:limit]
	}
	return strings.Join(lines, "\n")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func secretEnvName(name string) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "GOSPARK_") {
		return true
	}
	for _, suffix := range []string{"_TOKEN", "_SECRET", "_KEY", "_PASSWORD", "_CREDENTIAL", "_PRIVATE"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return upper == "TOKEN" || upper == "SECRET" || upper == "PASSWORD"
}

type boundedBuffer struct {
	buf []byte
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(b.buf) < b.max {
		remaining := b.max - len(b.buf)
		if len(p) > remaining {
			b.buf = append(b.buf, p[:remaining]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return string(b.buf) }
