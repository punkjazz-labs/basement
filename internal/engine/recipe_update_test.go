package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

// versionedExecutor keys its simulated container state by "id@version",
// mirroring host.go's containerName (which embeds the recipe version), so a
// test can tell whether the engine operated on the OLD or the NEW version of
// a self-updated recipe — the two must never be conflated.
type versionedExecutor struct {
	mu               sync.Mutex
	running          map[string]bool
	failStartVersion int // 0 means never fail
	events           []string
}

func versionKey(r recipe.Recipe) string { return fmt.Sprintf("%s@%d", r.ID, r.Version) }

func (e *versionedExecutor) ArtifactPath(r recipe.Recipe) string { return "/managed/" + r.ID }
func (e *versionedExecutor) RuntimeImageBytes(context.Context, recipe.Recipe) (int64, bool) {
	return 0, false
}
func (e *versionedExecutor) Execute(_ context.Context, _ operations.Execution, op recipe.Operation, r recipe.Recipe, _ operations.Progress) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, op.Type+":"+versionKey(r))
	switch op.Type {
	case "stop_container":
		e.running[versionKey(r)] = false
	case "create_container":
		return map[string]any{"container_id": "container-" + versionKey(r)}, nil
	case "start_container":
		if r.Version == e.failStartVersion {
			return nil, errors.New("start failed")
		}
		e.running[versionKey(r)] = true
	case "wait_http":
		if !e.running[versionKey(r)] {
			return nil, errors.New("not running")
		}
	case "verify_openai_inference":
		if !e.running[versionKey(r)] {
			return nil, errors.New("not running")
		}
	}
	return map[string]any{"operation": op.Type}, nil
}
func (e *versionedExecutor) Completed(_ context.Context, _ operations.Execution, op recipe.Operation, r recipe.Recipe, _ json.RawMessage) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch op.Type {
	case "stop_container":
		return !e.running[versionKey(r)]
	case "start_container", "wait_http", "verify_openai_inference":
		return e.running[versionKey(r)]
	default:
		return false
	}
}

// bumpedVersion returns a copy of r with Version increased by one, keeping
// every other field — the shape of a routine recipe update.
func bumpedVersion(r recipe.Recipe) recipe.Recipe {
	next := r
	next.Version++
	return next
}

func TestRecipeUpdateDoesNotChangeAlreadyInstalledModelResolution(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	embedded, _ := recipe.Builtin()
	v1 := singleSpark(embedded)
	v2 := bumpedVersion(v1)
	if err := s.SetInstalled(ctx, store.InstalledModel{RecipeID: v1.ID, RecipeVersion: v1.Version, Status: "ready", ArtifactPath: "/managed/" + v1.ID, Active: true}); err != nil {
		t.Fatal(err)
	}
	executor := &versionedExecutor{running: map[string]bool{versionKey(v1): true}}
	runner := New(s, executor, []recipe.Recipe{v1})
	// A background recipe-index refresh lands a new version for this ID
	// while v1 is the one actually installed and serving.
	runner.SetRecipes([]recipe.Recipe{v1, v2}, []recipe.Recipe{v2})

	job, _, err := s.CreateJob(ctx, "stop", v1.ID, "stop-after-update", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	waitJob(t, s, job.ID, "stopped")

	executor.mu.Lock()
	defer executor.mu.Unlock()
	for _, event := range executor.events {
		if strings.Contains(event, versionKey(v2)) {
			t.Fatalf("stop operated on the new (not-installed) version: %v", executor.events)
		}
	}
	if executor.running[versionKey(v1)] {
		t.Fatalf("the actually-installed v1 container was not stopped: %#v", executor.running)
	}
}

func TestSelfUpdateWhileServingSwitchesToNewVersion(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	embedded, _ := recipe.Builtin()
	v1 := singleSpark(embedded)
	v2 := bumpedVersion(v1)
	if err := s.SetInstalled(ctx, store.InstalledModel{RecipeID: v1.ID, RecipeVersion: v1.Version, Status: "ready", ArtifactPath: "/managed/" + v1.ID, Active: true}); err != nil {
		t.Fatal(err)
	}
	executor := &versionedExecutor{running: map[string]bool{versionKey(v1): true}}
	runner := New(s, executor, []recipe.Recipe{v1})
	runner.SetRecipes([]recipe.Recipe{v1, v2}, []recipe.Recipe{v2})

	job, _, err := s.CreateJob(ctx, "install", v1.ID, "update-switch-now", map[string]any{"confirmed": true, "activate": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	waitJob(t, s, job.ID, "ready")

	model, err := s.Model(ctx, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if model.RecipeVersion != v2.Version || !model.Active || model.Status != "ready" {
		t.Fatalf("model after self-update = %#v, want version %d active ready", model, v2.Version)
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	joined := strings.Join(executor.events, ",")
	stopOld := "stop_container:" + versionKey(v1)
	startNew := "start_container:" + versionKey(v2)
	if strings.Index(joined, stopOld) == -1 || strings.Index(joined, startNew) == -1 || strings.Index(joined, stopOld) > strings.Index(joined, startNew) {
		t.Fatalf("expected the old version stopped before the new one started, got: %s", joined)
	}
}

func TestSelfUpdateFailureRestoresOldVersionNotNewOne(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	embedded, _ := recipe.Builtin()
	v1 := singleSpark(embedded)
	v2 := bumpedVersion(v1)
	if err := s.SetInstalled(ctx, store.InstalledModel{RecipeID: v1.ID, RecipeVersion: v1.Version, Status: "ready", ArtifactPath: "/managed/" + v1.ID, Active: true}); err != nil {
		t.Fatal(err)
	}
	executor := &versionedExecutor{running: map[string]bool{versionKey(v1): true}, failStartVersion: v2.Version}
	runner := New(s, executor, []recipe.Recipe{v1})
	runner.SetRecipes([]recipe.Recipe{v1, v2}, []recipe.Recipe{v2})

	job, _, err := s.CreateJob(ctx, "install", v1.ID, "update-switch-fails", map[string]any{"confirmed": true, "activate": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if !strings.Contains(failed.Error, "previous model "+v1.ID+" restored and verified") {
		t.Fatalf("rollback outcome missing from job error: %q", failed.Error)
	}

	model, err := s.Model(ctx, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The rollback must restore the OLD version's row, not leave the model
	// pointed at the new version that never actually came up.
	if model.RecipeVersion != v1.Version || !model.Active || model.Status != "ready" {
		t.Fatalf("model after failed self-update = %#v, want version %d active ready", model, v1.Version)
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if !executor.running[versionKey(v1)] {
		t.Fatalf("old version was not restarted during rollback: %#v", executor.running)
	}
	if executor.running[versionKey(v2)] {
		t.Fatalf("failed new version was left running: %#v", executor.running)
	}
}
