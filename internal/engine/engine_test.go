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

type switchExecutor struct {
	mu           sync.Mutex
	running      map[string]bool
	failVerifyID string
	failStartID  string
	failMemoryID string
	events       []string
}

func (s *switchExecutor) ArtifactPath(r recipe.Recipe) string { return "/managed/" + r.ID }
func (s *switchExecutor) RuntimeImageBytes(context.Context, recipe.Recipe) (int64, bool) {
	return 0, false
}
func (s *switchExecutor) Execute(_ context.Context, _ operations.Execution, op recipe.Operation, r recipe.Recipe, _ operations.Progress) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, op.Type+":"+r.ID)
	switch op.Type {
	case "verify_memory":
		if r.ID == s.failMemoryID {
			return nil, errors.New("insufficient unified memory for guarded start")
		}
	case "stop_container":
		s.running[r.ID] = false
	case "start_container":
		if r.ID == s.failStartID {
			return nil, errors.New("rollback start failed")
		}
		s.running[r.ID] = true
	case "wait_http":
		if !s.running[r.ID] {
			return nil, errors.New("not running")
		}
	case "verify_openai_inference":
		if !s.running[r.ID] {
			return nil, errors.New("not running")
		}
		if r.ID == s.failVerifyID {
			return nil, errors.New("target inference verification failed")
		}
	}
	return map[string]any{"operation": op.Type, "recipe_id": r.ID}, nil
}
func (s *switchExecutor) Completed(_ context.Context, _ operations.Execution, op recipe.Operation, r recipe.Recipe, _ json.RawMessage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch op.Type {
	case "stop_container":
		return !s.running[r.ID]
	case "start_container", "wait_http":
		return s.running[r.ID]
	case "verify_openai_inference":
		return s.running[r.ID] && r.ID != s.failVerifyID
	default:
		return false
	}
}

func (f *fakeExecutor) ArtifactPath(r recipe.Recipe) string { return "/managed/" + r.ID }
func (f *fakeExecutor) RuntimeImageBytes(context.Context, recipe.Recipe) (int64, bool) {
	return 0, false
}
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

func TestPlanForDownloadOnlyInstallExcludesContainerAndSwitchOperations(t *testing.T) {
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
	runner := New(s, &fakeExecutor{}, recipes)
	job, _, err := s.CreateJob(ctx, "install", recipes[0].ID, "download-only", map[string]any{"confirmed": true, "activate": false})
	if err != nil {
		t.Fatal(err)
	}
	plans, _, err := runner.plan(ctx, job, recipes[0])
	if err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{
		"write_generated_config": true, "create_container": true, "verify_memory": true,
		"start_container": true, "wait_http": true, "verify_openai_inference": true,
		"stop_container": true, "verify_port": true,
	}
	for _, plan := range plans {
		if forbidden[plan.Operation.Type] {
			t.Fatalf("download-only plan contains %s", plan.Operation.Type)
		}
		if plan.BeginSwitch {
			t.Fatalf("download-only plan contains a switch step: %+v", plan)
		}
	}
	if len(plans) == 0 || plans[len(plans)-1].Operation.Type != "download_artifact" {
		t.Fatalf("download-only plan should end at download_artifact, got %+v", plans)
	}
}

func TestDownloadOnlyInstallLeavesModelStoppedAndSkipsTheContainer(t *testing.T) {
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
	install, _, err := s.CreateJob(ctx, "install", id, "download-only", map[string]any{"confirmed": true, "activate": false})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(install.ID)
	job := waitJob(t, s, install.ID, "ready")
	for _, step := range job.Steps {
		if step.Operation == "create_container" || step.Operation == "start_container" {
			t.Fatalf("download-only install ran %s", step.Operation)
		}
	}
	model, err := s.Model(ctx, id)
	if err != nil {
		t.Fatalf("model missing after download-only install: %v", err)
	}
	if model.Active || model.Status != "stopped" {
		t.Fatalf("download-only model should end inactive/stopped, got %#v", model)
	}
	if fake.container || fake.running {
		t.Fatalf("download-only install touched the container: %#v", fake)
	}
}

// TestDownloadOnlyInstallThenStartCreatesTheContainer covers the gap a
// download-only install leaves behind: it never wrote the runtime config or
// created the container, so the first start afterwards must do both before
// the normal start sequence, not just verify_memory/start_container against
// a container that was never created.
func TestDownloadOnlyInstallThenStartCreatesTheContainer(t *testing.T) {
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
	install, _, err := s.CreateJob(ctx, "install", id, "download-only", map[string]any{"confirmed": true, "activate": false})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(install.ID)
	waitJob(t, s, install.ID, "ready")

	start, _, err := s.CreateJob(ctx, "start", id, "start-after-download-only", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(start.ID)
	job := waitJob(t, s, start.ID, "ready")
	var sawConfig, sawContainer bool
	for _, step := range job.Steps {
		sawConfig = sawConfig || step.Operation == "write_generated_config"
		sawContainer = sawContainer || step.Operation == "create_container"
	}
	if !sawConfig || !sawContainer {
		t.Fatalf("start after a download-only install should write config and create the container, steps=%+v", job.Steps)
	}
	model, err := s.Model(ctx, id)
	if err != nil {
		t.Fatalf("model missing: %v", err)
	}
	if !model.Active || model.ContainerID == "" {
		t.Fatalf("started model should be active with a container id, got %#v", model)
	}
}

// TestPlanForStartingADownloadOnlyModelWhileAnotherServesComposesWithSwitch
// exercises the case the review flagged: X was installed download-only while
// Y served, so starting X now must both build X's container and switch Y
// out, and the two mechanisms (container creation, switch insertion) must
// compose in the right order.
func TestPlanForStartingADownloadOnlyModelWhileAnotherServesComposesWithSwitch(t *testing.T) {
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
	target, serving := recipes[0], recipes[1]
	if target.Service.DefaultHostPort != serving.Service.DefaultHostPort {
		t.Fatalf("test fixture assumes recipes share a host port: %d vs %d", target.Service.DefaultHostPort, serving.Service.DefaultHostPort)
	}
	if err := s.ActivateExclusively(ctx, store.InstalledModel{RecipeID: serving.ID, RecipeVersion: serving.Version, Status: "ready", ContainerID: "serving-container"}); err != nil {
		t.Fatal(err)
	}
	// A completed download-only install: installed, inactive, no container.
	if err := s.SetInstalled(ctx, store.InstalledModel{RecipeID: target.ID, RecipeVersion: target.Version, Status: "stopped", Active: false}); err != nil {
		t.Fatal(err)
	}
	runner := New(s, &fakeExecutor{}, recipes)
	job, _, err := s.CreateJob(ctx, "start", target.ID, "start-while-serving", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	plans, previous, err := runner.plan(ctx, job, target)
	if err != nil {
		t.Fatal(err)
	}
	if previous == nil || previous.ID != serving.ID {
		t.Fatalf("expected %s to be reported as the previously active model, got %+v", serving.ID, previous)
	}
	indexOf := func(match func(plannedOperation) bool) int {
		for i, plan := range plans {
			if match(plan) {
				return i
			}
		}
		return -1
	}
	portIndex := indexOf(func(p plannedOperation) bool { return p.Operation.Type == "verify_port" })
	configIndex := indexOf(func(p plannedOperation) bool { return p.Operation.Type == "write_generated_config" })
	containerIndex := indexOf(func(p plannedOperation) bool { return p.Operation.Type == "create_container" })
	switchIndex := indexOf(func(p plannedOperation) bool { return p.BeginSwitch })
	memoryIndex := indexOf(func(p plannedOperation) bool { return p.Operation.Type == "verify_memory" })
	startIndex := indexOf(func(p plannedOperation) bool { return p.Operation.Type == "start_container" })
	if portIndex < 0 || configIndex < 0 || containerIndex < 0 || switchIndex < 0 || memoryIndex < 0 || startIndex < 0 {
		t.Fatalf("plan is missing an expected step: %+v", plans)
	}
	if !(portIndex < configIndex && configIndex < containerIndex && containerIndex < switchIndex && switchIndex < memoryIndex && memoryIndex < startIndex) {
		t.Fatalf("plan steps are out of order: port=%d config=%d container=%d switch=%d memory=%d start=%d, plans=%+v",
			portIndex, configIndex, containerIndex, switchIndex, memoryIndex, startIndex, plans)
	}
	if plans[switchIndex].Operation.Type != "stop_container" || plans[switchIndex].Recipe.ID != serving.ID {
		t.Fatalf("switch step should stop the serving recipe %s, got %+v", serving.ID, plans[switchIndex])
	}
	portReceipt := plans[portIndex].Receipt
	if portReceipt == nil || portReceipt["available_after_switch"] != true {
		t.Fatalf("verify_port should carry the available_after_switch receipt when a switch is planned, got %+v", portReceipt)
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

func TestSwitchMakesOnlyVerifiedTargetActive(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	previous, target := recipes[0], recipes[1]
	for _, model := range []store.InstalledModel{
		{RecipeID: previous.ID, RecipeVersion: previous.Version, Status: "ready", ArtifactPath: "/managed/" + previous.ID, Active: true},
		{RecipeID: target.ID, RecipeVersion: target.Version, Status: "stopped", ArtifactPath: "/managed/" + target.ID, ContainerID: "existing-container"},
	} {
		if err := s.SetInstalled(ctx, model); err != nil {
			t.Fatal(err)
		}
	}
	executor := &switchExecutor{running: map[string]bool{previous.ID: true, target.ID: false}}
	runner := New(s, executor, recipes)
	job, _, err := s.CreateJob(ctx, "start", target.ID, "switch-success", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	completed := waitJob(t, s, job.ID, "ready")
	if len(completed.Steps) != 5 || completed.Steps[0].Operation != "stop_container" || completed.Steps[1].Operation != "verify_memory" {
		t.Fatalf("switch steps=%#v", completed.Steps)
	}
	assertActiveModel(t, s, target.ID, previous.ID, "stopped")
	if got := strings.Join(executor.events, ","); !strings.HasPrefix(got, "stop_container:"+previous.ID+",verify_memory:"+target.ID+",start_container:"+target.ID) {
		t.Fatalf("unsafe switch order: %s", got)
	}
}

func TestSwitchFailureRestoresPreviousModel(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	previous, target := recipes[0], recipes[1]
	for _, model := range []store.InstalledModel{
		{RecipeID: previous.ID, RecipeVersion: previous.Version, Status: "ready", ArtifactPath: "/managed/" + previous.ID, Active: true},
		{RecipeID: target.ID, RecipeVersion: target.Version, Status: "stopped", ArtifactPath: "/managed/" + target.ID, ContainerID: "existing-container"},
	} {
		if err := s.SetInstalled(ctx, model); err != nil {
			t.Fatal(err)
		}
	}
	executor := &switchExecutor{running: map[string]bool{previous.ID: true, target.ID: false}, failVerifyID: target.ID}
	runner := New(s, executor, recipes)
	job, _, err := s.CreateJob(ctx, "start", target.ID, "switch-rollback", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if !strings.Contains(failed.Error, "previous model "+previous.ID+" restored and verified") {
		t.Fatalf("rollback outcome missing from job: %q", failed.Error)
	}
	if len(failed.Steps) < 10 || failed.Steps[5].Operation != "rollback_stop_container" || failed.Steps[6].Operation != "rollback_verify_memory" {
		t.Fatalf("rollback receipts missing: %#v", failed.Steps)
	}
	assertActiveModel(t, s, previous.ID, target.ID, "stopped")
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if !executor.running[previous.ID] || executor.running[target.ID] {
		t.Fatalf("runtime rollback state=%#v events=%#v", executor.running, executor.events)
	}
}

func TestInstallSecondModelDownloadsBeforeSwitch(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	previous, target := recipes[0], recipes[1]
	if err := s.SetInstalled(ctx, store.InstalledModel{RecipeID: previous.ID, RecipeVersion: previous.Version, Status: "ready", ArtifactPath: "/managed/" + previous.ID, Active: true}); err != nil {
		t.Fatal(err)
	}
	executor := &switchExecutor{running: map[string]bool{previous.ID: true, target.ID: false}}
	runner := New(s, executor, recipes)
	job, _, err := s.CreateJob(ctx, "install", target.ID, "install-and-switch", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	waitJob(t, s, job.ID, "ready")
	assertActiveModel(t, s, target.ID, previous.ID, "stopped")
	executor.mu.Lock()
	events := append([]string(nil), executor.events...)
	executor.mu.Unlock()
	downloadIndex, stopIndex, startIndex := -1, -1, -1
	for index, event := range events {
		switch event {
		case "download_artifact:" + target.ID:
			downloadIndex = index
		case "stop_container:" + previous.ID:
			stopIndex = index
		case "start_container:" + target.ID:
			startIndex = index
		}
	}
	if downloadIndex < 0 || stopIndex <= downloadIndex || startIndex <= stopIndex {
		t.Fatalf("download/switch order is unsafe: %#v", events)
	}
}

func TestSwitchReportsWhenRollbackAlsoFails(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	previous, target := recipes[0], recipes[1]
	for _, model := range []store.InstalledModel{
		{RecipeID: previous.ID, RecipeVersion: previous.Version, Status: "ready", ArtifactPath: "/managed/" + previous.ID, Active: true},
		{RecipeID: target.ID, RecipeVersion: target.Version, Status: "stopped", ArtifactPath: "/managed/" + target.ID, ContainerID: "existing-container"},
	} {
		if err := s.SetInstalled(ctx, model); err != nil {
			t.Fatal(err)
		}
	}
	executor := &switchExecutor{
		running:      map[string]bool{previous.ID: true, target.ID: false},
		failVerifyID: target.ID,
		failStartID:  previous.ID,
	}
	runner := New(s, executor, recipes)
	job, _, err := s.CreateJob(ctx, "start", target.ID, "switch-double-failure", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if !strings.Contains(failed.Error, "rollback to "+previous.ID+" failed: rollback start failed") {
		t.Fatalf("double failure outcome missing: %q", failed.Error)
	}
	for _, id := range []string{previous.ID, target.ID} {
		model, err := s.Model(ctx, id)
		if err != nil || model.Active || model.Status != "failed" {
			t.Fatalf("model %s should be failed and inactive: %#v err=%v", id, model, err)
		}
	}
}

func TestSwitchMemoryGuardFailsBeforeTargetStartAndRollsBack(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	previous, target := recipes[0], recipes[1]
	for _, model := range []store.InstalledModel{
		{RecipeID: previous.ID, RecipeVersion: previous.Version, Status: "ready", ArtifactPath: "/managed/" + previous.ID, Active: true},
		{RecipeID: target.ID, RecipeVersion: target.Version, Status: "stopped", ArtifactPath: "/managed/" + target.ID, ContainerID: "existing-container"},
	} {
		if err := s.SetInstalled(ctx, model); err != nil {
			t.Fatal(err)
		}
	}
	executor := &switchExecutor{running: map[string]bool{previous.ID: true, target.ID: false}, failMemoryID: target.ID}
	runner := New(s, executor, recipes)
	job, _, err := s.CreateJob(ctx, "start", target.ID, "switch-memory-guard", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if !strings.Contains(failed.Error, "previous model "+previous.ID+" restored and verified") {
		t.Fatalf("rollback outcome missing: %q", failed.Error)
	}
	executor.mu.Lock()
	events := strings.Join(executor.events, ",")
	executor.mu.Unlock()
	if strings.Contains(events, "start_container:"+target.ID) || !strings.Contains(events, "start_container:"+previous.ID) {
		t.Fatalf("target started despite memory guard or rollback missing: %s", events)
	}
	assertActiveModel(t, s, previous.ID, target.ID, "stopped")
}

func TestRestartFinalizesAlreadyVerifiedRollback(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	previous, target := recipes[0], recipes[1]
	for _, model := range []store.InstalledModel{
		{RecipeID: previous.ID, RecipeVersion: previous.Version, Status: "switching", ArtifactPath: "/managed/" + previous.ID, Active: true},
		{RecipeID: target.ID, RecipeVersion: target.Version, Status: "starting", ArtifactPath: "/managed/" + target.ID, ContainerID: "existing-container"},
	} {
		if err := s.SetInstalled(ctx, model); err != nil {
			t.Fatal(err)
		}
	}
	job, _, err := s.CreateJob(ctx, "start", target.ID, "rollback-restart", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BeginStep(ctx, job.ID, 9, "rollback_verify_openai_inference"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteStep(ctx, job.ID, 9, map[string]any{"response_non_empty": true}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateJobState(ctx, job.ID, "interrupted", "target inference verification failed"); err != nil {
		t.Fatal(err)
	}
	executor := &switchExecutor{running: map[string]bool{previous.ID: true, target.ID: false}}
	runner := New(s, executor, recipes)
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if !strings.Contains(failed.Error, "previous model "+previous.ID+" restored and verified") {
		t.Fatalf("rollback recovery outcome missing: %q", failed.Error)
	}
	assertActiveModel(t, s, previous.ID, target.ID, "stopped")
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if len(executor.events) != 0 {
		t.Fatalf("verified rollback was executed again: %#v", executor.events)
	}
}

// gateExecutor blocks one operation until the test releases it; the gate
// deliberately ignores ctx so cancellation timing stays under test control.
type gateExecutor struct {
	switchExecutor
	gateOp      string
	gateRecipe  string
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	captured    map[string]bool
}

func (g *gateExecutor) Execute(ctx context.Context, execution operations.Execution, op recipe.Operation, r recipe.Recipe, progress operations.Progress) (map[string]any, error) {
	if op.Type == "remove_artifact_if_unshared" {
		g.mu.Lock()
		g.captured = execution.SharedArtifacts
		g.mu.Unlock()
	}
	if op.Type == g.gateOp && r.ID == g.gateRecipe {
		g.enteredOnce.Do(func() { close(g.entered) })
		<-g.release
	}
	return g.switchExecutor.Execute(ctx, execution, op, r, progress)
}

func TestStopOfUnrelatedModelIsNotBlockedByLongInstall(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	active, target := recipes[0], recipes[1]
	if err := s.SetInstalled(ctx, store.InstalledModel{RecipeID: active.ID, RecipeVersion: active.Version, Status: "ready", ArtifactPath: "/managed/" + active.ID, Active: true}); err != nil {
		t.Fatal(err)
	}
	executor := &gateExecutor{
		switchExecutor: switchExecutor{running: map[string]bool{active.ID: true, target.ID: false}},
		gateOp:         "download_artifact", gateRecipe: target.ID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	runner := New(s, executor, recipes)
	install, _, err := s.CreateJob(ctx, "install", target.ID, "slow-install", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(install.ID)
	select {
	case <-executor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("install never reached the download gate")
	}
	stop, _, err := s.CreateJob(ctx, "stop", active.ID, "fast-stop", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(stop.ID)
	waitJob(t, s, stop.ID, "stopped")
	close(executor.release)
	waitJob(t, s, install.ID, "ready")
}

func TestCancelDuringSwitchStaysNonTerminalUntilRollbackFinishes(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	previous, target := recipes[0], recipes[1]
	for _, model := range []store.InstalledModel{
		{RecipeID: previous.ID, RecipeVersion: previous.Version, Status: "ready", ArtifactPath: "/managed/" + previous.ID, Active: true},
		{RecipeID: target.ID, RecipeVersion: target.Version, Status: "stopped", ArtifactPath: "/managed/" + target.ID, ContainerID: "existing-container"},
	} {
		if err := s.SetInstalled(ctx, model); err != nil {
			t.Fatal(err)
		}
	}
	executor := &gateExecutor{
		switchExecutor: switchExecutor{running: map[string]bool{previous.ID: true, target.ID: false}},
		gateOp:         "wait_http", gateRecipe: target.ID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	runner := New(s, executor, recipes)
	job, _, err := s.CreateJob(ctx, "start", target.ID, "cancel-switch", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	select {
	case <-executor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("switch never reached the verification gate")
	}
	if err := runner.Cancel(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.State != "cancelling" {
		t.Fatalf("state=%s, want non-terminal cancelling while rollback is outstanding", pending.State)
	}
	close(executor.release)
	cancelled := waitJob(t, s, job.ID, "cancelled")
	if !strings.Contains(cancelled.Error, "previous model "+previous.ID+" restored and verified") {
		t.Fatalf("rollback outcome missing from cancelled job: %q", cancelled.Error)
	}
	assertActiveModel(t, s, previous.ID, target.ID, "stopped")
}

func TestRemovePassesSharedArtifactsFromOtherInstalledModels(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	removed, kept := recipes[0], recipes[1]
	for _, model := range []store.InstalledModel{
		{RecipeID: removed.ID, RecipeVersion: removed.Version, Status: "stopped", ArtifactPath: "/managed/shared-path"},
		{RecipeID: kept.ID, RecipeVersion: kept.Version, Status: "stopped", ArtifactPath: "/managed/shared-path"},
	} {
		if err := s.SetInstalled(ctx, model); err != nil {
			t.Fatal(err)
		}
	}
	executor := &gateExecutor{switchExecutor: switchExecutor{running: map[string]bool{}}}
	runner := New(s, executor, recipes)
	job, _, err := s.CreateJob(ctx, "remove", removed.ID, "remove-shared", RemovePayload{RemoveArtifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	waitJob(t, s, job.ID, "removed")
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if !executor.captured["/managed/shared-path"] {
		t.Fatalf("shared artifact path missing from removal guard: %#v", executor.captured)
	}
	if !executor.captured[operations.ArtifactKey(kept.Artifacts[0])] {
		t.Fatalf("kept model's pinned artifact key missing from removal guard: %#v", executor.captured)
	}
	if executor.captured[operations.ArtifactKey(removed.Artifacts[0])] {
		t.Fatalf("removed model's own artifact must stay deletable: %#v", executor.captured)
	}
}

func assertActiveModel(t *testing.T, s *store.Store, activeID, inactiveID, inactiveStatus string) {
	t.Helper()
	active, err := s.Model(context.Background(), activeID)
	if err != nil || !active.Active || active.Status != "ready" {
		t.Fatalf("active model=%#v err=%v", active, err)
	}
	inactive, err := s.Model(context.Background(), inactiveID)
	if err != nil || inactive.Active || inactive.Status != inactiveStatus {
		t.Fatalf("inactive model=%#v err=%v", inactive, err)
	}
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

// A cancelled install must stop the container it already started; otherwise
// the leftover keeps the host port and the next install fails preflight on
// the manager's own debris.
func TestCancelledInstallStopsItsContainer(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	target := recipes[0]
	executor := &gateExecutor{
		switchExecutor: switchExecutor{running: map[string]bool{}},
		gateOp:         "wait_http", gateRecipe: target.ID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	runner := New(s, executor, recipes)
	job, _, err := s.CreateJob(ctx, "install", target.ID, "cancel-cleanup", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	select {
	case <-executor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("install never reached the health gate")
	}
	if err := runner.Cancel(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	close(executor.release)
	waitJob(t, s, job.ID, "cancelled")

	deadline := time.Now().Add(5 * time.Second)
	for {
		executor.mu.Lock()
		events := append([]string(nil), executor.events...)
		executor.mu.Unlock()
		cleaned := false
		for _, event := range events {
			if event == "stop_container:"+target.ID {
				cleaned = true
			}
		}
		if cleaned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled install never stopped its container; events=%v", events)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
