package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/punkjazz-labs/runonspark-manager/internal/operations"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
	"github.com/punkjazz-labs/runonspark-manager/internal/store"
)

// twoSparkRecipe is a shipped single-Spark recipe given the interconnect a
// two-Spark recipe must carry. No two-Spark recipe ships yet, so the fixture
// lives in the tests.
func twoSparkRecipe(t *testing.T) recipe.Recipe {
	t.Helper()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("base recipe missing")
	}
	r.Topology = recipe.Topology{SparkCount: 2, Interconnect: &recipe.Interconnect{
		Kind:       "connectx7",
		MasterPort: 29501,
		SharedEnvironment: map[string]string{
			"NCCL_IB_DISABLE": "0", "NCCL_IB_HCA": "rocep1s0f0", "NCCL_IB_GID_INDEX": "3",
			"NCCL_SOCKET_IFNAME": "enp1s0f0np0", "GLOO_SOCKET_IFNAME": "enp1s0f0np0",
			"TP_SOCKET_IFNAME": "enp1s0f0np0",
		},
	}}
	r.Service.VLLM.TensorParallelSize = 2
	if err := recipe.Validate(r); err != nil {
		t.Fatalf("fixture is not a valid two-Spark recipe: %v", err)
	}
	return r
}

// fleetExecutor is a deterministic stand-in for two Sparks. It records every
// step as "operation@node" so orchestration order is asserted directly, and
// it can be told which node fails which step.
type fleetExecutor struct {
	mu     sync.Mutex
	events []string
	// detail additionally records which recipe each step was for, needed
	// once more than one model is involved in a job.
	detail       []string
	running      map[string]bool
	failStepNode string // "operation@role"
	failRecipeOp string // "operation/recipe-id"
	peerReady    bool
	peerAsked    int
}

func newFleetExecutor() *fleetExecutor {
	return &fleetExecutor{running: map[string]bool{}, peerReady: true}
}

func (f *fleetExecutor) Plan(_ context.Context, r recipe.Recipe) (operations.Deployment, error) {
	if !r.Distributed() {
		return operations.Deployment{}, nil
	}
	return operations.Deployment{
		Head:   operations.Placement{Role: operations.RoleHead, NodeName: "spark-a", NodeCount: 2, MasterAddress: "169.254.10.1", MasterPort: 29501},
		Worker: operations.Placement{Role: operations.RoleWorker, NodeName: "spark-b", PeerID: "peer_1", NodeCount: 2, MasterAddress: "169.254.10.1", MasterPort: 29501},
		Peer:   operations.PeerTarget{ID: "peer_1", Name: "spark-b", BaseURL: "http://spark-b:8181", APIKey: "worker-key"},
	}, nil
}

func (f *fleetExecutor) ArtifactPath(r recipe.Recipe) string { return "/managed/" + r.ID }
func (f *fleetExecutor) RuntimeImageBytes(context.Context, recipe.Recipe) (int64, bool) {
	return 0, false
}

func (f *fleetExecutor) node(placement operations.Placement) string {
	if !placement.Distributed() {
		return "local"
	}
	return placement.Role
}

func (f *fleetExecutor) Execute(_ context.Context, execution operations.Execution, op recipe.Operation, r recipe.Recipe, _ operations.Progress) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	node := f.node(execution.Placement)
	key := op.Type + "@" + node
	f.events = append(f.events, key)
	f.detail = append(f.detail, op.Type+"/"+r.ID+"@"+node)
	receipt := map[string]any{"operation": op.Type, "node": execution.Placement.NodeName, "node_role": execution.Placement.Role}
	if op.Type == operations.VerifyPeerNode {
		f.peerAsked++
		if !f.peerReady {
			return receipt, errors.New("the other Spark is not ready to run this model: verify_disk: not enough free space")
		}
		return receipt, nil
	}
	if key == f.failStepNode || op.Type+"/"+r.ID == f.failRecipeOp {
		return receipt, fmt.Errorf("%s failed on %s", op.Type, node)
	}
	switch op.Type {
	case "start_container":
		f.running[node] = true
	case "stop_container":
		f.running[node] = false
	case "wait_http", "verify_openai_inference":
		if !f.running[operations.RoleHead] && !f.running["local"] {
			return receipt, errors.New("nothing is serving")
		}
	}
	return receipt, nil
}

func (f *fleetExecutor) Completed(context.Context, operations.Execution, recipe.Operation, recipe.Recipe, json.RawMessage) bool {
	return false
}

func (f *fleetExecutor) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.events...)
}

func (f *fleetExecutor) recordedDetail() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.detail...)
}

func indexOf(events []string, want string) int {
	for index, event := range events {
		if event == want {
			return index
		}
	}
	return -1
}

func newTwoSparkEngine(t *testing.T, fake *fleetExecutor) (*Engine, *store.Store, recipe.Recipe) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r := twoSparkRecipe(t)
	return New(s, fake, []recipe.Recipe{r}), s, r
}

func TestTwoSparkInstallStagesBothNodesAndStartsTheWorkerFirst(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	runner, s, r := newTwoSparkEngine(t, fake)
	job, _, err := s.CreateJob(ctx, "install", r.ID, "two-spark-install", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	waitJob(t, s, job.ID, "ready")

	events := fake.recorded()
	if fake.peerAsked != 1 {
		t.Fatalf("the worker manager was asked to check itself %d times, want once", fake.peerAsked)
	}
	// The worker's own preflight gates every byte written to either node.
	if peer := indexOf(events, operations.VerifyPeerNode+"@worker"); peer < 0 || peer > indexOf(events, "pull_image@head") {
		t.Fatalf("the worker was not verified before staging started: %v", events)
	}
	for _, staged := range []string{"pull_image", "download_artifact", "write_generated_config"} {
		if indexOf(events, staged+"@head") < 0 || indexOf(events, staged+"@worker") < 0 {
			t.Fatalf("%s did not run on both nodes: %v", staged, events)
		}
	}
	if indexOf(events, "start_container@worker") > indexOf(events, "create_container@head") {
		t.Fatalf("the worker rank must be up before the head container is created: %v", events)
	}
	if indexOf(events, "start_container@head") > indexOf(events, "wait_http@head") {
		t.Fatalf("the head was health-checked before it started: %v", events)
	}
	// Only the head serves, so only the head is waited on and tested.
	for _, headOnly := range []string{"wait_http@worker", "verify_openai_inference@worker", "verify_port@worker"} {
		if indexOf(events, headOnly) >= 0 {
			t.Fatalf("%s ran on the worker: %v", headOnly, events)
		}
	}

	finished, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]int{}
	for _, step := range finished.Steps {
		var receipt map[string]any
		if json.Unmarshal(step.Receipt, &receipt) != nil {
			continue
		}
		role, _ := receipt["node_role"].(string)
		name, _ := receipt["node"].(string)
		if role == "" || name == "" {
			t.Fatalf("step %s has a receipt that does not name its node: %s", step.Operation, string(step.Receipt))
		}
		if !strings.HasSuffix(step.Operation, ":"+role) {
			t.Fatalf("step %s is not named for the node that ran it (%s)", step.Operation, role)
		}
		nodes[name]++
	}
	if nodes["spark-a"] == 0 || nodes["spark-b"] == 0 {
		t.Fatalf("receipts do not cover both Sparks: %v", nodes)
	}
}

func TestTwoSparkInstallStopsBothNodesWhenTheWorkerFails(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	fake.failStepNode = "start_container@worker"
	runner, s, r := newTwoSparkEngine(t, fake)
	job, _, err := s.CreateJob(ctx, "install", r.ID, "worker-fails", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if !strings.Contains(failed.Error, "start_container failed on worker") {
		t.Fatalf("the failure did not name the node: %s", failed.Error)
	}
	events := fake.recorded()
	// Nothing on the head may have been created once the worker was known bad.
	if indexOf(events, "create_container@head") >= 0 {
		t.Fatalf("the head was brought up after the worker failed: %v", events)
	}
	assertBothNodesTornDown(t, s, job.ID, events)
}

func TestTwoSparkInstallStopsBothNodesWhenTheHeadNeverBecomesHealthy(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	fake.failStepNode = "wait_http@head"
	runner, s, r := newTwoSparkEngine(t, fake)
	job, _, err := s.CreateJob(ctx, "install", r.ID, "head-unhealthy", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if !strings.Contains(failed.Error, "wait_http failed on head") {
		t.Fatalf("the failure did not name the node: %s", failed.Error)
	}
	assertBothNodesTornDown(t, s, job.ID, fake.recorded())
}

func TestTwoSparkInstallRefusesWhenTheWorkerIsNotReady(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	fake.peerReady = false
	runner, s, r := newTwoSparkEngine(t, fake)
	job, _, err := s.CreateJob(ctx, "install", r.ID, "worker-not-ready", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if !strings.Contains(failed.Error, "not ready to run this model") {
		t.Fatalf("unexpected failure: %s", failed.Error)
	}
	for _, event := range fake.recorded() {
		if strings.HasPrefix(event, "pull_image") || strings.HasPrefix(event, "download_artifact") {
			t.Fatalf("staging started despite an unready worker: %v", fake.recorded())
		}
	}
}

func TestTwoSparkRemoveTearsDownBothNodes(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	runner, s, r := newTwoSparkEngine(t, fake)
	install, _, err := s.CreateJob(ctx, "install", r.ID, "install-before-remove", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(install.ID)
	waitJob(t, s, install.ID, "ready")

	remove, _, err := s.CreateJob(ctx, "remove", r.ID, "remove-both", RemovePayload{RemoveArtifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(remove.ID)
	waitJob(t, s, remove.ID, "removed")

	events := fake.recorded()
	for _, want := range []string{"stop_container@head", "stop_container@worker", "remove_container@head", "remove_container@worker", "remove_artifact_if_unshared@head", "remove_artifact_if_unshared@worker"} {
		if indexOf(events, want) < 0 {
			t.Fatalf("%s did not run: %v", want, events)
		}
	}
	if indexOf(events, "stop_container@head") > indexOf(events, "stop_container@worker") {
		t.Fatalf("the head must stop serving before its worker rank goes away: %v", events)
	}
}

func TestSingleSparkJobsNeverConsultTheFleet(t *testing.T) {
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
	fake := newFleetExecutor()
	runner := New(s, fake, recipes)
	job, _, err := s.CreateJob(ctx, "install", recipes[0].ID, "single-spark", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	waitJob(t, s, job.ID, "ready")
	if fake.peerAsked != 0 {
		t.Fatal("a single-Spark install asked a peer for permission")
	}
	for _, event := range fake.recorded() {
		if !strings.HasSuffix(event, "@local") {
			t.Fatalf("a single-Spark install ran %s off-node", event)
		}
	}
	finished, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range finished.Steps {
		if strings.Contains(step.Operation, ":") {
			t.Fatalf("a single-Spark step gained a node suffix: %s", step.Operation)
		}
	}
}

// assertBothNodesTornDown checks the failure contract: whatever failed, both
// Sparks are returned to stopped and each teardown is its own named receipt.
func assertBothNodesTornDown(t *testing.T, s *store.Store, jobID string, events []string) {
	t.Helper()
	for _, role := range []string{operations.RoleHead, operations.RoleWorker} {
		found := false
		for _, event := range events {
			if event == "stop_container@"+role {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s was never stopped: %v", role, events)
		}
	}
	job, err := s.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	teardowns := map[string]bool{}
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Operation, "teardown_stop_container:") {
			teardowns[step.Operation] = true
		}
	}
	for _, role := range []string{operations.RoleHead, operations.RoleWorker} {
		if !teardowns["teardown_stop_container:"+role] {
			t.Fatalf("no teardown receipt for the %s node: %v", role, teardowns)
		}
	}
}

// mixedFleet installs a distributed model, then activates a single-node one,
// which is the case where reading the TARGET's topology would leave the
// predecessor's worker rank running.
func mixedFleet(t *testing.T, fake *fleetExecutor) (*Engine, *store.Store, recipe.Recipe, recipe.Recipe) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	distributed := twoSparkRecipe(t)
	distributed.ID = "two-spark-model"
	builtin, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	single, _ := recipe.Find(builtin, "qwen36-35b-a3b-nvfp4-1s")
	return New(s, fake, []recipe.Recipe{distributed, single}), s, distributed, single
}

func TestSwitchingAwayFromADistributedModelStopsBothOfItsRanks(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	runner, s, distributed, single := mixedFleet(t, fake)

	first, _, err := s.CreateJob(ctx, "install", distributed.ID, "install-distributed", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(first.ID)
	waitJob(t, s, first.ID, "ready")

	fake.mu.Lock()
	fake.events, fake.detail = nil, nil
	fake.mu.Unlock()

	second, _, err := s.CreateJob(ctx, "install", single.ID, "install-single", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(second.ID)
	waitJob(t, s, second.ID, "ready")

	detail := fake.recordedDetail()
	for _, want := range []string{"stop_container/two-spark-model@head", "stop_container/two-spark-model@worker"} {
		if indexOf(detail, want) < 0 {
			t.Fatalf("%s was not stopped when a single-node model took over: %v", want, detail)
		}
	}
	if indexOf(detail, "stop_container/two-spark-model@head") > indexOf(detail, "stop_container/two-spark-model@worker") {
		t.Fatalf("the outgoing head must stop serving before its worker rank: %v", detail)
	}
	// The incoming single-node model never touches a second Spark.
	for _, event := range detail {
		if strings.HasSuffix(event, "@worker") && !strings.Contains(event, distributed.ID) {
			t.Fatalf("a single-node install reached the other Spark: %s", event)
		}
	}
}

func TestRollbackRestoresADistributedPredecessorOnBothItsNodes(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	runner, s, distributed, single := mixedFleet(t, fake)

	first, _, err := s.CreateJob(ctx, "install", distributed.ID, "install-distributed", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(first.ID)
	waitJob(t, s, first.ID, "ready")

	// The single-node model comes up and fails its health check, so the
	// distributed model it displaced has to come back on both of its nodes.
	fake.failRecipeOp = "wait_http/" + single.ID
	second, _, err := s.CreateJob(ctx, "install", single.ID, "install-single", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(second.ID)
	failed := waitJob(t, s, second.ID, "failed")
	if !strings.Contains(failed.Error, "restored and verified") {
		t.Fatalf("the previous model was not restored: %s", failed.Error)
	}

	job, err := s.GetJob(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, step := range job.Steps {
		names = append(names, step.Operation)
	}
	for _, want := range []string{"rollback_verify_memory:worker", "rollback_start_container:worker", "rollback_start_container:head", "rollback_verify_openai_inference:head"} {
		if indexOf(names, want) < 0 {
			t.Fatalf("rollback did not follow the previous model's topology: %v", names)
		}
	}
	if indexOf(names, "rollback_start_container:worker") > indexOf(names, "rollback_start_container:head") {
		t.Fatalf("the restored worker rank must come up before its head: %v", names)
	}
	models, err := s.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		if model.RecipeID == distributed.ID && !model.Active {
			t.Fatalf("the distributed model was not made active again: %#v", model)
		}
	}
}

func TestTeardownStopsBothRanksEvenWhenReceiptsCannotBePersisted(t *testing.T) {
	fake := newFleetExecutor()
	runner, s, r := newTwoSparkEngine(t, fake)
	job, _, err := s.CreateJob(context.Background(), "install", r.ID, "teardown-without-storage", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := fake.Plan(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	// Losing the state database must not become a reason to leave ranks
	// running on either Spark.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	runner.teardownDistributed(job, r, 0, deployment)
	events := fake.recorded()
	for _, want := range []string{"stop_container@head", "stop_container@worker"} {
		if indexOf(events, want) < 0 {
			t.Fatalf("%s did not run once receipts could not be written: %v", want, events)
		}
	}
}
