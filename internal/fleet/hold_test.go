package fleet

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

// holdNamed is a RuntimeHolder that holds exactly the Sparks it is given by
// display name, the way an attached-service declaration names them.
func holdNamed(service string, names ...string) RuntimeHolder {
	return func(_, displayName string) (string, bool) {
		for _, name := range names {
			if name == displayName {
				return service, true
			}
		}
		return "", false
	}
}

func runtimeRequest(id, kind string, runtime bool) ReservationRequest {
	return ReservationRequest{
		ReservationID: id, DeploymentID: "deployment:" + id, RecipeID: "recipe-" + id, RecipeVersion: 1,
		Claims:       Claims{Version: ClaimsVersion, Kind: kind, Runtime: runtime, Ports: []int{}, FabricInterfaces: []string{}},
		PrepareToken: LocalPrepareToken(id),
	}
}

func TestHeldNodeRefusesNewRuntimeClaimsOnly(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	allocator := NewAllocator(database, "node-local")

	// Admitted before the service was declared: an install still downloading.
	early := runtimeRequest("early-install", ClaimKindLocalJob, true)
	if _, _, err := allocator.Prepare(ctx, early); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.Commit(ctx, early.ReservationID, early.PrepareToken, []byte(`{"kind":"local-engine"}`)); err != nil {
		t.Fatal(err)
	}

	allocator.SetRuntimeHolder(func() (string, bool) { return "Video pair", true })
	for _, kind := range []string{ClaimKindLocalJob, ClaimKindIndependent, ClaimKindLegacyRank} {
		_, _, err := allocator.Prepare(ctx, runtimeRequest("serve-"+kind, kind, true))
		var held HeldNodeError
		if !errors.As(err, &held) || held.Holder != "Video pair" {
			t.Fatalf("%s runtime claim on a held node: err = %v", kind, err)
		}
		if !strings.Contains(err.Error(), "the attached service Video pair has this Spark reserved outside Basement") {
			t.Fatalf("refusal must name the service: %v", err)
		}
	}
	if _, err := allocator.Reservation(ctx, "serve-"+ClaimKindLocalJob); err == nil {
		t.Fatal("a refused claim must leave no row behind")
	}
	// A download claims no memory, and neither does the maintenance latch of
	// a manager update; a recovered claim only records what already serves.
	for _, request := range []ReservationRequest{
		runtimeRequest("download-only", ClaimKindLocalJob, false),
		runtimeRequest("manager-update", ClaimKindUpdate, true),
		runtimeRequest("recovered", ClaimKindRecovered, true),
	} {
		if _, _, err := allocator.Prepare(ctx, request); err != nil {
			t.Fatalf("%s was refused on a held node: %v", request.ReservationID, err)
		}
	}
	// A retry of the claim admitted earlier answers as it did, but it cannot
	// take the runtime slot now.
	if _, _, err := allocator.Prepare(ctx, early); err != nil {
		t.Fatalf("a retry of an admitted claim must answer unchanged: %v", err)
	}
	if err := allocator.Activate(ctx, early.ReservationID, ""); !errors.As(err, new(HeldNodeError)) {
		t.Fatalf("an admitted install took the runtime slot of a held node: %v", err)
	}

	allocator.SetRuntimeHolder(nil)
	if err := allocator.Activate(ctx, early.ReservationID, ""); err != nil {
		t.Fatalf("removing the declaration must release the node: %v", err)
	}
	// Already active stays active: a declaration added now does not reach
	// back into a model that serves.
	allocator.SetRuntimeHolder(func() (string, bool) { return "Video pair", true })
	if err := allocator.Activate(ctx, early.ReservationID, ""); err != nil {
		t.Fatalf("re-activating an active claim must not be refused: %v", err)
	}
}

func TestControllerKeepsPlacementsOffHeldSparks(t *testing.T) {
	ctx := context.Background()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	placeable := independentRecipes(t, recipes, 2)
	first, second := placeable[0], placeable[1]
	controller, _ := newPlacementManager(t, "controller", "192.0.2.10", recipes)
	memberA, memberAStore := newPlacementManager(t, "node-a", "192.0.2.20", recipes)
	memberB, memberBStore := newPlacementManager(t, "node-b", "192.0.2.21", recipes)
	memberA.SetIndependentRuntime(&placementRuntime{database: memberAStore, allocator: memberA.Allocator()})
	memberB.SetIndependentRuntime(&placementRuntime{database: memberBStore, allocator: memberB.Allocator()})
	controller.newClient = inMemoryFleetClients(t, controller, map[string]*Manager{
		memberA.identity.CertificateFingerprint: memberA, memberB.identity.CertificateFingerprint: memberB,
	})
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
	intent := IndependentIntent{Confirmed: true, AcceptLicence: true, ConfirmTerritoryEligibility: true, Activate: true}
	onB, _, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: second.ID, NodeID: memberB.identity.NodeID, IdempotencyKey: "before-declaration", Intent: intent})
	if err != nil {
		t.Fatal(err)
	}

	// The declaration lives on the controller only; neither member has one.
	controller.SetRuntimeHolder(holdNamed("Video pair", "controller", "node-a", "node-b"))
	plan, err := controller.PlanIndependent(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := "The attached service Video pair has this Spark reserved outside Basement, so it cannot take a model now."
	for _, candidate := range plan.Candidates {
		if candidate.Eligible || candidate.Reason != want {
			t.Fatalf("candidate %s = %+v", candidate.DisplayName, candidate)
		}
	}
	if plan.RecommendedNodeID != "" {
		t.Fatal("a plan with every Spark held recommends none of them")
	}
	_, _, err = controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: first.ID, NodeID: memberA.identity.NodeID, IdempotencyKey: "onto-held", Intent: intent})
	if err == nil || err.Error() != "node-a could not do this: "+want {
		t.Fatalf("placement onto a held member: %v", err)
	}
	if jobs, _ := memberAStore.ListJobs(ctx, 10); len(jobs) != 0 {
		t.Fatalf("a refused placement reached the member: %+v", jobs)
	}

	// The model placed before the declaration can still be stopped, and not
	// started again.
	if _, err := controller.ActionDeployment(ctx, onB.DeploymentID, "stop", "stop-on-b", IndependentIntent{}); err != nil {
		t.Fatalf("stopping a model on a held Spark must stay possible: %v", err)
	}
	if _, err := controller.ActionDeployment(ctx, onB.DeploymentID, "start", "start-on-b", IndependentIntent{}); err == nil ||
		err.Error() != "node-b could not do this: "+want {
		t.Fatalf("starting a model on a held Spark: %v", err)
	}

	controller.SetRuntimeHolder(holdNamed("Video pair", "node-b"))
	plan, err = controller.PlanIndependent(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range plan.Candidates {
		if held := candidate.DisplayName == "node-b"; candidate.Eligible == held {
			t.Fatalf("only node-b is held: %+v", candidate)
		}
	}
	if _, _, err := controller.CreateIndependentDeployment(ctx, CreateDeploymentRequest{RecipeID: first.ID, NodeID: memberA.identity.NodeID, IdempotencyKey: "onto-free", Intent: intent}); err != nil {
		t.Fatalf("a Spark no declaration names must still take placements: %v", err)
	}
}

func TestManagerHolderReachesItsOwnAllocator(t *testing.T) {
	manager, _ := newTestManager(t, "head-box", "192.0.2.10")
	asked := ""
	manager.SetRuntimeHolder(func(nodeID, displayName string) (string, bool) {
		asked = nodeID + "/" + displayName
		return "Video pair", true
	})
	_, _, err := manager.Allocator().Prepare(context.Background(), runtimeRequest("local", ClaimKindLocalJob, true))
	if !errors.As(err, new(HeldNodeError)) {
		t.Fatalf("the manager's own allocator ignored its holder: %v", err)
	}
	if asked != manager.identity.NodeID+"/head-box" {
		t.Fatalf("the allocator asked about %q, not this node", asked)
	}
	manager.SetRuntimeHolder(nil)
	if _, _, err := manager.Allocator().Prepare(context.Background(), runtimeRequest("local", ClaimKindLocalJob, true)); err != nil {
		t.Fatal(err)
	}
}
