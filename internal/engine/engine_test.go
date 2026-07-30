package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/punkjazz-labs/runonspark-manager/internal/operations"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
	"github.com/punkjazz-labs/runonspark-manager/internal/store"
)

type fakeExecutor struct {
	mu                                          sync.Mutex
	image, artifact, config, container, running bool
	failPull                                    bool
}

func (f *fakeExecutor) ArtifactPath(r recipe.Recipe) string { return "/managed/" + r.ID }
func (f *fakeExecutor) Execute(_ context.Context, execution operations.Execution, op recipe.Operation, _ recipe.Recipe, progress operations.Progress) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch op.Type {
	case "pull_image":
		if f.failPull {
			return nil, errors.New("Authorization: Bearer " + "hf_" + "SUPERSECRET1234567890")
		}
		f.image = true
	case "download_artifact":
		if progress != nil {
			_ = progress(map[string]any{"bytes_complete": 50, "bytes_total": 100, "percent": 50})
		}
		f.artifact = true
	case "write_generated_config":
		f.config = true
	case "create_container":
		f.container = true
		return map[string]any{"operation": op.Type, "ok": true, "container_id": "container-test-id"}, nil
	case "start_container":
		if !f.container {
			return nil, errors.New("container missing")
		}
		f.running = true
	case "wait_http":
		if !f.running {
			return nil, errors.New("not running")
		}
	case "verify_openai_inference":
		if !f.running {
			return nil, errors.New("not running")
		}
		return map[string]any{"response_non_empty": true}, nil
	case "stop_container":
		f.running = false
	case "remove_container":
		f.container = false
	case "remove_artifact_if_unshared":
		if execution.RemoveArtifacts {
			f.artifact = false
		}
	}
	return map[string]any{"operation": op.Type, "ok": true}, nil
}
func (f *fakeExecutor) Completed(_ context.Context, execution operations.Execution, op recipe.Operation, _ recipe.Recipe, _ json.RawMessage) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch op.Type {
	case "pull_image":
		return f.image
	case "download_artifact":
		return f.artifact
	case "write_generated_config":
		return f.config
	case "create_container":
		return f.container
	case "start_container", "wait_http", "verify_openai_inference":
		return f.running
	case "stop_container":
		return !f.running
	case "remove_container":
		return !f.container
	case "remove_artifact_if_unshared":
		return !execution.RemoveArtifacts || !f.artifact
	default:
		return false
	}
}

func TestQwenLifecycleVerticalSlice(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeExecutor{}
	runner := New(s, fake, recipes)
	id := recipes[0].ID
	install, created, err := s.CreateJob(ctx, "install", id, "install-once", map[string]bool{"confirmed": true})
	if err != nil || !created {
		t.Fatalf("CreateJob() created=%v err=%v", created, err)
	}
	runner.Start(install.ID)
	ready := waitJob(t, s, install.ID, "ready")
	if len(ready.Steps) != len(recipes[0].Operations) {
		t.Fatalf("install steps=%d want %d", len(ready.Steps), len(recipes[0].Operations))
	}
	duplicate, created, err := s.CreateJob(ctx, "install", id, "install-once", map[string]bool{"confirmed": true})
	if err != nil || created || duplicate.ID != install.ID {
		t.Fatalf("idempotency failed: created=%v id=%s err=%v", created, duplicate.ID, err)
	}
	model, err := s.Model(ctx, id)
	if err != nil || !model.Active || model.Status != "ready" || model.ContainerID != "container-test-id" {
		t.Fatalf("model=%#v err=%v", model, err)
	}
	for _, item := range []struct {
		kind, key, want string
		payload         any
	}{{"stop", "stop-once", "stopped", map[string]any{}}, {"start", "start-once", "ready", map[string]any{}}, {"smoke-test", "smoke-once", "ready", map[string]any{}}, {"remove", "remove-once", "removed", RemovePayload{RemoveArtifacts: true}}} {
		job, _, err := s.CreateJob(ctx, item.kind, id, item.key, item.payload)
		if err != nil {
			t.Fatal(err)
		}
		runner.Start(job.ID)
		waitJob(t, s, job.ID, item.want)
	}
	if _, err := s.Model(ctx, id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed model still exists: %v", err)
	}
	if fake.artifact || fake.container || fake.running {
		t.Fatalf("owned runtime state was not removed: %#v", fake)
	}
}

func TestExecutorErrorsAreRedacted(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	runner := New(s, &fakeExecutor{failPull: true}, recipes)
	job, _, _ := s.CreateJob(ctx, "install", recipes[0].ID, "redact", map[string]any{})
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if strings.Contains(failed.Error, "hf_SUPERSECRET") || !strings.Contains(failed.Error, "[REDACTED]") {
		t.Fatalf("secret was not redacted: %q", failed.Error)
	}
}

func TestRestartReconcilesHealthBeforeRestoringReady(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recipes, _ := recipe.Builtin()
	id := recipes[0].ID
	if err := s.SetInstalled(ctx, store.InstalledModel{RecipeID: id, RecipeVersion: 1, Status: "ready", ArtifactPath: "/managed/" + id, ContainerID: "existing-container", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runner := New(s, &fakeExecutor{image: true, artifact: true, config: true, container: true, running: true}, recipes)
	if err := runner.ReconcileActiveModel(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		model, err := s.Model(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if model.Status == "ready" {
			if model.ContainerID != "existing-container" {
				t.Fatalf("container ID was lost: %#v", model)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	model, _ := s.Model(ctx, id)
	t.Fatalf("model was not reconciled: %#v", model)
}

func waitJob(t *testing.T, s *store.Store, id, want string) store.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := s.GetJob(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == want {
			return job
		}
		if job.State == "failed" && want != "failed" {
			t.Fatalf("job failed: %s", job.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := s.GetJob(context.Background(), id)
	t.Fatalf("job state=%s, want %s", job.State, want)
	return store.Job{}
}
