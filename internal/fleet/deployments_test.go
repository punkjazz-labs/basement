package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

type placementRuntime struct {
	database  *store.Store
	allocator *Allocator
	mu        sync.Mutex
	preflight []string
}

func (runtime *placementRuntime) PreflightIndependent(_ context.Context, selected recipe.Recipe, _ string) (json.RawMessage, bool, error) {
	runtime.mu.Lock()
	runtime.preflight = append(runtime.preflight, selected.ID)
	runtime.mu.Unlock()
	return json.RawMessage(`{"ready":true,"checks":[{"operation":"verify_port","ok":true}]}`), true, nil
}

func (runtime *placementRuntime) CreateIndependentJob(ctx context.Context, selected recipe.Recipe, intent IndependentIntent, reservationID, deploymentID, key string) (store.Job, bool, error) {
	job, created, err := runtime.database.CreateJob(ctx, "install", selected.ID, key, map[string]any{
		"activate": intent.Activate, "reservation_id": reservationID, "deployment_id": deploymentID,
	})
	if err != nil {
		return store.Job{}, false, err
	}
	if !created {
		job, err = runtime.database.GetJob(ctx, job.ID)
		return job, false, err
	}
	if err := runtime.allocator.AttachJob(ctx, reservationID, job.ID); err != nil {
		return store.Job{}, false, err
	}
	previous := ""
	models, err := runtime.database.Models(ctx)
	if err != nil {
		return store.Job{}, false, err
	}
	for _, model := range models {
		if model.Active {
			previous = model.RecipeID
			break
		}
	}
	if intent.Activate {
		if err := runtime.allocator.Activate(ctx, reservationID, previous); err != nil {
			return store.Job{}, false, err
		}
		if err := runtime.database.ActivateExclusively(ctx, store.InstalledModel{RecipeID: selected.ID, RecipeVersion: selected.Version, Status: "ready", Active: true}); err != nil {
			return store.Job{}, false, err
		}
	} else if err := runtime.database.SetInstalled(ctx, store.InstalledModel{RecipeID: selected.ID, RecipeVersion: selected.Version, Status: "stopped"}); err != nil {
		return store.Job{}, false, err
	}
	if err := runtime.database.UpdateJobState(ctx, job.ID, "ready", ""); err != nil {
		return store.Job{}, false, err
	}
	job, err = runtime.database.GetJob(ctx, job.ID)
	return job, true, err
}

func (runtime *placementRuntime) IndependentJob(ctx context.Context, jobID string) (store.Job, error) {
	return runtime.database.GetJob(ctx, jobID)
}

func (runtime *placementRuntime) IndependentAction(ctx context.Context, owner store.Job, action, key string, _ IndependentIntent) (store.Job, error) {
	var ownerPayload struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal(owner.Payload, &ownerPayload); err != nil || ownerPayload.DeploymentID == "" {
		return store.Job{}, errors.New("fixture owner job has no deployment")
	}
	job, _, err := runtime.database.CreateJob(ctx, action, owner.RecipeID, key, map[string]any{"deployment_id": ownerPayload.DeploymentID})
	if err != nil {
		return store.Job{}, err
	}
	if err := runtime.database.UpdateJobState(ctx, job.ID, "ready", ""); err != nil {
		return store.Job{}, err
	}
	return runtime.database.GetJob(ctx, job.ID)
}

func TestIndependentPlacementsOwnJobsAndServingPerNode(t *testing.T) {
	ctx := context.Background()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	first, second := twoIndependentRecipes(t, recipes)
	controller, controllerStore := newPlacementManager(t, "controller", "192.168.99.10", recipes)
	memberA, memberAStore := newPlacementManager(t, "node-a", "192.168.99.20", recipes)
	memberB, memberBStore := newPlacementManager(t, "node-b", "192.168.99.21", recipes)
	memberA.SetIndependentRuntime(&placementRuntime{database: memberAStore, allocator: memberA.Allocator()})
	memberB.SetIndependentRuntime(&placementRuntime{database: memberBStore, allocator: memberB.Allocator()})
	targets := map[string]*Manager{memberA.identity.CertificateFingerprint: memberA, memberB.identity.CertificateFingerprint: memberB}
	controller.newClient = inMemoryFleetClients(t, controller, targets)
	for _, target := range []*Manager{memberA, memberB} {
		code, err := target.CreateJoinCode(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := controller.Adopt(ctx, AdoptRequest{DisplayName: target.displayName, ConsoleURL: target.consoleURL, NodeURL: target.nodeURL, JoinCode: code.Code}); err != nil {
			t.Fatal(err)
		}
	}
	if err := controller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := memberA.PlanIndependent(ctx, first.ID); err == nil {
		t.Fatal("a fleet member issued a placement plan")
	}
	intent := IndependentIntent{Confirmed: true, AcceptLicence: true, ConfirmTerritoryEligibility: true, Activate: true}
	deploymentA, created, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: first.ID, NodeID: memberA.identity.NodeID, IdempotencyKey: "place-first", Intent: intent})
	if err != nil || !created {
		t.Fatalf("first placement created=%v err=%v", created, err)
	}
	deploymentB, created, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: second.ID, NodeID: memberB.identity.NodeID, IdempotencyKey: "place-second", Intent: intent})
	if err != nil || !created {
		t.Fatalf("second placement created=%v err=%v", created, err)
	}
	if deploymentA.Job == nil || deploymentB.Job == nil || deploymentA.Job.ID == deploymentB.Job.ID {
		t.Fatalf("independent jobs were not distinct: A=%+v B=%+v", deploymentA.Job, deploymentB.Job)
	}
	controllerJobs, _ := controllerStore.ListJobs(ctx, 100)
	memberAJobs, _ := memberAStore.ListJobs(ctx, 100)
	memberBJobs, _ := memberBStore.ListJobs(ctx, 100)
	if len(controllerJobs) != 0 || len(memberAJobs) != 1 || len(memberBJobs) != 1 {
		t.Fatalf("job ownership controller=%d nodeA=%d nodeB=%d", len(controllerJobs), len(memberAJobs), len(memberBJobs))
	}
	assertServingRecipe(t, memberAStore, first.ID)
	assertServingRecipe(t, memberBStore, second.ID)
	if first.Service.DefaultHostPort != second.Service.DefaultHostPort {
		t.Fatalf("fixture ports differ: %d and %d", first.Service.DefaultHostPort, second.Service.DefaultHostPort)
	}

	retried, created, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: first.ID, NodeID: memberA.identity.NodeID, IdempotencyKey: "place-first", Intent: intent})
	if err != nil || created || retried.Job == nil || retried.Job.ID != deploymentA.Job.ID {
		t.Fatalf("idempotent retry created=%v job=%+v err=%v", created, retried.Job, err)
	}
	if _, _, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: second.ID, NodeID: memberB.identity.NodeID, IdempotencyKey: "place-first", Intent: intent}); err == nil {
		t.Fatal("an idempotency key accepted different placement details")
	}
	type placementResult struct {
		deployment Deployment
		created    bool
		err        error
	}
	start := make(chan struct{})
	results := make(chan placementResult, 2)
	for range 2 {
		go func() {
			<-start
			deployment, created, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: first.ID, NodeID: memberA.identity.NodeID, IdempotencyKey: "concurrent-retry", Intent: intent})
			results <- placementResult{deployment: deployment, created: created, err: err}
		}()
	}
	close(start)
	createdCount, concurrentJobID := 0, ""
	for range 2 {
		result := <-results
		if result.err != nil || result.deployment.Job == nil {
			t.Fatalf("concurrent retry deployment=%+v err=%v", result.deployment, result.err)
		}
		if result.created {
			createdCount++
		}
		if concurrentJobID == "" {
			concurrentJobID = result.deployment.Job.ID
		} else if concurrentJobID != result.deployment.Job.ID {
			t.Fatalf("concurrent retries created jobs %s and %s", concurrentJobID, result.deployment.Job.ID)
		}
	}
	if createdCount != 1 {
		t.Fatalf("concurrent retries reported %d creations", createdCount)
	}

	switchDeployment, created, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: second.ID, NodeID: memberA.identity.NodeID, IdempotencyKey: "switch-node-a", Intent: intent})
	if err != nil || !created || switchDeployment.Job == nil {
		t.Fatalf("node-local switch created=%v deployment=%+v err=%v", created, switchDeployment, err)
	}
	assertServingRecipe(t, memberAStore, second.ID)
	assertServingRecipe(t, memberBStore, second.ID)
	modelOnB, err := memberBStore.Model(ctx, second.ID)
	if err != nil || !modelOnB.Active {
		t.Fatalf("switch on node A disturbed node B: model=%+v err=%v", modelOnB, err)
	}
	actionJob, err := controller.ActionDeployment(ctx, deploymentB.DeploymentID, "benchmark", "benchmark-node-b", IndependentIntent{})
	if err != nil {
		t.Fatal(err)
	}
	projected, err := controller.Deployment(ctx, deploymentB.DeploymentID)
	if err != nil || projected.OwnerJobID != actionJob.ID || projected.Job == nil || projected.Job.ID != actionJob.ID {
		t.Fatalf("latest lifecycle projection=%+v action=%+v err=%v", projected, actionJob, err)
	}
}

func newPlacementManager(t *testing.T, name, address string, recipes []recipe.Recipe) (*Manager, *store.Store) {
	t.Helper()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	options := testManagerOptions(directory, database, address)
	options.DisplayName = name
	options.Recipes = recipes
	options.EffectiveRecipes = recipes
	manager, err := NewManager(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	return manager, database
}

func twoIndependentRecipes(t *testing.T, recipes []recipe.Recipe) (recipe.Recipe, recipe.Recipe) {
	t.Helper()
	var selected []recipe.Recipe
	for _, item := range recipes {
		if item.Topology.SparkCount == 1 {
			selected = append(selected, item)
		}
		if len(selected) == 2 {
			return selected[0], selected[1]
		}
	}
	t.Fatal("two independent recipes are required")
	return recipe.Recipe{}, recipe.Recipe{}
}

func assertServingRecipe(t *testing.T, database *store.Store, recipeID string) {
	t.Helper()
	models, err := database.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	active := ""
	for _, model := range models {
		if model.Active {
			if active != "" {
				t.Fatalf("node has two active models: %s and %s", active, model.RecipeID)
			}
			active = model.RecipeID
		}
	}
	if active != recipeID {
		t.Fatalf("serving recipe=%s want=%s", active, recipeID)
	}
}
