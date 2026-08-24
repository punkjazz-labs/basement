package fleet

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/punkjazz-labs/basement/internal/power"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

// recordingSMI is the nvidia-smi of a Spark in these tests. Nothing here ever
// touches a GPU, so it only records what one would have been asked for.
type recordingSMI struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingSMI) run(_ context.Context, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, strings.Join(args, " "))
	return nil
}

func (r *recordingSMI) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// One controller, one member, one switch. The choice travels over the mutual
// TLS fleet plane, the member writes it into its own store and asks its own
// GPU for it, and the controller learns the mode of every node from the
// heartbeat it already collects.
func TestControllerSetsAMemberPowerModeOverTheFleetPlane(t *testing.T) {
	ctx := context.Background()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	controller, controllerStore := newPlacementManager(t, "controller", "192.168.99.40", recipes)
	member, memberStore := newPlacementManager(t, "node-a", "192.168.99.41", recipes)
	controllerSMI, memberSMI := &recordingSMI{}, &recordingSMI{}
	controller.SetPowerRuntime(power.NewController(controllerStore, controllerSMI.run))
	member.SetPowerRuntime(power.NewController(memberStore, memberSMI.run))
	controller.newClient = inMemoryFleetClients(t, controller, map[string]*Manager{member.identity.CertificateFingerprint: member})
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Adopt(ctx, AdoptRequest{DisplayName: member.displayName, ConsoleURL: member.consoleURL, NodeURL: member.nodeURL, JoinCode: code.Code}); err != nil {
		t.Fatal(err)
	}

	state, err := controller.SetNodePowerMode(ctx, member.identity.NodeID, store.PowerModeCool)
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != store.PowerModeCool || state.Failure != "" {
		t.Fatalf("the member answered %+v", state)
	}
	stored, err := memberStore.PowerMode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Mode != store.PowerModeCool {
		t.Fatalf("the member store holds %+v", stored)
	}
	if calls := memberSMI.recorded(); len(calls) != 1 || calls[0] != "-lgc 0,2200" {
		t.Fatalf("the member GPU was asked for %v", calls)
	}
	if calls := controllerSMI.recorded(); len(calls) != 0 {
		t.Fatalf("a member change reached the controller GPU: %v", calls)
	}

	// The controller sets its own mode through the same call, from its own
	// runtime, so one dashboard drives every Spark including this one.
	if _, err := controller.SetNodePowerMode(ctx, controller.identity.NodeID, store.PowerModeCool); err != nil {
		t.Fatal(err)
	}
	if calls := controllerSMI.recorded(); len(calls) != 1 || calls[0] != "-lgc 0,2200" {
		t.Fatalf("the controller GPU was asked for %v", calls)
	}

	// The heartbeat is the whole read path. After one poll the summary knows
	// what every node is set to, with no call of its own.
	if err := controller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	summary, err := controller.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	memberSummary := findNodeSummary(t, summary, member.identity.NodeID)
	if memberSummary.PowerMode != store.PowerModeCool || memberSummary.PowerModeFailure != "" {
		t.Fatalf("the member row reads mode=%q failure=%q", memberSummary.PowerMode, memberSummary.PowerModeFailure)
	}
	controllerSummary := findNodeSummary(t, summary, controller.identity.NodeID)
	if controllerSummary.PowerMode != store.PowerModeCool {
		t.Fatalf("the controller row reads mode=%q", controllerSummary.PowerMode)
	}

	// A Spark that cannot take the cap says so in the row, and the mode it was
	// asked for stays what its owner chose.
	if _, err := memberStore.RecordPowerModeFailure(ctx, "This machine has no nvidia-smi command, so the GPU clock did not change."); err != nil {
		t.Fatal(err)
	}
	if err := controller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	summary, err = controller.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failing := findNodeSummary(t, summary, member.identity.NodeID)
	if failing.PowerMode != store.PowerModeCool || !strings.Contains(failing.PowerModeFailure, "nvidia-smi") {
		t.Fatalf("a refused cap reads mode=%q failure=%q", failing.PowerMode, failing.PowerModeFailure)
	}
}

// A Spark that does not answer is named by the name its owner gave it, in the
// one sentence every member operation uses for silence.
func TestPowerModeFailureNamesTheSparkByItsDisplayName(t *testing.T) {
	ctx := context.Background()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	controller, controllerStore := newPlacementManager(t, "controller", "192.168.99.50", recipes)
	member, memberStore := newPlacementManager(t, "kitchen-spark", "192.168.99.51", recipes)
	controller.SetPowerRuntime(power.NewController(controllerStore, (&recordingSMI{}).run))
	member.SetPowerRuntime(power.NewController(memberStore, (&recordingSMI{}).run))
	controller.newClient = inMemoryFleetClients(t, controller, map[string]*Manager{member.identity.CertificateFingerprint: member})
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Adopt(ctx, AdoptRequest{DisplayName: member.displayName, ConsoleURL: member.consoleURL, NodeURL: member.nodeURL, JoinCode: code.Code}); err != nil {
		t.Fatal(err)
	}

	// Every address in this fleet now leads nowhere.
	controller.newClient = inMemoryFleetClients(t, controller, map[string]*Manager{})
	_, err = controller.SetNodePowerMode(ctx, member.identity.NodeID, store.PowerModeCool)
	if err == nil {
		t.Fatal("a silent Spark accepted a power mode")
	}
	if !strings.HasPrefix(err.Error(), "kitchen-spark ") {
		t.Fatalf("the failure does not name the Spark: %q", err)
	}
	stored, storeErr := memberStore.PowerMode(ctx)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	if stored.Mode != store.PowerModeFull {
		t.Fatalf("a failed call changed the member setting to %q", stored.Mode)
	}

	// A Spark this fleet does not hold at all is refused before any call.
	if _, err := controller.SetNodePowerMode(ctx, "node_absent", store.PowerModeCool); err == nil ||
		!strings.Contains(err.Error(), "the fleet does not hold this Spark as an active member") {
		t.Fatalf("an unknown node answered %v", err)
	}
}

// The heartbeat a member signs carries its own mode. This is the only read
// path the fleet dashboard has for it, so it is the one that must be true.
func TestHeartbeatCarriesThePowerMode(t *testing.T) {
	ctx := context.Background()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	node, database := newPlacementManager(t, "node-a", "192.168.99.60", recipes)
	if _, err := database.SetPowerMode(ctx, store.PowerModeCool); err != nil {
		t.Fatal(err)
	}
	const sentence = "The nvidia-smi command failed, so the GPU clock did not change."
	if _, err := database.RecordPowerModeFailure(ctx, sentence); err != nil {
		t.Fatal(err)
	}
	envelope, err := BuildHeartbeat(ctx, node.identity, database, node.inventory, "fleet_test", "test", "test-build", "catalogue", node.now())
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Payload.PowerMode != store.PowerModeCool || envelope.Payload.PowerModeFailure != sentence {
		t.Fatalf("the heartbeat carries mode=%q failure=%q", envelope.Payload.PowerMode, envelope.Payload.PowerModeFailure)
	}
	if err := VerifyHeartbeat(envelope, node.identity.PublicKey, "fleet_test", node.identity.NodeID); err != nil {
		t.Fatalf("the heartbeat with a power mode does not verify: %v", err)
	}
}
