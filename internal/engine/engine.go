package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/punkjazz-labs/runonspark-manager/internal/operations"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
	"github.com/punkjazz-labs/runonspark-manager/internal/redact"
	"github.com/punkjazz-labs/runonspark-manager/internal/store"
)

type Engine struct {
	store     *store.Store
	executor  operations.Executor
	recipes   []recipe.Recipe
	mu        sync.Mutex
	running   map[string]context.CancelFunc
	lifecycle sync.Mutex
}

type RemovePayload struct {
	RemoveArtifacts bool `json:"remove_artifacts"`
}

func New(s *store.Store, executor operations.Executor, recipes []recipe.Recipe) *Engine {
	return &Engine{store: s, executor: executor, recipes: recipes, running: map[string]context.CancelFunc{}}
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
		e.lifecycle.Lock()
		defer e.lifecycle.Unlock()
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
	} else {
		cancel()
	}
	return e.store.UpdateJobState(ctx, jobID, "cancelled", "cancelled at a safe operation boundary")
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
	execution := operations.Execution{JobID: job.ID, Kind: job.Kind}
	var ops []recipe.Operation
	switch job.Kind {
	case "install":
		ops = r.Operations
	case "start":
		ops = []recipe.Operation{{Type: "start_container"}, {Type: "wait_http"}, {Type: "verify_openai_inference"}}
	case "stop":
		ops = []recipe.Operation{{Type: "stop_container"}}
	case "smoke-test":
		ops = []recipe.Operation{{Type: "wait_http"}, {Type: "verify_openai_inference"}}
	case "remove":
		var payload RemovePayload
		_ = json.Unmarshal(job.Payload, &payload)
		execution.RemoveArtifacts = payload.RemoveArtifacts
		ops = r.Uninstall
	default:
		_ = e.store.UpdateJobState(ctx, jobID, "failed", "unknown job kind")
		return
	}
	for index, op := range ops {
		if ctx.Err() != nil {
			_ = e.store.UpdateJobState(context.Background(), jobID, "cancelled", "cancelled at a safe operation boundary")
			return
		}
		previous, exists, err := e.store.Step(ctx, jobID, index)
		if err != nil {
			e.fail(ctx, jobID, index, err)
			return
		}
		if exists && previous.State == "completed" && e.executor.Completed(ctx, execution, op, r, previous.Receipt) {
			continue
		}
		if err := e.store.UpdateJobState(ctx, jobID, stateFor(job.Kind, op.Type), ""); err != nil {
			return
		}
		if err := e.store.BeginStep(ctx, jobID, index, op.Type); err != nil {
			e.fail(ctx, jobID, index, err)
			return
		}
		progress := func(value any) error { return e.store.UpdateStepReceipt(ctx, jobID, index, redact.JSON(value)) }
		receipt, err := e.executor.Execute(ctx, execution, op, r, progress)
		if err != nil {
			e.fail(ctx, jobID, index, err)
			return
		}
		if ctx.Err() != nil {
			e.fail(ctx, jobID, index, ctx.Err())
			return
		}
		if err := e.store.CompleteStep(ctx, jobID, index, redact.JSON(receipt)); err != nil {
			e.fail(ctx, jobID, index, err)
			return
		}
	}
	if ctx.Err() != nil {
		_ = e.store.UpdateJobState(context.Background(), jobID, "cancelled", "cancelled at a safe operation boundary")
		return
	}
	if err := e.finish(ctx, job, r); err != nil {
		e.fail(ctx, jobID, len(ops)-1, err)
		return
	}
}

func (e *Engine) finish(ctx context.Context, job store.Job, r recipe.Recipe) error {
	switch job.Kind {
	case "install", "start":
		containerID := e.containerID(ctx, job.ID)
		if existing, err := e.store.Model(ctx, r.ID); err == nil && containerID == "" {
			containerID = existing.ContainerID
		}
		model := store.InstalledModel{RecipeID: r.ID, RecipeVersion: r.Version, Status: "ready", ArtifactPath: e.executor.ArtifactPath(r), ContainerID: containerID, Active: true}
		if err := e.store.SetInstalled(ctx, model); err != nil {
			return err
		}
		if err := e.store.SetOnlyActive(ctx, r.ID); err != nil {
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
		"verify_architecture": "preflighting", "verify_dgx_spark": "preflighting", "verify_disk": "preflighting", "verify_port": "preflighting", "verify_docker": "preflighting", "verify_nvidia_runtime": "preflighting", "verify_artifact_access": "preflighting",
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
