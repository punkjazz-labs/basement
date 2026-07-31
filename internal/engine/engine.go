package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/punkjazz-labs/runonspark-manager/internal/operations"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
	"github.com/punkjazz-labs/runonspark-manager/internal/redact"
	"github.com/punkjazz-labs/runonspark-manager/internal/store"
)

type Engine struct {
	store       *store.Store
	executor    operations.Executor
	recipes     []recipe.Recipe
	mu          sync.Mutex
	running     map[string]context.CancelFunc
	recipeLocks map[string]*sync.Mutex
	// runtime serializes container and activation mutations across recipes;
	// downloads and preflight run outside it so a long install cannot block
	// stopping an unrelated model.
	runtime chan struct{}
}

type RemovePayload struct {
	RemoveArtifacts bool `json:"remove_artifacts"`
}

type plannedOperation struct {
	Operation   recipe.Operation
	Recipe      recipe.Recipe
	BeginSwitch bool
	Receipt     map[string]any
}

func New(s *store.Store, executor operations.Executor, recipes []recipe.Recipe) *Engine {
	return &Engine{store: s, executor: executor, recipes: recipes, running: map[string]context.CancelFunc{}, recipeLocks: map[string]*sync.Mutex{}, runtime: make(chan struct{}, 1)}
}

func (e *Engine) recipeLock(recipeID string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	lock, ok := e.recipeLocks[recipeID]
	if !ok {
		lock = &sync.Mutex{}
		e.recipeLocks[recipeID] = lock
	}
	return lock
}

func (e *Engine) acquireRuntime(ctx context.Context) error {
	select {
	case e.runtime <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) releaseRuntime() { <-e.runtime }

// runtimeOperations mutate or verify the shared runtime (containers, live
// memory, the single active slot) and therefore take the runtime lock.
var runtimeOperations = map[string]bool{
	"stop_container": true, "create_container": true, "verify_memory": true,
	"start_container": true, "wait_http": true, "verify_openai_inference": true,
	"remove_container": true,
}

func (e *Engine) ResumeInterrupted(ctx context.Context) error {
	jobs, err := e.store.ListJobs(ctx, 100)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.State == "interrupted" {
			e.Start(job.ID)
		}
	}
	return nil
}

func (e *Engine) ReconcileActiveModel(ctx context.Context) error {
	models, err := e.store.Models(ctx)
	if err != nil {
		return err
	}
	for _, model := range models {
		if !model.Active || model.Status != "recovering" {
			continue
		}
		job, _, err := e.store.CreateJob(ctx, "start", model.RecipeID, fmt.Sprintf("reconcile-%d", time.Now().UnixNano()), map[string]any{"recovery": true})
		if err != nil {
			return err
		}
		e.Start(job.ID)
	}
	return nil
}

func (e *Engine) Start(jobID string) {
	e.mu.Lock()
	if _, exists := e.running[jobID]; exists {
		e.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.running[jobID] = cancel
	e.mu.Unlock()
	go func() {
		defer func() { e.mu.Lock(); delete(e.running, jobID); e.mu.Unlock() }()
		e.run(ctx, jobID)
	}()
}

func (e *Engine) Cancel(ctx context.Context, jobID string) error {
	e.mu.Lock()
	cancel := e.running[jobID]
	e.mu.Unlock()
	if cancel == nil {
		job, err := e.store.GetJob(ctx, jobID)
		if err != nil {
			return err
		}
		if terminal(job.State) {
			return errors.New("job is already complete")
		}
		return e.store.UpdateJobState(ctx, jobID, "cancelled", "cancelled at a safe operation boundary")
	}
	cancel()
	// The running goroutine owns the final state: it may still need to roll
	// back a partial switch, so only cancellation intent is recorded here and
	// the job stays non-terminal until the goroutine finishes.
	marked, err := e.store.MarkCancelling(ctx, jobID)
	if err != nil {
		return err
	}
	_ = marked // false means the goroutine already wrote a terminal state
	return nil
}

func (e *Engine) run(ctx context.Context, jobID string) {
	job, err := e.store.GetJob(ctx, jobID)
	if err != nil {
		return
	}
	r, ok := recipe.Find(e.recipes, job.RecipeID)
	if !ok {
		_ = e.store.UpdateJobState(ctx, jobID, "failed", "recipe is no longer available")
		return
	}
	lock := e.recipeLock(r.ID)
	lock.Lock()
	defer lock.Unlock()
	runtimeHeld := false
	defer func() {
		if runtimeHeld {
			e.releaseRuntime()
		}
	}()
	execution := operations.Execution{JobID: job.ID, Kind: job.Kind}
	if job.Kind == "remove" {
		var payload RemovePayload
		_ = json.Unmarshal(job.Payload, &payload)
		execution.RemoveArtifacts = payload.RemoveArtifacts
		if payload.RemoveArtifacts {
			shared, sharedErr := e.sharedArtifacts(ctx, r.ID)
			if sharedErr != nil {
				_ = e.store.UpdateJobState(ctx, jobID, "failed", redact.String(sharedErr.Error()))
				return
			}
			execution.SharedArtifacts = shared
		}
	}
	plans, previous, err := e.plan(ctx, job, r)
	if err != nil {
		_ = e.store.UpdateJobState(ctx, jobID, "failed", redact.String(err.Error()))
		return
	}
	if previous != nil && rollbackWasVerified(job) {
		if err := e.store.SetModelState(ctx, r.ID, "stopped", false); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.fail(ctx, jobID, len(plans)-1, fmt.Errorf("recover completed rollback target state: %w", err))
			return
		}
		if err := e.store.SetOnlyActive(ctx, previous.ID); err != nil {
			e.fail(ctx, jobID, len(plans)-1, fmt.Errorf("recover completed rollback active model: %w", err))
			return
		}
		message := strings.TrimSpace(job.Error)
		if message == "" {
			message = "target switch failed before manager restart"
		}
		_ = e.store.UpdateJobState(ctx, jobID, "failed", fmt.Sprintf("%s; previous model %s restored and verified", redact.String(message), previous.ID))
		return
	}
	switchStarted := false
	for index, plan := range plans {
		if !runtimeHeld && (plan.BeginSwitch || runtimeOperations[plan.Operation.Type]) {
			if err := e.acquireRuntime(ctx); err != nil {
				if switchStarted && previous != nil {
					e.failSwitch(job, r, *previous, plans, index, err)
				} else {
					e.fail(ctx, jobID, index, err)
				}
				return
			}
			runtimeHeld = true
		}
		if plan.BeginSwitch {
			if previous == nil {
				e.fail(ctx, jobID, index, errors.New("switch plan lost the previous model"))
				return
			}
			if err := e.store.BeginSwitch(ctx, previous.ID, r.ID); err != nil {
				e.fail(ctx, jobID, index, fmt.Errorf("record switch intent: %w", err))
				return
			}
			switchStarted = true
		}
		if ctx.Err() != nil {
			if switchStarted && previous != nil {
				e.failSwitch(job, r, *previous, plans, index, ctx.Err())
			} else {
				_ = e.store.UpdateJobState(context.Background(), jobID, "cancelled", "cancelled at a safe operation boundary")
			}
			return
		}
		op, target := plan.Operation, plan.Recipe
		previousStep, exists, err := e.store.Step(ctx, jobID, index)
		if err != nil {
			if switchStarted && previous != nil {
				e.failSwitch(job, r, *previous, plans, index, err)
			} else {
				e.fail(ctx, jobID, index, err)
			}
			return
		}
		if exists && previousStep.State == "completed" && (plan.Receipt != nil || e.executor.Completed(ctx, execution, op, target, previousStep.Receipt)) {
			continue
		}
		if err := e.store.UpdateJobState(ctx, jobID, stateFor(job.Kind, op.Type), ""); err != nil {
			return
		}
		if err := e.store.BeginStep(ctx, jobID, index, op.Type); err != nil {
			if switchStarted && previous != nil {
				e.failSwitch(job, r, *previous, plans, index, err)
			} else {
				e.fail(ctx, jobID, index, err)
			}
			return
		}
		if plan.Receipt != nil {
			if err := e.store.CompleteStep(ctx, jobID, index, redact.JSON(plan.Receipt)); err != nil {
				e.fail(ctx, jobID, index, err)
				return
			}
			continue
		}
		progress := func(value any) error { return e.store.UpdateStepReceipt(ctx, jobID, index, redact.JSON(value)) }
		receipt, err := e.executor.Execute(ctx, execution, op, target, progress)
		if err != nil {
			if switchStarted && previous != nil {
				e.failSwitch(job, r, *previous, plans, index, err)
			} else {
				e.fail(ctx, jobID, index, err)
			}
			return
		}
		if ctx.Err() != nil {
			if switchStarted && previous != nil {
				e.failSwitch(job, r, *previous, plans, index, ctx.Err())
			} else {
				e.fail(ctx, jobID, index, ctx.Err())
			}
			return
		}
		if err := e.store.CompleteStep(ctx, jobID, index, redact.JSON(receipt)); err != nil {
			if switchStarted && previous != nil {
				e.failSwitch(job, r, *previous, plans, index, err)
			} else {
				e.fail(ctx, jobID, index, err)
			}
			return
		}
	}
	if ctx.Err() != nil {
		if switchStarted && previous != nil {
			e.failSwitch(job, r, *previous, plans, len(plans)-1, ctx.Err())
		} else {
			_ = e.store.UpdateJobState(context.Background(), jobID, "cancelled", "cancelled at a safe operation boundary")
		}
		return
	}
	if err := e.finish(ctx, job, r); err != nil {
		if switchStarted && previous != nil {
			e.failSwitch(job, r, *previous, plans, len(plans)-1, err)
		} else {
			e.fail(ctx, jobID, len(plans)-1, err)
		}
		return
	}
}

func (e *Engine) plan(ctx context.Context, job store.Job, target recipe.Recipe) ([]plannedOperation, *recipe.Recipe, error) {
	var ops []recipe.Operation
	switch job.Kind {
	case "install":
		ops = target.Operations
	case "start":
		ops = []recipe.Operation{{Type: "verify_memory"}, {Type: "start_container"}, {Type: "wait_http"}, {Type: "verify_openai_inference"}}
	case "stop":
		ops = []recipe.Operation{{Type: "stop_container"}}
	case "smoke-test":
		ops = []recipe.Operation{{Type: "wait_http"}, {Type: "verify_openai_inference"}}
	case "remove":
		var payload RemovePayload
		_ = json.Unmarshal(job.Payload, &payload)
		ops = target.Uninstall
	default:
		return nil, nil, fmt.Errorf("unknown job kind %s", job.Kind)
	}
	plans := make([]plannedOperation, 0, len(ops)+1)
	if job.Kind != "install" && job.Kind != "start" {
		for _, op := range ops {
			plans = append(plans, plannedOperation{Operation: op, Recipe: target})
		}
		return plans, nil, nil
	}
	previous, err := e.activeRecipe(ctx, target.ID)
	if err != nil || previous == nil {
		for _, op := range ops {
			plans = append(plans, plannedOperation{Operation: op, Recipe: target})
		}
		return plans, previous, err
	}
	switchStopPlanned := false
	for _, op := range ops {
		if job.Kind == "install" && op.Type == "verify_port" && previous.Service.DefaultHostPort == target.Service.DefaultHostPort {
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Receipt: map[string]any{"host_port": target.Service.DefaultHostPort, "occupied_by_managed_recipe": previous.ID, "available_after_switch": true}})
			continue
		}
		if !switchStopPlanned && (op.Type == "verify_memory" || op.Type == "start_container") {
			plans = append(plans, plannedOperation{Operation: recipe.Operation{Type: "stop_container"}, Recipe: *previous, BeginSwitch: true})
			switchStopPlanned = true
		}
		plans = append(plans, plannedOperation{Operation: op, Recipe: target})
	}
	return plans, previous, nil
}

func (e *Engine) sharedArtifacts(ctx context.Context, excludeRecipeID string) (map[string]bool, error) {
	models, err := e.store.Models(ctx)
	if err != nil {
		return nil, err
	}
	shared := map[string]bool{}
	for _, model := range models {
		if model.RecipeID == excludeRecipeID {
			continue
		}
		if model.ArtifactPath != "" {
			shared[model.ArtifactPath] = true
		}
		other, ok := recipe.Find(e.recipes, model.RecipeID)
		if !ok {
			continue
		}
		for _, artifact := range other.Artifacts {
			shared[operations.ArtifactKey(artifact)] = true
		}
	}
	return shared, nil
}

func (e *Engine) activeRecipe(ctx context.Context, targetID string) (*recipe.Recipe, error) {
	models, err := e.store.Models(ctx)
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		if !model.Active || model.RecipeID == targetID {
			continue
		}
		active, ok := recipe.Find(e.recipes, model.RecipeID)
		if !ok {
			return nil, fmt.Errorf("active model recipe %s is no longer available", model.RecipeID)
		}
		return &active, nil
	}
	return nil, nil
}

func rollbackWasVerified(job store.Job) bool {
	for _, step := range job.Steps {
		if step.Operation == "rollback_verify_openai_inference" && step.State == "completed" {
			return true
		}
	}
	return false
}

func (e *Engine) failSwitch(job store.Job, target, previous recipe.Recipe, plans []plannedOperation, failedIndex int, cause error) {
	original := redact.String(cause.Error())
	_ = e.store.FailStep(context.Background(), job.ID, failedIndex, original)
	_ = e.store.UpdateJobState(context.Background(), job.ID, "rolling_back", original)
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	execution := operations.Execution{JobID: job.ID, Kind: "rollback"}
	rollback := []plannedOperation{
		{Operation: recipe.Operation{Type: "stop_container"}, Recipe: target},
		{Operation: recipe.Operation{Type: "verify_memory"}, Recipe: previous},
		{Operation: recipe.Operation{Type: "start_container"}, Recipe: previous},
		{Operation: recipe.Operation{Type: "wait_http"}, Recipe: previous},
		{Operation: recipe.Operation{Type: "verify_openai_inference"}, Recipe: previous},
	}
	var rollbackErr error
	for offset, plan := range rollback {
		index := len(plans) + offset
		name := "rollback_" + plan.Operation.Type
		if err := e.store.BeginStep(rollbackCtx, job.ID, index, name); err != nil {
			rollbackErr = err
			break
		}
		if e.executor.Completed(rollbackCtx, execution, plan.Operation, plan.Recipe, nil) {
			if err := e.store.CompleteStep(rollbackCtx, job.ID, index, redact.JSON(map[string]any{"operation": plan.Operation.Type, "already_satisfied": true})); err != nil {
				rollbackErr = err
				break
			}
			continue
		}
		receipt, err := e.executor.Execute(rollbackCtx, execution, plan.Operation, plan.Recipe, nil)
		if err != nil {
			rollbackErr = err
			_ = e.store.FailStep(context.Background(), job.ID, index, redact.String(err.Error()))
			break
		}
		if err := e.store.CompleteStep(rollbackCtx, job.ID, index, redact.JSON(receipt)); err != nil {
			rollbackErr = err
			break
		}
	}
	finalState := "failed"
	if errors.Is(cause, context.Canceled) {
		finalState = "cancelled"
	}
	message := original
	if rollbackErr == nil {
		if err := e.store.SetModelState(context.Background(), target.ID, "stopped", false); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErr = err
		}
	}
	if rollbackErr == nil {
		if err := e.store.SetOnlyActive(context.Background(), previous.ID); err != nil {
			rollbackErr = err
		}
	}
	if rollbackErr == nil {
		message = fmt.Sprintf("%s; previous model %s restored and verified", original, previous.ID)
	} else {
		_ = e.store.SetModelState(context.Background(), target.ID, "failed", false)
		_ = e.store.SetModelState(context.Background(), previous.ID, "failed", false)
		message = fmt.Sprintf("%s; rollback to %s failed: %s", original, previous.ID, redact.String(rollbackErr.Error()))
	}
	_ = e.store.UpdateJobState(context.Background(), job.ID, finalState, message)
}

func (e *Engine) finish(ctx context.Context, job store.Job, r recipe.Recipe) error {
	switch job.Kind {
	case "install", "start":
		containerID := e.containerID(ctx, job.ID)
		if existing, err := e.store.Model(ctx, r.ID); err == nil && containerID == "" {
			containerID = existing.ContainerID
		}
		model := store.InstalledModel{RecipeID: r.ID, RecipeVersion: r.Version, Status: "ready", ArtifactPath: e.executor.ArtifactPath(r), ContainerID: containerID, Active: true}
		if err := e.store.ActivateExclusively(ctx, model); err != nil {
			return err
		}
		return e.store.UpdateJobState(ctx, job.ID, "ready", "")
	case "stop":
		model, err := e.store.Model(ctx, r.ID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			model.Status, model.Active = "stopped", false
			if err := e.store.SetInstalled(ctx, model); err != nil {
				return err
			}
		}
		return e.store.UpdateJobState(ctx, job.ID, "stopped", "")
	case "remove":
		if err := e.store.DeleteModel(ctx, r.ID); err != nil {
			return err
		}
		return e.store.UpdateJobState(ctx, job.ID, "removed", "")
	case "smoke-test":
		return e.store.UpdateJobState(ctx, job.ID, "ready", "")
	default:
		return fmt.Errorf("cannot finish job kind %s", job.Kind)
	}
}

func (e *Engine) containerID(ctx context.Context, jobID string) string {
	job, err := e.store.GetJob(ctx, jobID)
	if err != nil {
		return ""
	}
	for _, step := range job.Steps {
		var receipt map[string]any
		if json.Unmarshal(step.Receipt, &receipt) == nil {
			if id, ok := receipt["container_id"].(string); ok && id != "" {
				return id
			}
		}
	}
	return ""
}

func (e *Engine) fail(ctx context.Context, jobID string, index int, err error) {
	message := redact.String(err.Error())
	_ = e.store.FailStep(context.Background(), jobID, index, message)
	state := "failed"
	if errors.Is(err, context.Canceled) {
		state = "cancelled"
	}
	_ = e.store.UpdateJobState(context.Background(), jobID, state, message)
}

func stateFor(kind, operation string) string {
	states := map[string]string{
		"verify_architecture": "preflighting", "verify_dgx_spark": "preflighting", "verify_memory_capacity": "preflighting", "verify_memory": "checking_memory", "verify_disk": "preflighting", "verify_port": "preflighting", "verify_docker": "preflighting", "verify_nvidia_runtime": "preflighting", "verify_artifact_access": "preflighting",
		"pull_image": "downloading_runtime", "download_artifact": "downloading_models", "write_generated_config": "configuring", "create_container": "configuring",
		"start_container": "starting", "wait_http": "verifying_health", "verify_openai_inference": "verifying_inference", "stop_container": "stopping", "remove_container": "removing", "remove_artifact_if_unshared": "removing",
	}
	if state := states[operation]; state != "" {
		return state
	}
	return kind
}

func terminal(state string) bool {
	switch state {
	case "ready", "failed", "cancelled", "stopped", "removed":
		return true
	}
	return false
}
