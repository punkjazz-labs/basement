package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

// The carrier job exists only to hold a deployment id where
// independentDeploymentID can read it. It must be terminal at once and it
// must never reach the engine, so adopting a model can never disturb the
// container that model already serves from.
func TestAdoptIndependentJobIsTerminalAndRunsNoOperation(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authManager, err := auth.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var selected recipe.Recipe
	for _, item := range recipes {
		if item.Topology.SparkCount == 1 {
			selected = item
			break
		}
	}
	if selected.ID == "" {
		t.Fatal("an independent recipe is required")
	}
	executor := &apiExecutor{done: map[string]bool{}}
	runner := engine.New(database, executor, recipes)
	server := New("test", directory, authManager, database, readyInventory{}, executor, runner, recipes)
	if err := database.SetInstalled(ctx, store.InstalledModel{RecipeID: selected.ID, RecipeVersion: selected.Version, Status: "ready", Active: true}); err != nil {
		t.Fatal(err)
	}

	job, created, err := server.AdoptIndependentJob(ctx, selected, "deployment_under_test", "adopt-key")
	if err != nil || !created {
		t.Fatalf("adoption created=%v err=%v", created, err)
	}
	if job.Kind != "adopt" || job.State != "ready" {
		t.Fatalf("the carrier job is not a terminal adopt job: %+v", job)
	}
	// A job handed to the engine runs in its own goroutine, so give one time
	// to appear before calling the executor untouched.
	time.Sleep(100 * time.Millisecond)
	executor.mu.Lock()
	operations, running := len(executor.done), executor.running
	executor.mu.Unlock()
	if operations != 0 || running {
		t.Fatalf("adoption executed %d operations and left running=%v", operations, running)
	}
	stored, err := database.GetJob(ctx, job.ID)
	if err != nil || stored.State != "ready" {
		t.Fatalf("the carrier job did not stay terminal: job=%+v err=%v", stored, err)
	}

	// An earlier attempt that died before the job reached terminal must be
	// repaired by the next one, not left behind as queued work.
	if err := database.UpdateJobState(ctx, job.ID, "queued", ""); err != nil {
		t.Fatal(err)
	}
	repaired, created, err := server.AdoptIndependentJob(ctx, selected, "deployment_under_test", "adopt-key")
	if err != nil || created || repaired.State != "ready" {
		t.Fatalf("retry created=%v job=%+v err=%v", created, repaired, err)
	}

	// The installed row is the authority: a version it does not hold is
	// refused, whatever the controller believed.
	moved := selected
	moved.Version = selected.Version + 1
	if _, _, err := server.AdoptIndependentJob(ctx, moved, "deployment_under_test", "adopt-moved"); err == nil {
		t.Fatal("adoption accepted a version this node does not run")
	}
	if _, err := database.Model(ctx, selected.ID); err != nil {
		t.Fatalf("adoption disturbed the installed model: %v", err)
	}
}

// The public route must reach adoption past the /api/v1/fleet/deployments/
// prefix that sits beside it, and must report a record it created apart from
// one it found. This also drives the local branch, where the controller
// adopts a model on itself.
func TestFleetDeploymentAdoptRouteCreatesOnceAndRepeats(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authManager, err := auth.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var selected recipe.Recipe
	for _, item := range recipes {
		if item.Topology.SparkCount == 1 {
			selected = item
			break
		}
	}
	executor := &apiExecutor{done: map[string]bool{}}
	runner := engine.New(database, executor, recipes)
	server := New("test", directory, authManager, database, readyInventory{}, executor, runner, recipes)
	manager, err := fleet.NewManager(ctx, fleet.Options{
		DataDir: directory, Database: database, Inventory: readyInventory{}, Version: "test", BuildIdentity: "test-build",
		DisplayName: "node-local", ConsoleURL: "http://192.168.99.10:7070", NodeURL: "https://192.168.99.10:7071",
		Recipes: recipes, EffectiveRecipes: recipes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.SetFleetManager(manager)
	if err := database.SetInstalled(ctx, store.InstalledModel{RecipeID: selected.ID, RecipeVersion: selected.Version, Status: "ready", Active: true}); err != nil {
		t.Fatal(err)
	}
	summary, err := manager.Summary(ctx)
	if err != nil || len(summary.Nodes) != 1 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	cookie, csrf := pairMembershipConsole(t, server, authManager)
	adopt := func(key string) *httptest.ResponseRecorder {
		body := `{"node_id":"` + summary.Nodes[0].NodeID + `","recipe_id":"` + selected.ID + `"}`
		request := httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/fleet/deployments/adopt", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://console.test")
		request.Header.Set("X-CSRF-Token", csrf)
		request.Header.Set("Idempotency-Key", key)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	created := adopt("adopt-one")
	if created.Code != http.StatusCreated {
		t.Fatalf("first adoption status=%d body=%s", created.Code, created.Body.String())
	}
	repeated := adopt("adopt-two")
	if repeated.Code != http.StatusOK {
		t.Fatalf("repeat adoption status=%d body=%s", repeated.Code, repeated.Body.String())
	}
	deployments, err := manager.Deployments(ctx)
	if err != nil || len(deployments) != 1 || deployments[0].OwnerJobID == "" {
		t.Fatalf("adoption did not leave one owned record: %+v err=%v", deployments, err)
	}
	executor.mu.Lock()
	operations := len(executor.done)
	executor.mu.Unlock()
	if operations != 0 {
		t.Fatalf("adoption over the route executed %d operations", operations)
	}
}

func TestPublicAPIKeyCannotPlanOrCreateFleetDeployment(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authManager, err := auth.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	executor := &apiExecutor{done: map[string]bool{}}
	runner := engine.New(database, executor, recipes)
	server := New("test", directory, authManager, database, readyInventory{}, executor, runner, recipes)
	manager, err := fleet.NewManager(ctx, fleet.Options{
		DataDir: directory, Database: database, Inventory: readyInventory{}, Version: "test", BuildIdentity: "test-build",
		DisplayName: "node-local", ConsoleURL: "http://192.168.99.10:7070", NodeURL: "https://192.168.99.10:7071",
		Recipes: recipes, EffectiveRecipes: recipes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.SetFleetManager(manager)
	_, key, err := database.CreateAPIKey(ctx, "public client")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		body string
		want int
	}{
		{path: "/api/v1/fleet/placements/plan", body: `{"recipe_id":"anything"}`, want: http.StatusUnauthorized},
		{path: "/api/v1/fleet/deployments", body: `{"recipe_id":"anything","node_id":"anything","confirmed":true}`, want: http.StatusForbidden},
		{path: "/api/v1/fleet/deployments/adopt", body: `{"recipe_id":"anything","node_id":"anything"}`, want: http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodPost, "http://manager.test"+test.path, bytes.NewBufferString(test.body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+key)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("%s status=%d body=%s", test.path, response.Code, response.Body.String())
		}
	}
}

// The route behind the console's Clear tool. It ends a record this fleet can
// no longer act on, and it is a console mutation like every other one: an API
// key cannot reach it, only the owner's session can.
func TestFleetDeploymentReleaseRouteClearsAStrandedRecord(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authManager, err := auth.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var selected recipe.Recipe
	for _, item := range recipes {
		if item.Topology.SparkCount == 1 {
			selected = item
			break
		}
	}
	executor := &apiExecutor{done: map[string]bool{}}
	runner := engine.New(database, executor, recipes)
	server := New("test", directory, authManager, database, readyInventory{}, executor, runner, recipes)
	manager, err := fleet.NewManager(ctx, fleet.Options{
		DataDir: directory, Database: database, Inventory: readyInventory{}, Version: "test", BuildIdentity: "test-build",
		DisplayName: "node-local", ConsoleURL: "http://192.168.99.10:7070", NodeURL: "https://192.168.99.10:7071",
		Recipes: recipes, EffectiveRecipes: recipes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.SetFleetManager(manager)
	summary, err := manager.Summary(ctx)
	if err != nil || len(summary.Nodes) != 1 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	// A record an earlier placement left behind without an owner job. Nothing
	// runs for it anywhere, and no action can be addressed to it.
	if _, _, err := database.CreateFleetDeployment(ctx, store.FleetDeployment{
		DeploymentID: "deployment_stranded", RecipeID: selected.ID, RecipeVersion: selected.Version,
		RecipeFingerprint: "fingerprint", TopologyCount: 1, OwnerNodeID: summary.Nodes[0].NodeID, State: "committing",
	}, store.FleetDeploymentNode{NodeID: summary.Nodes[0].NodeID, ReservationID: "reservation_stranded"}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := pairMembershipConsole(t, server, authManager)
	releaseID := func(owner bool, deploymentID string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/fleet/deployments/"+deploymentID+"/release", bytes.NewBufferString(`{}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://console.test")
		if owner {
			request.Header.Set("X-CSRF-Token", csrf)
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	release := func(owner bool) *httptest.ResponseRecorder { return releaseID(owner, "deployment_stranded") }
	if refused := release(false); refused.Code != http.StatusForbidden {
		t.Fatalf("a caller with no owner session cleared a record: status=%d body=%s", refused.Code, refused.Body.String())
	}
	// A record that is not there answers as the read of that same record
	// answers, so the console reads one word for one state.
	if missing := releaseID(true, "deployment_that_never_existed"); missing.Code != http.StatusNotFound {
		t.Fatalf("clearing a record that is not there: status=%d body=%s", missing.Code, missing.Body.String())
	}
	cleared := release(true)
	if cleared.Code != http.StatusOK {
		t.Fatalf("clearing status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	// Clearing one twice is not an error, and the second time reports no
	// second clearing.
	repeated := release(true)
	if repeated.Code != http.StatusOK || !strings.Contains(repeated.Body.String(), `"released":false`) {
		t.Fatalf("clearing twice: status=%d body=%s", repeated.Code, repeated.Body.String())
	}
	stored, err := database.FleetDeployment(ctx, "deployment_stranded")
	if err != nil || stored.State != "removed" {
		t.Fatalf("the record reads %+v err=%v", stored, err)
	}
	// Clearing is bookkeeping. No operation ran on this machine for it.
	executor.mu.Lock()
	operations := len(executor.done)
	executor.mu.Unlock()
	if operations != 0 {
		t.Fatalf("clearing a record executed %d operations", operations)
	}
}
