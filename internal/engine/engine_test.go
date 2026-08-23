package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
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
	id := singleSpark(recipes).ID
	install, created, err := s.CreateJob(ctx, "install", id, "install-once", map[string]bool{"confirmed": true})
	if err != nil || !created {
		t.Fatalf("CreateJob() created=%v err=%v", created, err)
	}
	runner.Start(install.ID)
	ready := waitJob(t, s, install.ID, "ready")
	if len(ready.Steps) != len(singleSpark(recipes).Operations) {
		t.Fatalf("install steps=%d want %d", len(ready.Steps), len(singleSpark(recipes).Operations))
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
	job, _, err := s.CreateJob(ctx, "install", singleSpark(recipes).ID, "download-only", map[string]any{"confirmed": true, "activate": false})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := runner.plan(ctx, job, singleSpark(recipes), operations.Deployment{})
	if err != nil {
		t.Fatal(err)
	}
	plans := planned.plans
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
	id := singleSpark(recipes).ID
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
	id := singleSpark(recipes).ID
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
	target, serving := twoSingleSparks(recipes)
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
	planned, err := runner.plan(ctx, job, target, operations.Deployment{})
	if err != nil {
		t.Fatal(err)
	}
	plans, previous := planned.plans, planned.previous
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
	job, _, _ := s.CreateJob(ctx, "install", singleSpark(recipes).ID, "redact", map[string]any{})
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
	id := singleSpark(recipes).ID
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

// A fleet upgrade restarts the manager while the upgrade's maintenance
// reservation still closes runtime admission, so the restarted manager's one
// recovery attempt lands inside that window and fails on the reservation
// conflict. Hardware proved it three times (2026-08-12 twice, 2026-08-23
// once): the model then stays recovering forever, because nothing retries.
// Recovery must retry a conflict-failed start until admission reopens.
func TestRecoveryRetriesAfterMaintenanceWindow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recipes, _ := recipe.Builtin()
	id := singleSpark(recipes).ID
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
	runner.recoveryRetryDelay = 20 * time.Millisecond
	if err := runner.Reservations().Reconcile(ctx, recipes); err != nil {
		t.Fatal(err)
	}
	maintID := fleet.ReservationID(fleet.ClaimKindUpdate, "test-upgrade")
	if _, _, err := runner.Reservations().Prepare(ctx, fleet.ReservationRequest{
		ReservationID: maintID, DeploymentID: "upgrade:test-upgrade", DriverNodeID: "local",
		RecipeID: "basement-manager", RecipeVersion: 1,
		Claims: fleet.Claims{Version: fleet.ClaimsVersion, Kind: fleet.ClaimKindUpdate, Runtime: true, Ports: []int{}, FabricInterfaces: []string{}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Reservations().Commit(ctx, maintID, fleet.LocalPrepareToken(maintID), []byte(`{"kind":"test"}`)); err != nil {
		t.Fatal(err)
	}
	if err := runner.Reservations().ActivateMaintenance(ctx, maintID); err != nil {
		t.Fatal(err)
	}
	if err := runner.ReconcileActiveModel(ctx); err != nil {
		t.Fatal(err)
	}
	// The first attempt must fail on the closed admission and leave the model
	// recovering, not failed.
	sawConflict := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := s.ListJobs(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range jobs {
			if job.Kind == "start" && job.State == "failed" && strings.Contains(job.Error, store.ErrReservationConflict.Error()) {
				sawConflict = true
			}
		}
		if sawConflict {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawConflict {
		t.Fatal("the recovery start never hit the maintenance conflict this test holds open")
	}
	// Admission reopens, as it does when the fleet upgrade finalizes. The
	// retry must bring the model to ready with no further calls.
	if err := runner.Reservations().Release(ctx, maintID); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		model, err := s.Model(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if model.Status == "ready" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	jobs, _ := s.ListJobs(ctx, 5)
	model, _ := s.Model(ctx, id)
	t.Fatalf("recovery never reached ready after the window closed; model=%q jobs=%+v", model.Status, jobs)
}

func countStartJobs(t *testing.T, s *store.Store, recipeID string) int {
	t.Helper()
	jobs, err := s.ListJobs(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, job := range jobs {
		if job.Kind == "start" && job.RecipeID == recipeID {
			count++
		}
	}
	return count
}

func recoveringModel(t *testing.T, path string, recipes []recipe.Recipe, id string) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
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
	return s
}

// A recovery start that fails for any reason other than a reservation
// conflict must not retry: the failure is real, and repeating it would only
// repeat the damage report.
func TestRecoveryDoesNotRetryNonConflictFailures(t *testing.T) {
	ctx := context.Background()
	recipes, _ := recipe.Builtin()
	id := singleSpark(recipes).ID
	s := recoveringModel(t, filepath.Join(t.TempDir(), "manager.db"), recipes, id)
	defer s.Close()
	runner := New(s, &fakeExecutor{failPull: true}, recipes)
	runner.recoveryRetryDelay = 20 * time.Millisecond
	if err := runner.Reservations().Reconcile(ctx, recipes); err != nil {
		t.Fatal(err)
	}
	if err := runner.ReconcileActiveModel(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := s.ListJobs(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		failed := false
		for _, job := range jobs {
			if job.Kind == "start" && job.State == "failed" {
				if strings.Contains(job.Error, store.ErrReservationConflict.Error()) {
					t.Fatalf("this test needs a non-conflict failure, got: %s", job.Error)
				}
				failed = true
			}
		}
		if failed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if count := countStartJobs(t, s, id); count != 1 {
		t.Fatalf("a non-conflict failure must not retry; start jobs = %d", count)
	}
}

func TestSwitchMakesOnlyVerifiedTargetActive(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	previous, target := twoSingleSparks(recipes)
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
	previous, target := twoSingleSparks(recipes)
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
	previous, target := twoSingleSparks(recipes)
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
	previous, target := twoSingleSparks(recipes)
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
	previous, target := twoSingleSparks(recipes)
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
	previous, target := twoSingleSparks(recipes)
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
	// reservedSeen records the ReservedBytes each step observed, keyed by
	// "operation:recipeID", so a test can inspect what a specific step of a
	// specific job's execution was told about other jobs' disk claims.
	reservedSeen map[string]int64
}

func (g *gateExecutor) Execute(ctx context.Context, execution operations.Execution, op recipe.Operation, r recipe.Recipe, progress operations.Progress) (map[string]any, error) {
	if op.Type == "remove_artifact_if_unshared" {
		g.mu.Lock()
		g.captured = execution.SharedArtifacts
		g.mu.Unlock()
	}
	g.mu.Lock()
	if g.reservedSeen == nil {
		g.reservedSeen = map[string]int64{}
	}
	g.reservedSeen[op.Type+":"+r.ID] = execution.ReservedBytes
	g.mu.Unlock()
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
	active, target := twoSingleSparks(recipes)
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
	previous, target := twoSingleSparks(recipes)
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
	removed, kept := twoSingleSparks(recipes)
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

// dockerAwareExecutor is a switchExecutor whose fake Docker daemon can also
// be asked what it is running, the way the real host executor is asked
// through operations.ManagedContainerLister. The container list is derived
// from the same running map the operations mutate, so Docker reality and
// executor behavior can never disagree inside a test. verifyFailuresLeft
// fails a recipe's inference verification a set number of times and then
// lets it pass, which is what task #48's hardware sequence needs: the
// rolled-back model's verification must fail exactly once AFTER its
// container was restarted, and the retried model must fail once and then
// succeed.
type dockerAwareExecutor struct {
	switchExecutor
	versions           map[string]int
	verifyFailuresLeft map[string]int
}

func (d *dockerAwareExecutor) Execute(ctx context.Context, execution operations.Execution, op recipe.Operation, r recipe.Recipe, progress operations.Progress) (map[string]any, error) {
	receipt, err := d.switchExecutor.Execute(ctx, execution, op, r, progress)
	if err == nil && op.Type == "verify_openai_inference" {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.verifyFailuresLeft[r.ID] > 0 {
			d.verifyFailuresLeft[r.ID]--
			return nil, errors.New("inference verification failed on hardware")
		}
	}
	return receipt, err
}

func (d *dockerAwareExecutor) Completed(ctx context.Context, execution operations.Execution, op recipe.Operation, r recipe.Recipe, receipt json.RawMessage) bool {
	if op.Type == "verify_openai_inference" {
		d.mu.Lock()
		pending := d.verifyFailuresLeft[r.ID] > 0
		d.mu.Unlock()
		if pending {
			return false
		}
	}
	return d.switchExecutor.Completed(ctx, execution, op, r, receipt)
}

func (d *dockerAwareExecutor) ManagedContainers(context.Context) ([]operations.ManagedContainer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	containers := make([]operations.ManagedContainer, 0, len(d.running))
	for id, running := range d.running {
		if !running {
			continue
		}
		containers = append(containers, operations.ManagedContainer{
			Name:     "basement-" + id + "-v" + strconv.Itoa(d.versions[id]),
			Running:  true,
			RecipeID: id,
			Version:  strconv.Itoa(d.versions[id]),
		})
	}
	return containers, nil
}

// TestRetryAfterDesyncedRollbackStopsTheContainerDockerActuallyRuns replays
// task #48 as observed on hardware: model A serves, an install of B fails its
// verification, and the rollback restarts A's container at the Docker level
// but then fails ITS verification — the double-failure path clears the
// store's active-model pointer while A's container keeps running. The retried
// install of B plans from that lying store (no previous model, so no stop
// step), and only switch-time reconciliation against Docker's own container
// list can stop A before B claims the memory beside it.
func TestRetryAfterDesyncedRollbackStopsTheContainerDockerActuallyRuns(t *testing.T) {
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
	previous, target := twoSingleSparks(recipes)
	executor := &dockerAwareExecutor{
		switchExecutor:     switchExecutor{running: map[string]bool{}},
		versions:           map[string]int{previous.ID: previous.Version, target.ID: target.Version},
		verifyFailuresLeft: map[string]int{},
	}
	runner := New(s, executor, recipes)

	installA, _, err := s.CreateJob(ctx, "install", previous.ID, "desync-install-a", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(installA.ID)
	waitJob(t, s, installA.ID, "ready")

	executor.mu.Lock()
	executor.verifyFailuresLeft[target.ID] = 1
	executor.verifyFailuresLeft[previous.ID] = 1
	executor.mu.Unlock()
	installB, _, err := s.CreateJob(ctx, "install", target.ID, "desync-install-b", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(installB.ID)
	failed := waitJob(t, s, installB.ID, "failed")
	if !strings.Contains(failed.Error, "rollback to "+previous.ID+" failed") {
		t.Fatalf("fixture did not reach the double-failure path: %q", failed.Error)
	}
	executor.mu.Lock()
	stillRunning := executor.running[previous.ID]
	mark := len(executor.events)
	executor.mu.Unlock()
	if !stillRunning {
		t.Fatal("fixture broke: the previous model's container should still be running after the failed rollback")
	}
	desynced, err := s.Model(ctx, previous.ID)
	if err != nil || desynced.Active {
		t.Fatalf("fixture broke: the store should no longer name %s active: %#v err=%v", previous.ID, desynced, err)
	}

	retry, _, err := s.CreateJob(ctx, "install", target.ID, "desync-install-b-retry", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(retry.ID)
	finished := waitJob(t, s, retry.ID, "ready")

	executor.mu.Lock()
	events := append([]string(nil), executor.events[mark:]...)
	running := make(map[string]bool, len(executor.running))
	for id, value := range executor.running {
		running[id] = value
	}
	executor.mu.Unlock()
	stop := indexOfEvent(events, "stop_container:"+previous.ID)
	memory := indexOfEvent(events, "verify_memory:"+target.ID)
	start := indexOfEvent(events, "start_container:"+target.ID)
	if stop < 0 || memory < stop || start < stop {
		t.Fatalf("retry did not stop the actually-running container before starting the target: stop=%d memory=%d start=%d events=%v", stop, memory, start, events)
	}
	assertExactlyOneRunningRecipe(t, running, target.ID)
	sawStopStep := false
	for _, step := range finished.Steps {
		if step.Operation == "stop_container" && step.State == "completed" {
			sawStopStep = true
		}
	}
	if !sawStopStep {
		t.Fatalf("the reconciling stop must be a recorded job step, not an out-of-band kill: steps=%#v", finished.Steps)
	}
	model, err := s.Model(ctx, target.ID)
	if err != nil || !model.Active || model.Status != "ready" {
		t.Fatalf("retried model=%#v err=%v", model, err)
	}
}

// TestSwitchWithMatchingStoreAndDockerPlansNoExtraStops proves the
// reconciliation is a strict no-op when the store already tells the truth: a
// switch away from the genuinely active model still stops it exactly once,
// through the plan it always had.
func TestSwitchWithMatchingStoreAndDockerPlansNoExtraStops(t *testing.T) {
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
	previous, target := twoSingleSparks(recipes)
	if err := s.SetInstalled(ctx, store.InstalledModel{RecipeID: previous.ID, RecipeVersion: previous.Version, Status: "ready", ArtifactPath: "/managed/" + previous.ID, ContainerID: "serving-container", Active: true}); err != nil {
		t.Fatal(err)
	}
	executor := &dockerAwareExecutor{
		switchExecutor:     switchExecutor{running: map[string]bool{previous.ID: true}},
		versions:           map[string]int{previous.ID: previous.Version, target.ID: target.Version},
		verifyFailuresLeft: map[string]int{},
	}
	runner := New(s, executor, recipes)
	job, _, err := s.CreateJob(ctx, "install", target.ID, "matched-switch", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	finished := waitJob(t, s, job.ID, "ready")
	stopSteps := 0
	for _, step := range finished.Steps {
		if step.Operation == "stop_container" {
			stopSteps++
		}
	}
	if stopSteps != 1 {
		t.Fatalf("a matched store and Docker view must plan exactly one stop, got %d: steps=%#v", stopSteps, finished.Steps)
	}
	executor.mu.Lock()
	events := append([]string(nil), executor.events...)
	executor.mu.Unlock()
	stops := 0
	for _, event := range events {
		if strings.HasPrefix(event, "stop_container:") {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("exactly one container stop expected, got %d: %v", stops, events)
	}
	assertActiveModel(t, s, target.ID, previous.ID, "stopped")
}

// TestReinstallingTheServingVersionNeverStopsItsOwnContainer guards the
// legitimate reuse path: reinstalling the exact running version keeps its
// container (the Create 409 and Start 304 paths on real hardware), so the
// Docker reconciliation must not read that container as an orphan to stop.
func TestReinstallingTheServingVersionNeverStopsItsOwnContainer(t *testing.T) {
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
	target := singleSpark(recipes)
	if err := s.SetInstalled(ctx, store.InstalledModel{RecipeID: target.ID, RecipeVersion: target.Version, Status: "ready", ArtifactPath: "/managed/" + target.ID, ContainerID: "serving-container", Active: true}); err != nil {
		t.Fatal(err)
	}
	executor := &dockerAwareExecutor{
		switchExecutor:     switchExecutor{running: map[string]bool{target.ID: true}},
		versions:           map[string]int{target.ID: target.Version},
		verifyFailuresLeft: map[string]int{},
	}
	runner := New(s, executor, recipes)
	job, _, err := s.CreateJob(ctx, "install", target.ID, "reinstall-serving", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	waitJob(t, s, job.ID, "ready")
	executor.mu.Lock()
	events := append([]string(nil), executor.events...)
	stillRunning := executor.running[target.ID]
	executor.mu.Unlock()
	for _, event := range events {
		if strings.HasPrefix(event, "stop_container:") {
			t.Fatalf("reinstalling the serving version must not stop anything: %v", events)
		}
	}
	if !stillRunning {
		t.Fatal("the serving container should have kept running through its own reinstall")
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
	target := singleSpark(recipes)
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

// TestConcurrentInstallsOfDifferentRecipesProceedInParallel proves spec 02's
// core claim: a slow install of one recipe never blocks an install of a
// different recipe. Only steps that touch the shared runtime (start_container
// and friends) serialize; downloads do not.
func TestConcurrentInstallsOfDifferentRecipesProceedInParallel(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	blocked, other := twoSingleSparks(recipes)
	executor := &gateExecutor{
		switchExecutor: switchExecutor{running: map[string]bool{}},
		gateOp:         "download_artifact", gateRecipe: blocked.ID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	runner := New(s, executor, recipes)
	first, _, err := s.CreateJob(ctx, "install", blocked.ID, "first-install", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(first.ID)
	select {
	case <-executor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first install never reached the download gate")
	}
	second, _, err := s.CreateJob(ctx, "install", other.ID, "second-install", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(second.ID)
	waitJob(t, s, second.ID, "ready")
	pending, err := s.GetJob(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal(pending.State) {
		t.Fatalf("first install finished before its download gate released; test does not prove parallelism, state=%s", pending.State)
	}
	close(executor.release)
	waitJob(t, s, first.ID, "ready")
}

func TestConcurrentInstallsReplanTheLateActivatorAsASwitch(t *testing.T) {
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
	blocked, other := twoSingleSparks(recipes)
	if blocked.Service.DefaultHostPort != other.Service.DefaultHostPort {
		t.Fatalf("test recipes must share a host port: %d and %d", blocked.Service.DefaultHostPort, other.Service.DefaultHostPort)
	}
	executor := &gateExecutor{
		switchExecutor: switchExecutor{running: map[string]bool{}},
		gateOp:         "download_artifact", gateRecipe: blocked.ID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	runner := New(s, executor, recipes)
	first, _, err := s.CreateJob(ctx, "install", blocked.ID, "late-activator-first", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(first.ID)
	select {
	case <-executor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first install never reached the download gate")
	}
	second, _, err := s.CreateJob(ctx, "install", other.ID, "late-activator-second", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(second.ID)
	waitJob(t, s, second.ID, "ready")
	close(executor.release)
	finished := waitJob(t, s, first.ID, "ready")

	executor.mu.Lock()
	events := append([]string(nil), executor.events...)
	running := make(map[string]bool, len(executor.running))
	for id, value := range executor.running {
		running[id] = value
	}
	executor.mu.Unlock()
	stop := indexOfEvent(events, "stop_container:"+other.ID)
	start := indexOfEvent(events, "start_container:"+blocked.ID)
	if stop < 0 || start < 0 || stop > start {
		t.Fatalf("the late activator did not switch away from the model that finished first: %v", events)
	}
	assertExactlyOneRunningRecipe(t, running, blocked.ID)
	assertActiveModel(t, s, blocked.ID, other.ID, "stopped")

	var portReceipt map[string]any
	for _, step := range finished.Steps {
		if step.Operation == "verify_port" {
			if err := json.Unmarshal(step.Receipt, &portReceipt); err != nil {
				t.Fatalf("decode verify_port receipt: %v", err)
			}
		}
	}
	if portReceipt["occupied_by_managed_recipe"] != other.ID || portReceipt["available_after_switch"] != true {
		t.Fatalf("verify_port receipt does not name the model actually switched away from: %#v", portReceipt)
	}
}

func TestConcurrentInstallsOnDifferentHostPortsStillSwitch(t *testing.T) {
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
	blocked, other := twoSingleSparks(recipes)
	other.Service.DefaultHostPort = blocked.Service.DefaultHostPort + 1
	for index := range recipes {
		if recipes[index].ID == other.ID {
			recipes[index] = other
		}
	}
	if blocked.Service.DefaultHostPort == other.Service.DefaultHostPort {
		t.Fatal("test recipes unexpectedly share a host port")
	}
	executor := &gateExecutor{
		switchExecutor: switchExecutor{running: map[string]bool{}},
		gateOp:         "download_artifact", gateRecipe: blocked.ID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	runner := New(s, executor, recipes)
	first, _, err := s.CreateJob(ctx, "install", blocked.ID, "different-port-first", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(first.ID)
	select {
	case <-executor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first install never reached the download gate")
	}
	second, _, err := s.CreateJob(ctx, "install", other.ID, "different-port-second", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(second.ID)
	waitJob(t, s, second.ID, "ready")
	close(executor.release)
	waitJob(t, s, first.ID, "ready")

	executor.mu.Lock()
	events := append([]string(nil), executor.events...)
	running := make(map[string]bool, len(executor.running))
	for id, value := range executor.running {
		running[id] = value
	}
	executor.mu.Unlock()
	stop := indexOfEvent(events, "stop_container:"+other.ID)
	start := indexOfEvent(events, "start_container:"+blocked.ID)
	if stop < 0 || start < 0 || stop > start {
		t.Fatalf("different ports allowed two models to start alongside each other: %v", events)
	}
	assertExactlyOneRunningRecipe(t, running, blocked.ID)
	assertActiveModel(t, s, blocked.ID, other.ID, "stopped")
}

// TestSecondInstallVerifyDiskSeesFirstJobsReservationExcludingItsOwn covers
// both disk-reservation acceptance criteria at once: the sum a second job's
// verify_disk step is handed equals exactly the first job's reservation (not
// zero, and not inflated by the second job's own bytes).
func TestSecondInstallVerifyDiskSeesFirstJobsReservationExcludingItsOwn(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	blocked, other := twoSingleSparks(recipes)
	executor := &gateExecutor{
		switchExecutor: switchExecutor{running: map[string]bool{}},
		gateOp:         "verify_disk", gateRecipe: blocked.ID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	runner := New(s, executor, recipes)
	first, _, err := s.CreateJob(ctx, "install", blocked.ID, "first-install", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(first.ID)
	select {
	case <-executor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first install never reached its own verify_disk step")
	}
	// The first job's own verify_disk saw nothing reserved: at this point it
	// is the only running job, so it must never count its own bytes.
	executor.mu.Lock()
	firstReserved := executor.reservedSeen["verify_disk:"+blocked.ID]
	executor.mu.Unlock()
	if firstReserved != 0 {
		t.Fatalf("a job's own reservation must not count against itself: got %d", firstReserved)
	}
	second, _, err := s.CreateJob(ctx, "install", other.ID, "second-install", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(second.ID)
	waitJob(t, s, second.ID, "ready")
	executor.mu.Lock()
	secondReserved, sawIt := executor.reservedSeen["verify_disk:"+other.ID]
	executor.mu.Unlock()
	if !sawIt {
		t.Fatal("second job's verify_disk step was never observed")
	}
	if secondReserved != blocked.RequiredBytes() {
		t.Fatalf("second job's verify_disk should see exactly the first job's reservation: got %d want %d", secondReserved, blocked.RequiredBytes())
	}
	close(executor.release)
	waitJob(t, s, first.ID, "ready")
}

// TestReservationReleasedOnCompletionFailureAndCancellation proves the
// reservation registry never leaks: a leaked entry would understate free
// disk for every install after it, forever.
func TestReservationReleasedOnCompletionFailureAndCancellation(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()

	completed := New(s, &fakeExecutor{}, recipes)
	completeJob, _, err := s.CreateJob(ctx, "install", singleSpark(recipes).ID, "reserve-complete", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	completed.Start(completeJob.ID)
	waitJob(t, s, completeJob.ID, "ready")
	active, err := completed.Reservations().Reservation(ctx, fleet.ReservationID(fleet.ClaimKindLocalJob, completeJob.ID))
	if err != nil || active.State != "active" || completed.ReservedDiskBytes() != 0 {
		t.Fatalf("completed serving claim=%+v reserved_disk=%d err=%v", active, completed.ReservedDiskBytes(), err)
	}

	failing := New(s, &fakeExecutor{failPull: true}, recipes)
	failJob, _, err := s.CreateJob(ctx, "install", recipes[1].ID, "reserve-fail", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	failing.Start(failJob.ID)
	waitJob(t, s, failJob.ID, "failed")
	waitReservationReleased(t, failing, failJob.ID)

	executor := &gateExecutor{
		switchExecutor: switchExecutor{running: map[string]bool{}},
		gateOp:         "wait_http", gateRecipe: singleSpark(recipes).ID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	cancelling := New(s, executor, recipes)
	cancelJob, _, err := s.CreateJob(ctx, "install", singleSpark(recipes).ID, "reserve-cancel", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	cancelling.Start(cancelJob.ID)
	select {
	case <-executor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("install never reached the cancellation gate")
	}
	held, err := cancelling.Reservations().Reservation(ctx, fleet.ReservationID(fleet.ClaimKindLocalJob, cancelJob.ID))
	if err != nil || (held.State != "committed" && held.State != "active") {
		t.Fatal("reservation should be held while the install is paused mid-run")
	}
	if err := cancelling.Cancel(ctx, cancelJob.ID); err != nil {
		t.Fatal(err)
	}
	close(executor.release)
	waitJob(t, s, cancelJob.ID, "cancelled")
	waitReservationReleased(t, cancelling, cancelJob.ID)
}

// retryProbeExecutor stands in for a HostExecutor whose download_artifact
// step internally retries several times on transient network errors
// (host.go's retryNetwork), without ever returning to the engine between
// attempts. It records what the engine's reservation registry holds for its
// own job on every simulated attempt.
type retryProbeExecutor struct {
	fakeExecutor
	jobID    string
	runner   *Engine
	mu       sync.Mutex
	observed []int64
}

func (e *retryProbeExecutor) Execute(ctx context.Context, execution operations.Execution, op recipe.Operation, r recipe.Recipe, progress operations.Progress) (map[string]any, error) {
	if op.Type == "download_artifact" {
		for attempt := 0; attempt < 4; attempt++ {
			reservation, err := e.runner.Reservations().Reservation(ctx, fleet.ReservationID(fleet.ClaimKindLocalJob, e.jobID))
			reserved := int64(-1)
			if err == nil {
				reserved = reservation.Claims.DiskBytes
			}
			e.mu.Lock()
			e.observed = append(e.observed, reserved)
			e.mu.Unlock()
		}
	}
	return e.fakeExecutor.Execute(ctx, execution, op, r, progress)
}

// TestRetriesWithinAStepNeverDoubleTheReservation proves the reservation an
// install holds is set once at job start and never re-added, so a network
// blip that makes retryNetwork retry pull_image or download_artifact several
// times cannot inflate the job's claim on disk.
func TestRetriesWithinAStepNeverDoubleTheReservation(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recipes, _ := recipe.Builtin()
	r := singleSpark(recipes)
	job, _, err := s.CreateJob(ctx, "install", r.ID, "retry-sim", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	executor := &retryProbeExecutor{jobID: job.ID}
	runner := New(s, executor, recipes)
	executor.runner = runner
	runner.Start(job.ID)
	waitJob(t, s, job.ID, "ready")
	executor.mu.Lock()
	observed := append([]int64(nil), executor.observed...)
	executor.mu.Unlock()
	if len(observed) == 0 {
		t.Fatal("download_artifact retry probe never ran")
	}
	for i, value := range observed {
		if value != r.RequiredBytes() {
			t.Fatalf("reservation observed on simulated retry attempt %d was %d, want the single fixed value %d: retries must not double-reserve", i, value, r.RequiredBytes())
		}
	}
}

func TestInstallReservationSurvivesRestartAndReleasesDiskAfterResume(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recipes, _ := recipe.Builtin()
	selected := singleSpark(recipes)
	job, _, err := database.CreateJob(ctx, "install", selected.ID, "restart-reservation", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	allocator := fleet.NewAllocator(database, "local")
	reservationID := fleet.ReservationID(fleet.ClaimKindLocalJob, job.ID)
	fingerprint, err := fleet.RecipeFingerprint(selected)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := allocator.Prepare(ctx, fleet.ReservationRequest{
		ReservationID: reservationID, DeploymentID: "job:" + job.ID, DriverNodeID: "local",
		RecipeID: selected.ID, RecipeVersion: selected.Version, RecipeFingerprint: fingerprint,
		Claims: fleet.Claims{Version: fleet.ClaimsVersion, Kind: fleet.ClaimKindLocalJob, JobID: job.ID,
			DiskBytes: selected.RequiredBytes(), MemoryBytes: fleet.RecipeMemoryClaim(selected), Runtime: true,
			Ports: []int{selected.Service.DefaultHostPort}, FabricInterfaces: []string{}},
		PrepareToken: fleet.LocalPrepareToken(reservationID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.Commit(ctx, reservationID, fleet.LocalPrepareToken(reservationID), []byte(`{"kind":"local-engine"}`)); err != nil {
		t.Fatal(err)
	}
	if err := allocator.AttachJob(ctx, reservationID, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := allocator.Renew(ctx, reservationID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runner := New(database, &fakeExecutor{}, recipes)
	if err := runner.ReconcileReservations(ctx); err != nil {
		t.Fatal(err)
	}
	if reserved := runner.ReservedDiskBytes(); reserved != selected.RequiredBytes() {
		t.Fatalf("restart reserved disk=%d want=%d", reserved, selected.RequiredBytes())
	}
	if err := runner.ResumeInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	waitJob(t, database, job.ID, "ready")
	if reserved := runner.ReservedDiskBytes(); reserved != 0 {
		t.Fatalf("resumed job leaked %d reserved disk bytes", reserved)
	}
	reservation, err := runner.Reservations().Reservation(ctx, reservationID)
	if err != nil || reservation.State != "active" {
		t.Fatalf("resumed serving reservation=%+v err=%v", reservation, err)
	}
}

func TestInterruptedLocalJobThatCannotResumeReleasesItsReservation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	selected := singleSpark(recipes)
	job, _, err := database.CreateJob(ctx, "install", selected.ID, "missing-recipe-after-restart", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner := New(database, &fakeExecutor{}, recipes)
	if err := runner.PrepareJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	reservationID := fleet.ReservationID(fleet.ClaimKindLocalJob, job.ID)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runner = New(database, &fakeExecutor{}, nil)
	if err := runner.ReconcileReservations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.ResumeInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	waitJob(t, database, job.ID, "failed")
	reservation, err := runner.Reservations().Reservation(ctx, reservationID)
	if err != nil || reservation.State != "released" {
		t.Fatalf("unresumable local reservation=%+v err=%v", reservation, err)
	}
}

func TestPrepareJobPersistsReservationBeforeStartingAnyOperation(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	recipes, _ := recipe.Builtin()
	selected := singleSpark(recipes)
	job, _, err := database.CreateJob(ctx, "install", selected.ID, "prepare-before-ack", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner := New(database, &fakeExecutor{}, recipes)
	if err := runner.PrepareJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	reservation, err := runner.Reservations().Reservation(ctx, fleet.ReservationID(fleet.ClaimKindLocalJob, job.ID))
	if err != nil || reservation.State != "committed" || reservation.JobID != job.ID || reservation.Claims.DiskBytes != selected.RequiredBytes() || reservation.Claims.MemoryBytes != fleet.RecipeMemoryClaim(selected) {
		t.Fatalf("prepared reservation=%+v err=%v", reservation, err)
	}
	stored, err := database.GetJob(ctx, job.ID)
	if err != nil || len(stored.Steps) != 0 || stored.State != "queued" {
		t.Fatalf("prepare executed recipe work: job=%+v err=%v", stored, err)
	}
}

func waitReservationReleased(t *testing.T, runner *Engine, jobID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reservation, err := runner.Reservations().Reservation(context.Background(), fleet.ReservationID(fleet.ClaimKindLocalJob, jobID))
		if err == nil && reservation.State == "released" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("reservation for job %s was never released", jobID)
}

func indexOfEvent(events []string, want string) int {
	for index, event := range events {
		if event == want {
			return index
		}
	}
	return -1
}

func assertExactlyOneRunningRecipe(t *testing.T, running map[string]bool, want string) {
	t.Helper()
	count := 0
	for _, value := range running {
		if value {
			count++
		}
	}
	if count != 1 || !running[want] {
		t.Fatalf("running recipes=%v, want only %s", running, want)
	}
}

// singleSpark returns the first shipped single-Spark recipe. The pack now
// also carries distributed recipes, and these fixtures run one node.
func singleSpark(recipes []recipe.Recipe) recipe.Recipe {
	for _, r := range recipes {
		if !r.Distributed() {
			return r
		}
	}
	return recipe.Recipe{}
}

// twoSingleSparks returns two distinct shipped single-Spark recipes for
// switch fixtures.
func twoSingleSparks(recipes []recipe.Recipe) (recipe.Recipe, recipe.Recipe) {
	var picked []recipe.Recipe
	for _, r := range recipes {
		if !r.Distributed() {
			picked = append(picked, r)
			if len(picked) == 2 {
				return picked[0], picked[1]
			}
		}
	}
	return recipe.Recipe{}, recipe.Recipe{}
}
