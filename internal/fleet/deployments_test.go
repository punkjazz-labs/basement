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
		return store.Job{}, false, errors.New("the model is not installed on this Spark")
	}
	if model.RecipeVersion != selected.Version {
		return store.Job{}, false, errors.New("the installed model on this Spark is a different version, so update it before you adopt it")
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
	// A third model, so that the concurrent retry below can name a pair no
	// record owns yet: one model on one Spark is one record, and a second
	// placement of first on node A would now be refused before the race the
	// retry is there to test could even start.
	placeable := independentRecipes(t, recipes, 3)
	first, second, third := placeable[0], placeable[1], placeable[2]
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
			deployment, created, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: third.ID, NodeID: memberA.identity.NodeID, IdempotencyKey: "concurrent-retry", Intent: intent})
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

	// One model on one Spark is one LIVE record. Work that is still running
	// holds the pair, because a second install beside it would give the console
	// two rows and two owner jobs for one model.
	liveJob := liveOwnerJob(t, memberBStore, first.ID, "deployment_live_on_node_b")
	writeDeploymentRecord(t, controllerStore, store.FleetDeployment{
		DeploymentID: "deployment_live_on_node_b", RecipeID: first.ID, RecipeVersion: first.Version,
		RecipeFingerprint: "fingerprint", TopologyCount: 1, OwnerNodeID: memberB.identity.NodeID, State: "running",
	}, liveJob)
	_, created, err = controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: first.ID, NodeID: memberB.identity.NodeID, IdempotencyKey: "duplicate-live", Intent: intent})
	if err == nil || created || !strings.Contains(err.Error(), "already has a deployment record for that model") {
		t.Fatalf("a second live record for one pair was accepted: created=%v err=%v", created, err)
	}

	// A record whose work has finished holds nothing. This is the model node A
	// already serves under a record this fleet made, updated from the install
	// dialog: the new placement takes the pair and the settled record reads
	// removed. Refusing here would mean the fleet could never update a model it
	// had placed itself.
	if settled, err := controller.Deployment(ctx, deploymentA.DeploymentID); err != nil || settled.Job == nil || settled.Job.State != "ready" {
		t.Fatalf("the record to supersede is not settled: %+v err=%v", settled, err)
	}
	updated, created, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: first.ID, NodeID: memberA.identity.NodeID, IdempotencyKey: "update-first", Intent: intent})
	if err != nil || !created || updated.DeploymentID == deploymentA.DeploymentID {
		t.Fatalf("a settled record did not give up its pair: created=%v deployment=%+v err=%v", created, updated, err)
	}
	if superseded, err := controllerStore.FleetDeployment(ctx, deploymentA.DeploymentID); err != nil || superseded.State != "removed" {
		t.Fatalf("the superseded record reads %+v err=%v", superseded, err)
	}
	// The poll the console runs must not undo it. The superseded record still
	// names a job node A answers for, so reading that job back into the record
	// would leave one model on one Spark with two live records.
	if _, err := controller.Deployments(ctx); err != nil {
		t.Fatal(err)
	}
	if superseded, err := controllerStore.FleetDeployment(ctx, deploymentA.DeploymentID); err != nil || superseded.State != "removed" {
		t.Fatalf("a poll brought the superseded record back: %+v err=%v", superseded, err)
	}
	// The key that made the superseded record cannot carry on with it either.
	// Handing it back would read as an install that quietly did nothing, and
	// the record is over.
	_, created, err = controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: first.ID, NodeID: memberA.identity.NodeID, IdempotencyKey: "place-first", Intent: intent})
	if err == nil || created || !strings.Contains(err.Error(), "send a new install request") {
		t.Fatalf("a retry of the key that made a cleared record was accepted: created=%v err=%v", created, err)
	}
	if superseded, err := controllerStore.FleetDeployment(ctx, deploymentA.DeploymentID); err != nil || superseded.State != "removed" {
		t.Fatalf("a refused retry changed the cleared record: %+v err=%v", superseded, err)
	}
	// A new request brings a new key, so it mints a new record id and a clean
	// record. That is the way back, and it works.
	fresh, created, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: first.ID, NodeID: memberA.identity.NodeID, IdempotencyKey: "place-first-again", Intent: intent})
	if err != nil || !created || fresh.OwnerJobID == "" {
		t.Fatalf("a new request over a cleared record was refused: created=%v deployment=%+v err=%v", created, fresh, err)
	}

	// A create that died before it could name an owner job holds nothing
	// either. Retrying it used to be refused for good, which wedged that model
	// on that Spark with no way out of it from the console.
	writeDeploymentRecord(t, controllerStore, store.FleetDeployment{
		DeploymentID: "deployment_that_failed", RecipeID: third.ID, RecipeVersion: third.Version,
		RecipeFingerprint: "fingerprint", TopologyCount: 1, OwnerNodeID: memberB.identity.NodeID, State: "failed",
	}, store.Job{})
	afterFailure, created, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: third.ID, NodeID: memberB.identity.NodeID, IdempotencyKey: "retry-after-failure", Intent: intent})
	if err != nil || !created || afterFailure.OwnerJobID == "" {
		t.Fatalf("a retry after a failed create was refused: created=%v deployment=%+v err=%v", created, afterFailure, err)
	}
	if failed, err := controllerStore.FleetDeployment(ctx, "deployment_that_failed"); err != nil || failed.State != "removed" {
		t.Fatalf("the failed record reads %+v err=%v", failed, err)
	}
}

// A record the fleet can no longer act on: the owner Spark lost the job row,
// left the fleet, or never got a job at all. Such a record pins its console row
// to "No answer" with every button dead, and adoption cannot write a fresh one
// while it stands. Clearing it touches this controller's bookkeeping and
// nothing else.
func TestReleaseDeploymentEndsARecordTheFleetCannotReach(t *testing.T) {
	ctx := context.Background()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	held := independentRecipes(t, recipes, 4)
	controller, controllerStore := newPlacementManager(t, "controller", "192.168.99.80", recipes)
	member, memberStore := newPlacementManager(t, "loft", "192.168.99.81", recipes)
	member.SetIndependentRuntime(&placementRuntime{database: memberStore, allocator: member.Allocator()})
	controller.newClient = inMemoryFleetClients(t, controller, map[string]*Manager{member.identity.CertificateFingerprint: member})
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Adopt(ctx, AdoptRequest{DisplayName: member.displayName, ConsoleURL: member.consoleURL, NodeURL: member.nodeURL, JoinCode: code.Code}); err != nil {
		t.Fatal(err)
	}

	record := func(id string, selected recipe.Recipe, nodeID string, job store.Job) {
		writeDeploymentRecord(t, controllerStore, store.FleetDeployment{
			DeploymentID: id, RecipeID: selected.ID, RecipeVersion: selected.Version,
			RecipeFingerprint: "fingerprint", TopologyCount: 1, OwnerNodeID: nodeID, State: "running",
		}, job)
	}
	// Four records, one for each way a placement can end up stranded, and one
	// that is not stranded at all.
	record("deployment_without_a_job", held[0], member.identity.NodeID, store.Job{})
	record("deployment_with_an_unknown_job", held[1], member.identity.NodeID, store.Job{ID: "job_the_node_never_had", State: "running"})
	record("deployment_off_the_fleet", held[2], "node_that_left", store.Job{ID: "job_somewhere_else", State: "running"})
	record("deployment_the_node_answers_for", held[3], member.identity.NodeID,
		liveOwnerJob(t, memberStore, held[3].ID, "deployment_the_node_answers_for"))

	for _, id := range []string{"deployment_without_a_job", "deployment_with_an_unknown_job", "deployment_off_the_fleet"} {
		released, ended, err := controller.ReleaseDeployment(ctx, id)
		if err != nil || !ended || released.State != "removed" {
			t.Fatalf("%s was not cleared: ended=%v record=%+v err=%v", id, ended, released, err)
		}
		// Clearing one twice is not an error and does not report a second
		// clearing, so two clicks read the same as one.
		if _, again, err := controller.ReleaseDeployment(ctx, id); err != nil || again {
			t.Fatalf("%s reported a second clearing: again=%v err=%v", id, again, err)
		}
	}
	// The Spark still answers for this one, so the record is not stranded and
	// the owner is told to remove the model instead of throwing the record away.
	_, ended, err := controller.ReleaseDeployment(ctx, "deployment_the_node_answers_for")
	if err == nil || ended || !strings.Contains(err.Error(), "loft answers for this model") {
		t.Fatalf("a record the fleet can still read was cleared: ended=%v err=%v", ended, err)
	}
	if kept, err := controllerStore.FleetDeployment(ctx, "deployment_the_node_answers_for"); err != nil || kept.State == "removed" {
		t.Fatalf("a refused clearing still changed the record: %+v err=%v", kept, err)
	}
	// Nothing was asked of the member: clearing is bookkeeping, and the model
	// it named is left exactly as it was.
	jobs, err := memberStore.ListJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("clearing records left %d jobs on the member: %+v", len(jobs), jobs)
	}
	// A cleared pair can be placed again, which is the whole point of clearing
	// it: adopt-on-demand rebuilds the row from what that Spark reports.
	if err := memberStore.SetInstalled(ctx, store.InstalledModel{RecipeID: held[0].ID, RecipeVersion: held[0].Version, Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rebuilt, created, err := controller.AdoptIndependentDeployment(ctx, member.identity.NodeID, held[0].ID, "adopt-after-clearing")
	if err != nil || !created || rebuilt.OwnerJobID == "" {
		t.Fatalf("a cleared pair could not be adopted again: created=%v record=%+v err=%v", created, rebuilt, err)
	}
}

// liveOwnerJob puts a job on the Spark that owns a record, in the state a job
// is in while it still runs. The controller reads it through the fleet, so the
// record it belongs to reads as live work.
func liveOwnerJob(t *testing.T, database *store.Store, recipeID, deploymentID string) store.Job {
	t.Helper()
	job, _, err := database.CreateJob(context.Background(), "install", recipeID, "live:"+deploymentID,
		map[string]any{"deployment_id": deploymentID})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" {
		t.Fatalf("a new job is in state %q, which this fixture reads as live work", job.State)
	}
	return job
}

// writeDeploymentRecord puts a record straight into the controller's store, the
// way an earlier placement would have left it. An empty job means the record
// never got one.
func writeDeploymentRecord(t *testing.T, database *store.Store, record store.FleetDeployment, job store.Job) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := database.CreateFleetDeployment(ctx, record,
		store.FleetDeploymentNode{NodeID: record.OwnerNodeID, ReservationID: "reservation_" + record.DeploymentID}); err != nil {
		t.Fatal(err)
	}
	if job.ID == "" {
		return
	}
	if err := database.SetFleetDeploymentJob(ctx, record.DeploymentID, job.ID, job.State, "2026-08-24T00:00:00Z"); err != nil {
		t.Fatal(err)
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

	// The create path already owns this pair under an id of its own, and its
	// install is still running there. One model on one node must stay one live
	// record, whatever id that record carries.
	writeDeploymentRecord(t, controllerStore, store.FleetDeployment{
		DeploymentID: "deployment_from_the_create_path", RecipeID: duplicated.ID, RecipeVersion: duplicated.Version,
		RecipeFingerprint: "fingerprint", TopologyCount: 1, OwnerNodeID: member.identity.NodeID, State: "running",
	}, liveOwnerJob(t, memberStore, duplicated.ID, "deployment_from_the_create_path"))

	for _, test := range []struct {
		name     string
		nodeID   string
		recipeID string
		key      string
		want     string
	}{
		{name: "a two node recipe", nodeID: member.identity.NodeID, recipeID: grouped.ID, key: "adopt-grouped", want: "cannot use independent placement"},
		{name: "a model the node does not report", nodeID: member.identity.NodeID, recipeID: absent.ID, key: "adopt-absent", want: "does not report that model as installed"},
		{name: "a model the node moved to another version", nodeID: member.identity.NodeID, recipeID: stale.ID, key: "adopt-stale", want: "the installed model on this Spark is a different version"},
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

// The whole sequence a slow Spark puts a row through: the fleet loses touch,
// the owner clears the record it can no longer read, adoption rebuilds the row
// from the heartbeat, and then the Spark comes back and answers for the job
// the cleared record named. The cleared record must stay cleared through all
// of it, or that one model on that one Spark ends up with two live records.
func TestAClearedRecordStaysClearedWhenTheSparkComesBack(t *testing.T) {
	ctx := context.Background()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	placed := independentRecipes(t, recipes, 1)[0]
	controller, controllerStore := newPlacementManager(t, "controller", "192.168.99.90", recipes)
	member, memberStore := newPlacementManager(t, "loft", "192.168.99.91", recipes)
	member.SetIndependentRuntime(&placementRuntime{database: memberStore, allocator: member.Allocator()})
	answering := inMemoryFleetClients(t, controller, map[string]*Manager{member.identity.CertificateFingerprint: member})
	// The same Spark, during the moments it does not answer in time. Nothing
	// about it has changed: it is still an active member, and the job it owns
	// is still on its disk.
	silent := func(string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("the Spark did not answer in time")
		})}
	}
	controller.newClient = answering
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Adopt(ctx, AdoptRequest{DisplayName: member.displayName, ConsoleURL: member.consoleURL, NodeURL: member.nodeURL, JoinCode: code.Code}); err != nil {
		t.Fatal(err)
	}
	if err := controller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	installed, created, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{
		RecipeID: placed.ID, NodeID: member.identity.NodeID, IdempotencyKey: "place-it",
		Intent: IndependentIntent{Confirmed: true, AcceptLicence: true, ConfirmTerritoryEligibility: true, Activate: true},
	})
	if err != nil || !created || installed.OwnerJobID == "" {
		t.Fatalf("the placement to clear was not made: created=%v deployment=%+v err=%v", created, installed, err)
	}

	// The Spark stops answering in time, so the record cannot be read and the
	// row it owns goes dead. The owner clears it.
	controller.newClient = silent
	cleared, ended, err := controller.ReleaseDeployment(ctx, installed.DeploymentID)
	if err != nil || !ended || cleared.State != "removed" {
		t.Fatalf("the stranded record was not cleared: ended=%v record=%+v err=%v", ended, cleared, err)
	}

	// The Spark comes back. Its job is still there and still readable, which is
	// exactly what would put the record back to work.
	controller.newClient = answering
	if _, err := controller.Deployments(ctx); err != nil {
		t.Fatal(err)
	}
	if stored, err := controllerStore.FleetDeployment(ctx, installed.DeploymentID); err != nil || stored.State != "removed" {
		t.Fatalf("a poll brought the cleared record back: %+v err=%v", stored, err)
	}
	// The console reads the same thing the store holds. The job is still
	// carried, because it is the truth about that Spark, but the record is
	// over.
	view, err := controller.Deployment(ctx, installed.DeploymentID)
	if err != nil || view.State != "removed" {
		t.Fatalf("the console reads the cleared record as %+v err=%v", view, err)
	}
	if view.Job == nil || view.Stale {
		t.Fatalf("the cleared record hides the job its Spark still answers for: %+v", view)
	}

	// A console tab that has not caught up still holds the cleared id and can
	// fire an action at it. The job it names is readable again, so the answer
	// would put the record back to work. Every action is refused instead.
	for _, action := range []string{"smoke-test", "benchmark", "start", "stop", "remove"} {
		if _, err := controller.ActionDeployment(ctx, installed.DeploymentID, action, "stale-tab-"+action, IndependentIntent{}); err == nil ||
			!strings.Contains(err.Error(), "nothing to act on") {
			t.Fatalf("%s against a cleared record was accepted: %v", action, err)
		}
	}
	if stored, err := controllerStore.FleetDeployment(ctx, installed.DeploymentID); err != nil || stored.State != "removed" {
		t.Fatalf("an action brought the cleared record back: %+v err=%v", stored, err)
	}

	// Adopt-on-demand rebuilds the row from what that Spark reports, which is
	// the whole point of clearing it.
	if err := controller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rebuilt, created, err := controller.AdoptIndependentDeployment(ctx, member.identity.NodeID, placed.ID, "adopt-after-clearing")
	if err != nil || !created || rebuilt.DeploymentID == installed.DeploymentID {
		t.Fatalf("the cleared pair was not rebuilt: created=%v record=%+v err=%v", created, rebuilt, err)
	}

	// One model on one Spark, one live record, however many polls run over it.
	for range 3 {
		if _, err := controller.Deployments(ctx); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := controllerStore.FleetDeployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live := []string{}
	for _, item := range stored {
		if item.OwnerNodeID == member.identity.NodeID && item.RecipeID == placed.ID && item.State != "removed" {
			live = append(live, item.DeploymentID)
		}
	}
	if len(live) != 1 || live[0] != rebuilt.DeploymentID {
		t.Fatalf("the pair holds %d live records: %v", len(live), live)
	}
}

// Adoption is the one deliberate way back from a cleared record, and it has to
// be: its id comes from the fleet, the node and the model, so a pair whose
// adopted record was cleared can only ever come back under that same id. If
// adoption handed the dead record back, that model would be wedged on that
// Spark for good.
func TestAdoptingAgainRebuildsAClearedRecordUnderItsOwnID(t *testing.T) {
	ctx := context.Background()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	held := independentRecipes(t, recipes, 2)
	adopted, guarded := held[0], held[1]
	controller, controllerStore := newPlacementManager(t, "controller", "192.168.99.100", recipes)
	member, memberStore := newPlacementManager(t, "loft", "192.168.99.101", recipes)
	member.SetIndependentRuntime(&placementRuntime{database: memberStore, allocator: member.Allocator()})
	answering := inMemoryFleetClients(t, controller, map[string]*Manager{member.identity.CertificateFingerprint: member})
	silent := func(string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("the Spark did not answer in time")
		})}
	}
	controller.newClient = answering
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Adopt(ctx, AdoptRequest{DisplayName: member.displayName, ConsoleURL: member.consoleURL, NodeURL: member.nodeURL, JoinCode: code.Code}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []store.InstalledModel{
		{RecipeID: adopted.ID, RecipeVersion: adopted.Version, Status: "ready", Active: true},
		{RecipeID: guarded.ID, RecipeVersion: guarded.Version, Status: "ready"},
	} {
		if err := memberStore.SetInstalled(ctx, model); err != nil {
			t.Fatal(err)
		}
	}
	if err := controller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	first, created, err := controller.AdoptIndependentDeployment(ctx, member.identity.NodeID, adopted.ID, "adopt-once")
	if err != nil || !created || first.OwnerJobID == "" {
		t.Fatalf("the record to clear was not adopted: created=%v record=%+v err=%v", created, first, err)
	}
	controller.newClient = silent
	if _, ended, err := controller.ReleaseDeployment(ctx, first.DeploymentID); err != nil || !ended {
		t.Fatalf("the adopted record was not cleared: ended=%v err=%v", ended, err)
	}
	controller.newClient = answering

	rebuilt, _, err := controller.AdoptIndependentDeployment(ctx, member.identity.NodeID, adopted.ID, "adopt-again")
	if err != nil {
		t.Fatalf("a cleared adopted record could not be rebuilt: %v", err)
	}
	// Same id, because that is the only id this pair can have, and a record
	// that is alive again with a job of its own.
	if rebuilt.DeploymentID != first.DeploymentID {
		t.Fatalf("the rebuild made a second id: %s and %s", first.DeploymentID, rebuilt.DeploymentID)
	}
	if rebuilt.State == "removed" || rebuilt.OwnerJobID == "" || rebuilt.Job == nil {
		t.Fatalf("the rebuilt record is not alive: %+v", rebuilt)
	}
	// It starts over rather than resuming: the job it names is not the one the
	// cleared record named.
	if rebuilt.OwnerJobID == first.OwnerJobID {
		t.Fatalf("the rebuilt record kept the job of the record that was cleared: %s", rebuilt.OwnerJobID)
	}
	// Polling cannot undo any of it, and the pair holds one record throughout.
	for range 3 {
		if _, err := controller.Deployments(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if live := liveRecordsFor(t, controllerStore, member.identity.NodeID, adopted.ID); len(live) != 1 || live[0] != rebuilt.DeploymentID {
		t.Fatalf("the pair holds %d live records: %v", len(live), live)
	}
	// Nothing was disturbed on the Spark: the model still serves exactly as it
	// did before the record was cleared and rebuilt.
	assertServingRecipe(t, memberStore, adopted.ID)

	// A removed record with work still running behind it should not exist. If
	// one ever does, the rebuild must not take that job away: the refusal is
	// read before the revival, not after it.
	config, err := controllerStore.FleetConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	guardedID := stablePlacementID("deployment_", config.FleetID, adoptPlacementKey(member.identity.NodeID, guarded.ID))
	fingerprint, err := RecipeFingerprint(guarded)
	if err != nil {
		t.Fatal(err)
	}
	writeDeploymentRecord(t, controllerStore, store.FleetDeployment{
		DeploymentID: guardedID, RecipeID: guarded.ID, RecipeVersion: guarded.Version,
		RecipeFingerprint: fingerprint, TopologyCount: 1, OwnerNodeID: member.identity.NodeID, State: "running",
	}, liveOwnerJob(t, memberStore, guarded.ID, guardedID))
	if err := controllerStore.ObserveFleetDeployment(ctx, guardedID, "removed", "2026-08-24T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.AdoptIndependentDeployment(ctx, member.identity.NodeID, guarded.ID, "adopt-guarded"); err == nil ||
		!strings.Contains(err.Error(), "still has work running for that model") {
		t.Fatalf("a removed record with live work behind it was rebuilt: %v", err)
	}
	if stored, err := controllerStore.FleetDeployment(ctx, guardedID); err != nil || stored.State != "removed" || stored.OwnerJobID == "" {
		t.Fatalf("the refused rebuild changed the record: %+v err=%v", stored, err)
	}
}

// liveRecordsFor names every record for one model on one Spark that the fleet
// has not let go of. One is the most there may ever be.
func liveRecordsFor(t *testing.T, database *store.Store, nodeID, recipeID string) []string {
	t.Helper()
	stored, err := database.FleetDeployments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	live := []string{}
	for _, item := range stored {
		if item.OwnerNodeID == nodeID && item.RecipeID == recipeID && item.State != "removed" {
			live = append(live, item.DeploymentID)
		}
	}
	return live
}
