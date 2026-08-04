package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/recipe/recipetest"
	"github.com/punkjazz-labs/basement/internal/store"
)

// mediaExecutor records every operation it is asked to run, so a test can
// assert on the plan the engine built rather than on what the runtime did.
type mediaExecutor struct {
	mu      sync.Mutex
	steps   []string
	running bool
}

func (m *mediaExecutor) ArtifactPath(r recipe.Recipe) string { return "/managed/" + r.ID }
func (m *mediaExecutor) RuntimeImageBytes(context.Context, recipe.Recipe) (int64, bool) {
	return 0, false
}
func (m *mediaExecutor) Execute(_ context.Context, _ operations.Execution, op recipe.Operation, _ recipe.Recipe, _ operations.Progress) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, op.Type)
	switch op.Type {
	case "start_container":
		m.running = true
	case "stop_container":
		m.running = false
	}
	return map[string]any{"operation": op.Type}, nil
}
func (m *mediaExecutor) Completed(_ context.Context, _ operations.Execution, op recipe.Operation, _ recipe.Recipe, _ json.RawMessage) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch op.Type {
	case "start_container", "wait_http", "verify_media_generation":
		return m.running
	}
	return false
}

func (m *mediaExecutor) ran(operation string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, step := range m.steps {
		if step == operation {
			return true
		}
	}
	return false
}

// TestMediaJobsVerifyByGenerating proves the kind-aware verification reaches
// every job that plans one. Install takes its sequence from the recipe, but
// start and smoke-test build theirs in the engine, and a media model asked
// for a chat completion would fail on a model that is working perfectly.
func TestMediaJobsVerifyByGenerating(t *testing.T) {
	recipetest.WithTextToVideoGraph(t)
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	media := recipetest.Media()
	if err := recipe.Validate(media); err != nil {
		t.Fatalf("the media fixture must stay a valid recipe: %v", err)
	}
	recipes := []recipe.Recipe{media}
	executor := &mediaExecutor{}
	runner := New(database, executor, recipes)

	install, _, err := database.CreateJob(ctx, "install", media.ID, "media-install", map[string]any{"confirmed": true, "activate": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(install.ID)
	job := waitJob(t, database, install.ID, "ready")
	if !endsWith(job, "verify_media_generation") {
		t.Fatalf("install did not end in the media verification: %+v", job.Steps)
	}

	for _, kind := range []string{"start", "smoke-test"} {
		executor.mu.Lock()
		executor.steps = nil
		executor.mu.Unlock()
		created, _, err := database.CreateJob(ctx, kind, media.ID, "media-"+kind, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		runner.Start(created.ID)
		waitJob(t, database, created.ID, "ready")
		if !executor.ran("verify_media_generation") {
			t.Fatalf("%s did not verify by generating: %v", kind, executor.steps)
		}
		if executor.ran("verify_openai_inference") {
			t.Fatalf("%s asked a media model for tokens: %v", kind, executor.steps)
		}
	}
}

func endsWith(job store.Job, operation string) bool {
	if len(job.Steps) == 0 {
		return false
	}
	return strings.HasPrefix(job.Steps[len(job.Steps)-1].Operation, operation)
}

// TestTextJobsStillVerifyByAsking is the other half: nothing that shipped
// before this kind changed how it is proved.
func TestTextJobsStillVerifyByAsking(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recipes {
		if got := recipe.InferenceVerification(r.Runtime.Kind); got != "verify_openai_inference" {
			t.Fatalf("%s verifies with %s", r.ID, got)
		}
	}
	// A download-only install still stops before the verification step,
	// whichever verification the kind uses.
	trimmed := downloadOnlyOperations(recipetest.Media().Operations)
	for _, op := range trimmed {
		if strings.HasPrefix(op.Type, "verify_media_generation") || op.Type == "start_container" {
			t.Fatalf("a download-only media install must not reach %s: %+v", op.Type, trimmed)
		}
	}
	if len(trimmed) == 0 {
		t.Fatal("a download-only media install must still download something")
	}
}
