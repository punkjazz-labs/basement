package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// AdoptIndependentJob mirrors the real runtime in internal/httpapi: it
// refuses a model that is not installed or that sits at another version, and
// it never touches the serving container. The carrier job is terminal at
// once, so nothing here starts, stops, or restarts anything.
func (runtime *placementRuntime) AdoptIndependentJob(ctx context.Context, selected recipe.Recipe, deploymentID, key string) (store.Job, bool, error) {
	model, err := runtime.database.Model(ctx, selected.ID)
	if err != nil {
		return store.Job{}, false, errors.New("the model is not installed on that node")
	}
	if model.RecipeVersion != selected.Version {
		return store.Job{}, false, errors.New("the installed model is a different version, so update it before you adopt it")
	}
	job, created, err := runtime.database.CreateJob(ctx, "adopt", selected.ID, key, map[string]any{"deployment_id": deploymentID})
	if err != nil {
		return store.Job{}, false, err
	}
	if created {
		if err := runtime.database.UpdateJobState(ctx, job.ID, "ready", ""); err != nil {
			return store.Job{}, false, err
		}
	}
	job, err = runtime.database.GetJob(ctx, job.ID)
	return job, created, err
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

func TestAdoptIndependentDeploymentRecordsAnInstalledModel(t *testing.T) {
	ctx := context.Background()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	independent := independentRecipes(t, recipes, 5)
	serving, stale, absent := independent[0], independent[1], independent[2]
	duplicated, repairable := independent[3], independent[4]
	grouped := groupedRecipe(t, recipes)
	controller, controllerStore := newPlacementManager(t, "controller", "192.168.99.30", recipes)
	member, memberStore := newPlacementManager(t, "node-a", "192.168.99.31", recipes)
	member.SetIndependentRuntime(&placementRuntime{database: memberStore, allocator: member.Allocator()})
	controller.newClient = inMemoryFleetClients(t, controller, map[string]*Manager{member.identity.CertificateFingerprint: member})
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Adopt(ctx, AdoptRequest{DisplayName: member.displayName, ConsoleURL: member.consoleURL, NodeURL: member.nodeURL, JoinCode: code.Code}); err != nil {
		t.Fatal(err)
	}
	// These models were installed before fleet placement existed, so none has
	// a deployment record and none owns a job anywhere. The heartbeat below
	// is how the controller learns which version each one runs.
	for _, model := range []store.InstalledModel{
		{RecipeID: serving.ID, RecipeVersion: serving.Version, Status: "ready", Active: true},
		{RecipeID: stale.ID, RecipeVersion: stale.Version, Status: "ready"},
		{RecipeID: duplicated.ID, RecipeVersion: duplicated.Version, Status: "ready"},
		{RecipeID: repairable.ID, RecipeVersion: repairable.Version, Status: "ready"},
	} {
		if err := memberStore.SetInstalled(ctx, model); err != nil {
			t.Fatal(err)
		}
	}
	if err := controller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// The node moves one model to another version after that heartbeat. The
	// controller's reading is now out of date, and the node's own row must
	// still be the authority that refuses it.
	if err := memberStore.SetInstalled(ctx, store.InstalledModel{RecipeID: stale.ID, RecipeVersion: stale.Version + 1, Status: "ready"}); err != nil {
		t.Fatal(err)
	}

	adopted, created, err := controller.AdoptIndependentDeployment(ctx, member.identity.NodeID, serving.ID, "adopt-serving")
	if err != nil || !created {
		t.Fatalf("first adoption created=%v err=%v", created, err)
	}
	if adopted.Job == nil || adopted.Job.Kind != "adopt" || adopted.Job.State != "ready" {
		t.Fatalf("the carrier job is not a terminal adopt job: %+v", adopted.Job)
	}
	if adopted.OwnerNodeID != member.identity.NodeID || adopted.RecipeID != serving.ID || adopted.OwnerJobID != adopted.Job.ID {
		t.Fatalf("the adopted record does not name the model on its node: %+v", adopted)
	}
	if independentDeploymentIDOf(t, *adopted.Job) != adopted.DeploymentID {
		t.Fatalf("the carrier job payload does not carry the deployment id: %s", adopted.Job.Payload)
	}

	// A second adoption with another key must reach the same record and must
	// not create a second job. The deployment id comes from the fleet, the
	// node, and the recipe, never from the key.
	again, created, err := controller.AdoptIndependentDeployment(ctx, member.identity.NodeID, serving.ID, "adopt-serving-again")
	if err != nil || created {
		t.Fatalf("repeat adoption created=%v err=%v", created, err)
	}
	if again.DeploymentID != adopted.DeploymentID {
		t.Fatalf("repeat adoption ids %s and %s differ", again.DeploymentID, adopted.DeploymentID)
	}
	memberJobs, err := memberStore.ListJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(memberJobs) != 1 || memberJobs[0].Kind != "adopt" {
		t.Fatalf("adoption did not leave exactly one carrier job: %+v", memberJobs)
	}
	controllerJobs, err := controllerStore.ListJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(controllerJobs) != 0 {
		t.Fatalf("the controller created jobs of its own: %+v", controllerJobs)
	}
	// Nothing started, stopped, or restarted: the model still serves exactly
	// as it did before the record appeared.
	assertServingRecipe(t, memberStore, serving.ID)

	// The create path already owns this pair under an id of its own. One
	// model on one node must stay one record, whatever id it carries.
	if _, _, err := controllerStore.CreateFleetDeployment(ctx, store.FleetDeployment{
		DeploymentID: "deployment_from_the_create_path", RecipeID: duplicated.ID, RecipeVersion: duplicated.Version,
		RecipeFingerprint: "fingerprint", TopologyCount: 1, OwnerNodeID: member.identity.NodeID, State: "committing",
	}, store.FleetDeploymentNode{NodeID: member.identity.NodeID, ReservationID: "reservation_from_the_create_path"}); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		nodeID   string
		recipeID string
		key      string
		want     string
	}{
		{name: "a two node recipe", nodeID: member.identity.NodeID, recipeID: grouped.ID, key: "adopt-grouped", want: "cannot use independent placement"},
		{name: "a model the node does not report", nodeID: member.identity.NodeID, recipeID: absent.ID, key: "adopt-absent", want: "does not report that model as installed"},
		{name: "a model the node moved to another version", nodeID: member.identity.NodeID, recipeID: stale.ID, key: "adopt-stale", want: "the installed model is a different version"},
		{name: "a node outside the fleet", nodeID: "node_missing", recipeID: serving.ID, key: "adopt-missing", want: "is not in this fleet"},
		{name: "a model outside the catalogue", nodeID: member.identity.NodeID, recipeID: "no-such-model", key: "adopt-unknown", want: "not in this controller's current catalogue"},
		{name: "no idempotency key", nodeID: member.identity.NodeID, recipeID: serving.ID, key: "", want: "a valid idempotency key is required"},
		{name: "a pair another record already owns", nodeID: member.identity.NodeID, recipeID: duplicated.ID, key: "adopt-duplicated", want: "already has a deployment record for that model"},
	} {
		_, created, err := controller.AdoptIndependentDeployment(ctx, test.nodeID, test.recipeID, test.key)
		if err == nil || created {
			t.Fatalf("%s: adoption was accepted", test.name)
		}
		if !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: error %q does not name %q", test.name, err.Error(), test.want)
		}
	}
	deployments, err := controller.Deployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 2 {
		t.Fatalf("a refused adoption left a record behind: %+v", deployments)
	}

	// An earlier attempt that wrote the record and then died before it could
	// name an owner job must not wedge this model on this node. The next
	// adoption finishes it.
	config, err := controllerStore.FleetConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	halfWritten := stablePlacementID("deployment_", config.FleetID, adoptPlacementKey(member.identity.NodeID, repairable.ID))
	fingerprint, err := RecipeFingerprint(repairable)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controllerStore.CreateFleetDeployment(ctx, store.FleetDeployment{
		DeploymentID: halfWritten, RecipeID: repairable.ID, RecipeVersion: repairable.Version,
		RecipeFingerprint: fingerprint, TopologyCount: 1, OwnerNodeID: member.identity.NodeID, State: "adopting",
	}, store.FleetDeploymentNode{NodeID: member.identity.NodeID}); err != nil {
		t.Fatal(err)
	}
	repaired, _, err := controller.AdoptIndependentDeployment(ctx, member.identity.NodeID, repairable.ID, "adopt-repairable")
	if err != nil {
		t.Fatalf("a half-written record wedged its model: %v", err)
	}
	if repaired.DeploymentID != halfWritten || repaired.OwnerJobID == "" || repaired.Job == nil {
		t.Fatalf("the half-written record did not gain an owner job: %+v", repaired)
	}

	// Only this node's pinned controller may adopt on it, and only for this
	// node. The controller certificate is the control: it reaches the
	// handler, so the refusals below are attributable to what they name.
	stranger, _ := newPlacementManager(t, "stranger", "192.168.99.32", recipes)
	callAdopt := func(caller *Manager, request adoptDeploymentRequest) *httptest.ResponseRecorder {
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		httpRequest := httptest.NewRequest(http.MethodPost, "https://member.test/internal/fleet/v1/deployments/adopt", bytes.NewReader(body))
		httpRequest.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{caller.identity.Certificate}}
		response := httptest.NewRecorder()
		member.Handler().ServeHTTP(response, httpRequest)
		return response
	}
	valid := adoptDeploymentRequest{
		NodeID: member.identity.NodeID, RecipeID: serving.ID, RecipeVersion: serving.Version,
		DeploymentID: adopted.DeploymentID, IdempotencyKey: "adopt-serving",
	}
	if control := callAdopt(controller, valid); control.Code != http.StatusOK {
		t.Fatalf("control adoption refused: status=%d body=%s", control.Code, control.Body.String())
	}
	if response := callAdopt(stranger, valid); response.Code != http.StatusForbidden {
		t.Fatalf("a node that is not the controller adopted: status=%d body=%s", response.Code, response.Body.String())
	}
	elsewhere := valid
	elsewhere.NodeID = controller.identity.NodeID
	if response := callAdopt(controller, elsewhere); response.Code != http.StatusConflict {
		t.Fatalf("a request aimed at another node was answered here: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdoptIndependentDeploymentRecordsTheVersionTheNodeRuns(t *testing.T) {
	ctx := context.Background()
	builtin, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	installed := independentRecipes(t, builtin, 1)[0]
	// The catalogue moved on while this model kept serving. History holds
	// both versions; the effective catalogue holds only the newer one.
	newer := installed
	newer.Version = installed.Version + 1
	history := []recipe.Recipe{installed, newer}
	effective := []recipe.Recipe{newer}
	controller, _ := newCatalogueManager(t, "controller", "192.168.99.40", history, effective)
	member, memberStore := newCatalogueManager(t, "node-a", "192.168.99.41", history, effective)
	member.SetIndependentRuntime(&placementRuntime{database: memberStore, allocator: member.Allocator()})
	controller.newClient = inMemoryFleetClients(t, controller, map[string]*Manager{member.identity.CertificateFingerprint: member})
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Adopt(ctx, AdoptRequest{DisplayName: member.displayName, ConsoleURL: member.consoleURL, NodeURL: member.nodeURL, JoinCode: code.Code}); err != nil {
		t.Fatal(err)
	}
	if err := memberStore.SetInstalled(ctx, store.InstalledModel{RecipeID: installed.ID, RecipeVersion: installed.Version, Status: "ready", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := controller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	adopted, created, err := controller.AdoptIndependentDeployment(ctx, member.identity.NodeID, installed.ID, "adopt-running-version")
	if err != nil || !created {
		t.Fatalf("adoption of a model behind the catalogue created=%v err=%v", created, err)
	}
	if adopted.RecipeVersion != installed.Version {
		t.Fatalf("adoption recorded version %d, but the node runs %d", adopted.RecipeVersion, installed.Version)
	}
	if adopted.RecipeVersion == newer.Version {
		t.Fatalf("adoption recorded the newest catalogue version %d instead of what runs", newer.Version)
	}
	fingerprint, err := RecipeFingerprint(installed)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.RecipeFingerprint != fingerprint {
		t.Fatalf("the record does not fingerprint the recipe the node actually runs: %+v", adopted)
	}
	model, err := memberStore.Model(ctx, installed.ID)
	if err != nil || model.RecipeVersion != installed.Version || !model.Active {
		t.Fatalf("adoption disturbed the running model: model=%+v err=%v", model, err)
	}
}

func independentDeploymentIDOf(t *testing.T, job store.Job) string {
	t.Helper()
	var payload struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.DeploymentID
}

func independentRecipes(t *testing.T, recipes []recipe.Recipe, count int) []recipe.Recipe {
	t.Helper()
	var selected []recipe.Recipe
	for _, item := range recipes {
		if item.Topology.SparkCount == 1 {
			selected = append(selected, item)
		}
		if len(selected) == count {
			return selected
		}
	}
	t.Fatalf("%d independent recipes are required", count)
	return nil
}

func groupedRecipe(t *testing.T, recipes []recipe.Recipe) recipe.Recipe {
	t.Helper()
	for _, item := range recipes {
		if item.Topology.SparkCount > 1 {
			return item
		}
	}
	t.Fatal("a multi-node recipe is required")
	return recipe.Recipe{}
}

func newPlacementManager(t *testing.T, name, address string, recipes []recipe.Recipe) (*Manager, *store.Store) {
	t.Helper()
	return newCatalogueManager(t, name, address, recipes, recipes)
}

// newCatalogueManager separates the catalogue that retains history from the
// effective catalogue, which holds only the newest version of each model.
// Adoption must read the first and not the second.
func newCatalogueManager(t *testing.T, name, address string, all, effective []recipe.Recipe) (*Manager, *store.Store) {
	t.Helper()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	options := testManagerOptions(directory, database, address)
	options.DisplayName = name
	options.Recipes = all
	options.EffectiveRecipes = effective
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
