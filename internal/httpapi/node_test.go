package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/resourceguard"
	"github.com/punkjazz-labs/basement/internal/store"
)

// twoSparkRecipe is a shipped single-Spark recipe given the interconnect a
// two-Spark recipe must carry. No two-Spark recipe ships yet.
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
			"NCCL_IB_HCA": "rocep1s0f0", "NCCL_IB_GID_INDEX": "3",
			"NCCL_SOCKET_IFNAME": "enp1s0f0np0", "GLOO_SOCKET_IFNAME": "enp1s0f0np0",
		},
	}}
	r.Service.VLLM.TensorParallelSize = 2
	if err := recipe.Validate(r); err != nil {
		t.Fatalf("fixture is not a valid two-Spark recipe: %v", err)
	}
	return r
}

// refusingPeer stands in for the other Spark. A worker must never call one:
// if the node endpoints forwarded delegated work instead of running it, this
// fails the test loudly rather than silently recursing.
type refusingPeer struct{ t *testing.T }

func (p refusingPeer) Target(context.Context) (operations.PeerTarget, error) {
	p.t.Error("a delegated worker step was forwarded to another Spark instead of run locally")
	return operations.PeerTarget{}, errors.New("no peer")
}
func (p refusingPeer) Preflight(context.Context, operations.PeerTarget, operations.Execution, recipe.Recipe) (map[string]any, error) {
	p.t.Error("a delegated worker preflight was forwarded to another Spark")
	return nil, errors.New("no peer")
}
func (p refusingPeer) Fabric(context.Context, operations.PeerTarget, recipe.Recipe) (operations.FabricProbe, error) {
	p.t.Error("a delegated cable check was forwarded to another Spark")
	return operations.FabricProbe{}, errors.New("no peer")
}
func (p refusingPeer) Step(context.Context, operations.PeerTarget, operations.Execution, recipe.Operation, recipe.Recipe, operations.Progress) (map[string]any, error) {
	p.t.Error("a delegated worker step was forwarded to another Spark instead of run locally")
	return nil, errors.New("no peer")
}

type nodeFixture struct {
	key         string
	distributed recipe.Recipe
	// sglang is the shipped two-Spark SGLang recipe. Its worker rank binds a
	// host port of its own, which a vLLM worker never does.
	sglang    recipe.Recipe
	single    recipe.Recipe
	localExec *apiExecutor
	api       *Server
	post      func(t *testing.T, path, key string, body any) (int, map[string]any)
}

// newNodeFixture wires the server exactly as production does: the executor
// handed to httpapi.New is the FleetExecutor, the same object the engine
// uses. That composition is the whole point of the local-execution test.
func newNodeFixture(t *testing.T) nodeFixture {
	t.Helper()
	dataDir := t.TempDir()
	database, err := store.Open(filepath.Join(dataDir, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	authManager, err := auth.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	builtin, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	distributed := twoSparkRecipe(t)
	// A worker only runs what its own catalogue holds, so the two-Spark
	// recipe has to be in it, under its own id.
	distributed.ID = "qwen36-35b-a3b-nvfp4-2s"
	single, _ := recipe.Find(builtin, "qwen36-35b-a3b-nvfp4-1s")
	sglang, ok := recipe.Find(builtin, "inkling-small-nvfp4-2s")
	if !ok {
		t.Fatal("two-Spark SGLang recipe missing")
	}
	catalog := append(append([]recipe.Recipe{}, builtin...), distributed)

	local := &apiExecutor{done: map[string]bool{}}
	executor := operations.NewFleetExecutor(local, refusingPeer{t: t})
	runner := engine.New(database, executor, catalog)
	api := New("test-version", dataDir, authManager, database, readyInventory{}, executor, runner, catalog)
	server := httptest.NewServer(api.Handler())
	t.Cleanup(api.Close)
	t.Cleanup(server.Close)
	_, secret, err := database.CreateAPIKey(context.Background(), "other-spark")
	if err != nil {
		t.Fatal(err)
	}

	post := func(t *testing.T, path, key string, body any) (int, map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		return response.StatusCode, decoded
	}
	return nodeFixture{key: secret, distributed: distributed, sglang: sglang, single: single, localExec: local, api: api, post: post}
}

// executed reports whether the local executor actually ran an operation.
func (a *apiExecutor) executed(operation string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done[operation]
}

func (a *apiExecutor) isRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// reservationFor reports the claim an operation ran under. An empty string
// means the step ran without a reservation of any kind.
func (a *apiExecutor) reservationFor(operation string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reserved[operation]
}

func workerStep(operation, jobID string, r recipe.Recipe) map[string]any {
	return map[string]any{
		"operation": operation, "recipe": r, "job_id": jobID,
		"placement": map[string]any{"role": "worker", "node": "spark-b", "node_count": 2},
	}
}

// workerStepReplacing is a delegated step of a job that takes this node's
// runtime slot from another model, which is what every step of a switch is
// until the model it replaces has been stopped.
func workerStepReplacing(operation, jobID string, r recipe.Recipe, replaces string) map[string]any {
	step := workerStep(operation, jobID, r)
	step["replaces_recipe_id"] = replaces
	return step
}

func TestDelegatedWorkerStepsRunLocallyAndAreNotForwarded(t *testing.T) {
	fixture := newNodeFixture(t)
	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("create_container", "job-1", fixture.distributed))
	if status != http.StatusOK {
		t.Fatalf("worker step status=%d body=%#v", status, body)
	}
	if body["error"] != nil {
		t.Fatalf("worker step reported %v", body["error"])
	}
	receipt, ok := body["receipt"].(map[string]any)
	if !ok || receipt["operation"] != "create_container" {
		t.Fatalf("worker step receipt=%#v", body["receipt"])
	}
	// refusingPeer fails the test if anything was forwarded; this asserts the
	// work actually landed on the local executor.
	if !fixture.localExec.executed("create_container") {
		t.Fatal("the worker container was never created on this node")
	}

	if status, _ := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-1"}); status != http.StatusOK {
		t.Fatalf("worker preflight status=%d", status)
	}
}

func TestWorkerOnlyRunsRecipesFromItsOwnCatalogue(t *testing.T) {
	fixture := newNodeFixture(t)

	tampered := fixture.distributed
	tampered.Runtime.Image = "ghcr.io/attacker/anything"
	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("create_container", "job-evil", tampered))
	if status != http.StatusBadRequest {
		t.Fatalf("an attacker-chosen image was accepted, status=%d body=%#v", status, body)
	}
	if fixture.localExec.executed("create_container") {
		t.Fatal("a container was created from a recipe this Spark does not hold")
	}

	// A schema-valid recipe this Spark simply does not have is refused too.
	unknown := fixture.distributed
	unknown.ID = "not-in-this-catalogue"
	if status, _ := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("pull_image", "job-evil", unknown)); status != http.StatusBadRequest {
		t.Fatalf("an unknown recipe was accepted, status=%d", status)
	}

	// So is a version this Spark has never verified.
	future := fixture.distributed
	future.Version = fixture.distributed.Version + 1
	if status, _ := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("pull_image", "job-evil", future)); status != http.StatusBadRequest {
		t.Fatalf("an unknown recipe version was accepted, status=%d", status)
	}

	// Single-Spark work is never delegated.
	if status, _ := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("pull_image", "job-evil", fixture.single)); status != http.StatusBadRequest {
		t.Fatalf("single-Spark delegation status=%d", status)
	}
}

// stubFabricProbe replaces live detection for the length of a test. CI
// runners can expose a real RDMA device, so a test about this endpoint has to
// fake the machine or it is testing the runner it happens to land on.
func stubFabricProbe(t *testing.T, probe operations.FabricProbe, err error) {
	t.Helper()
	previous := operations.ServeFabricProbe
	t.Cleanup(func() { operations.ServeFabricProbe = previous })
	operations.ServeFabricProbe = func(recipe.Recipe) (operations.FabricProbe, error) { return probe, err }
}

// The worker's half of the cable check: it says which port it detected, the
// address that port holds, and where it is listening for the head's dial.
func TestWorkerReportsWhereOnTheCableItCanBeMet(t *testing.T) {
	fixture := newNodeFixture(t)
	stubFabricProbe(t, operations.FabricProbe{Interface: "enP2p1s0f1np1", HCA: "roceP2p1s0f1", Address: "169.254.37.4", Port: 45111, Token: "abc123"}, nil)

	status, body := fixture.post(t, "/api/v1/internal/node/fabric", fixture.key, map[string]any{"recipe": fixture.distributed})
	if status != http.StatusOK {
		t.Fatalf("cable status=%d body=%#v", status, body)
	}
	for key, want := range map[string]any{
		"interface": "enP2p1s0f1np1", "hca": "roceP2p1s0f1", "address": "169.254.37.4", "port": float64(45111), "token": "abc123",
	} {
		if body[key] != want {
			t.Errorf("body[%s] = %v, want %v", key, body[key], want)
		}
	}
}

// A worker that cannot join the cable answers with the reason, not a
// transport failure, so the head records a real check receipt saying what
// this Spark reported.
func TestWorkerReportsWhyItCannotJoinTheCable(t *testing.T) {
	fixture := newNodeFixture(t)
	stubFabricProbe(t, operations.FabricProbe{}, errors.New("no high-speed port has link (rocep1s0f0/enp1s0f0np0: no link); connect the cable between the two Sparks"))

	status, body := fixture.post(t, "/api/v1/internal/node/fabric", fixture.key, map[string]any{"recipe": fixture.distributed})
	if status != http.StatusOK {
		t.Fatalf("cable status=%d body=%#v", status, body)
	}
	message, _ := body["error"].(string)
	if !strings.Contains(message, "connect the cable between the two Sparks") {
		t.Fatalf("the worker did not say why it cannot join the cable: %#v", body)
	}
	if body["address"] != nil {
		t.Fatalf("a failed probe reported an address anyway: %#v", body)
	}
}

// The endpoint carries the same guards as every other node endpoint: a fleet
// key, and only a two-Spark recipe this Spark itself holds.
func TestWorkerCableEndpointIsKeyOnlyAndCatalogued(t *testing.T) {
	fixture := newNodeFixture(t)
	stubFabricProbe(t, operations.FabricProbe{Interface: "enp1s0f1np1", Address: "169.254.37.4", Port: 45111, Token: "abc123"}, nil)

	if status, _ := fixture.post(t, "/api/v1/internal/node/fabric", "", map[string]any{"recipe": fixture.distributed}); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated cable check status=%d", status)
	}
	if status, _ := fixture.post(t, "/api/v1/internal/node/fabric", "not-a-key", map[string]any{"recipe": fixture.distributed}); status != http.StatusUnauthorized {
		t.Fatalf("bad-key cable check status=%d", status)
	}
	if status, _ := fixture.post(t, "/api/v1/internal/node/fabric", fixture.key, map[string]any{"recipe": fixture.single}); status != http.StatusBadRequest {
		t.Fatalf("a single-Spark recipe was accepted for a cable check, status=%d", status)
	}
	unknown := fixture.distributed
	unknown.ID = "not-in-this-catalogue"
	if status, _ := fixture.post(t, "/api/v1/internal/node/fabric", fixture.key, map[string]any{"recipe": unknown}); status != http.StatusBadRequest {
		t.Fatalf("an unknown recipe was accepted for a cable check, status=%d", status)
	}
}

func TestWorkerNodeEndpointsAreKeyOnlyAndAllowlisted(t *testing.T) {
	fixture := newNodeFixture(t)

	if status, _ := fixture.post(t, "/api/v1/internal/node/step", "", map[string]any{}); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated worker step status=%d", status)
	}
	if status, _ := fixture.post(t, "/api/v1/internal/node/step", "not-a-key", map[string]any{}); status != http.StatusUnauthorized {
		t.Fatalf("bad-key worker step status=%d", status)
	}

	status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-1"})
	if status != http.StatusOK || body["ready"] != true {
		t.Fatalf("worker preflight status=%d body=%#v", status, body)
	}
	// A worker rank publishes no HTTP port, so that check must not be run.
	for _, check := range body["checks"].([]any) {
		if check.(map[string]any)["operation"] == "verify_port" {
			t.Fatal("worker preflight checked the head's host port")
		}
	}

	// Policy steps stay local to each manager.
	if status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("verify_openai_inference", "job-1", fixture.distributed)); status != http.StatusBadRequest {
		t.Fatalf("off-allowlist worker step status=%d body=%#v", status, body)
	}

	// A head that forgets it is talking to a worker gets nothing.
	headRole := workerStep("pull_image", "job-1", fixture.distributed)
	headRole["placement"] = map[string]any{"role": "head", "node": "spark-a", "node_count": 2}
	if status, _ := fixture.post(t, "/api/v1/internal/node/step", fixture.key, headRole); status != http.StatusBadRequest {
		t.Fatalf("head-role worker step status=%d", status)
	}
}

// Whether a worker rank binds a host port is the runtime's answer. A vLLM
// worker is launched --headless and binds nothing, so a busy port on this
// machine is not its problem. An SGLang worker binds that same port here, so a
// busy port has to be reported now, while the head can still say which machine
// to clear, instead of an hour later as a head that will not start.
func TestWorkerChecksItsHostPortOnlyWhenItsRankBindsOne(t *testing.T) {
	fixture := newNodeFixture(t)
	fixture.localExec.failPort = true

	status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-1"})
	if status != http.StatusOK || body["ready"] != true {
		t.Fatalf("a headless vLLM worker was failed on the head's host port: status=%d body=%#v", status, body)
	}
	if checkFor(body, "verify_port") != nil {
		t.Fatal("worker preflight checked a port its vLLM rank never binds")
	}

	// Same machine, same busy port, a rank that does bind it.
	status, body = fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.sglang, "job_id": "job-1"})
	if status != http.StatusOK {
		t.Fatalf("sglang worker preflight status=%d body=%#v", status, body)
	}
	if body["ready"] != false {
		t.Fatalf("a worker whose port is taken reported itself ready: %#v", body)
	}
	check := checkFor(body, "verify_port")
	if check == nil {
		t.Fatal("the sglang worker never checked the port its rank binds")
	}
	if check["ok"] != false {
		t.Fatalf("the busy port was reported as available: %#v", check)
	}
	message, _ := check["error"].(string)
	if !strings.Contains(message, "8000") {
		t.Fatalf("the failure does not name the port: %q", message)
	}
}

// checkFor returns a named preflight check from a node response, or nil when
// the check was not run at all.
func checkFor(body map[string]any, operation string) map[string]any {
	checks, _ := body["checks"].([]any)
	for _, entry := range checks {
		check, _ := entry.(map[string]any)
		if check["operation"] == operation {
			return check
		}
	}
	return nil
}

// liveMemoryShortfall is what the shipped guard says on a GB10 whose memory is
// already held: the live check fails on a node whose device pool and system
// pool are the same memory. The message comes from the guard itself, so a test
// about what the owner reads is testing the copy that ships.
func liveMemoryShortfall(t *testing.T, node string, freeBytes int64) error {
	t.Helper()
	_, err := resourceguard.CheckMemory([]resourceguard.Node{{
		Name: node, SystemMemoryTotal: 128_000_000_000, SystemMemoryAvailable: freeBytes,
		GPUMemoryTotal: 128_000_000_000, GPUMemoryFree: freeBytes,
	}}, 1, resourceguard.MemoryPolicy{
		MinimumTotalBytes: 120_000_000_000, HostReserveBytes: 12_000_000_000,
		RuntimeBudgetBytes: 91_000_000_000, RequireLiveCapacity: true,
	})
	if err == nil {
		t.Fatal("the fixture's live memory check passed; it must fail")
	}
	return err
}

// nominalCapacityShortfall is the machine that is simply too small. It says
// nothing about what runs right now, so no running model can excuse it.
func nominalCapacityShortfall(t *testing.T, node string) error {
	t.Helper()
	_, err := resourceguard.CheckMemory([]resourceguard.Node{{
		Name: node, SystemMemoryTotal: 64_000_000_000, SystemMemoryAvailable: 60_000_000_000,
		GPUMemoryTotal: 64_000_000_000, GPUMemoryFree: 60_000_000_000,
	}}, 1, resourceguard.MemoryPolicy{
		MinimumTotalBytes: 120_000_000_000, HostReserveBytes: 12_000_000_000,
		RuntimeBudgetBytes: 40_000_000_000,
	})
	if err == nil {
		t.Fatal("the fixture's capacity check passed; it must fail")
	}
	return err
}

// servingContainer is the model this Spark runs right now.
func servingContainer(r recipe.Recipe) operations.ManagedContainer {
	return operations.ManagedContainer{
		Name: "basement-" + r.ID, Running: true, RecipeID: r.ID,
		Version: strconv.Itoa(r.Version), HostPort: r.Service.DefaultHostPort,
	}
}

// The two-Spark failure of 2026-08-29. One model served across both Sparks.
// The owner chose the switch flow for another model, and that plan stops the
// serving model before anything starts. Preflight still measures the worker's
// memory while the old model holds it, and the worker keeps no installed-model
// rows, so it could not name the holder and failed the deployment at Check
// system. The running container is the proof the worker does hold.
func TestWorkerForgivesLiveMemoryHeldByTheModelItStillRuns(t *testing.T) {
	fixture := newNodeFixture(t)
	fixture.localExec.fail("verify_memory", liveMemoryShortfall(t, "spark-b", 6_300_000_000))
	fixture.localExec.runContainer(servingContainer(fixture.single))

	status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-switch"})
	if status != http.StatusOK || body["ready"] != true {
		t.Fatalf("the worker refused a switch that stops the model holding its memory: status=%d body=%#v", status, body)
	}
	check := checkFor(body, "verify_memory")
	if check == nil || check["ok"] != true {
		t.Fatalf("worker live memory check=%#v", check)
	}
	receipt, _ := check["receipt"].(map[string]any)
	if receipt["occupied_by_managed_recipe"] != fixture.single.ID {
		t.Fatalf("the receipt does not name the model that holds the memory: %#v", receipt)
	}
}

// Nothing this manager owns holds the memory, so the failure stands. The owner
// then reads one sentence: on a GB10 the device pool and the system pool are
// the same memory, and reporting the same figure twice looked like two
// separate problems.
func TestWorkerRefusesLiveMemoryNoManagedModelHoldsAndSaysItOnce(t *testing.T) {
	fixture := newNodeFixture(t)
	fixture.localExec.fail("verify_memory", liveMemoryShortfall(t, "spark-b", 6_300_000_000))

	status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-switch"})
	if status != http.StatusOK || body["ready"] != false {
		t.Fatalf("a worker with no free memory reported itself ready: status=%d body=%#v", status, body)
	}
	check := checkFor(body, "verify_memory")
	if check == nil || check["ok"] != false {
		t.Fatalf("worker live memory check=%#v", check)
	}
	message, _ := check["error"].(string)
	if !strings.Contains(message, "spark-b has 6.3 GB of memory free; the model needs 103 GB (91.0 GB to run and 12.0 GB system reserve)") {
		t.Fatalf("the shortfall does not read as one plain sentence: %q", message)
	}
	if strings.Count(message, "6.3 GB") != 1 || strings.Contains(message, "free GPU memory") {
		t.Fatalf("the same memory is reported twice: %q", message)
	}
}

// A machine that is too small for the model is never forgiven. Nominal
// capacity does not change when a model stops, so a running container says
// nothing about it.
func TestWorkerNeverForgivesAMachineTooSmallForTheModel(t *testing.T) {
	fixture := newNodeFixture(t)
	fixture.localExec.fail("verify_memory_capacity", nominalCapacityShortfall(t, "spark-b"))
	fixture.localExec.runContainer(servingContainer(fixture.single))

	status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-switch"})
	if status != http.StatusOK || body["ready"] != false {
		t.Fatalf("a Spark too small for the model reported itself ready: status=%d body=%#v", status, body)
	}
	check := checkFor(body, "verify_memory_capacity")
	if check == nil || check["ok"] != false {
		t.Fatalf("worker capacity check=%#v", check)
	}
	if receipt, _ := check["receipt"].(map[string]any); receipt["occupied_by_managed_recipe"] != nil {
		t.Fatalf("a running model excused a machine that is too small: %#v", receipt)
	}
}

// The plan's own memory check runs after the previous model stops, so it must
// stay strict. A running managed container excuses nothing here: at this point
// the memory has to be free.
func TestThePlanStepMemoryCheckIsNeverForgiven(t *testing.T) {
	fixture := newNodeFixture(t)
	fixture.localExec.fail("verify_memory", liveMemoryShortfall(t, "spark-b", 6_300_000_000))
	fixture.localExec.runContainer(servingContainer(fixture.single))

	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("verify_memory", "job-switch", fixture.distributed))
	if status != http.StatusOK {
		t.Fatalf("worker step status=%d body=%#v", status, body)
	}
	message, _ := body["error"].(string)
	if message == "" {
		t.Fatalf("the plan's memory check was forgiven while a model still held the memory: %#v", body)
	}
	if !strings.Contains(message, "of memory free") {
		t.Fatalf("the step did not report the live shortfall: %q", message)
	}
}

// The same invariant, held against every shape a pardon could take. A pardon
// belongs to preflight and to nothing else, so whatever the delegated step is,
// the executor's own answer is what the head records, word for word. A model of
// another recipe running on this node is the one excuse preflight accepts, so
// that is the state each step is failed under here: a pardon written for a
// single operation would pass every other test in this file and be caught only
// by its own row below.
func TestADelegatedStepNeverForgivesAnExecutorFailure(t *testing.T) {
	for _, operation := range []string{
		"verify_memory", "pull_image", "download_artifact", "write_generated_config",
		"create_container", "start_container", "stop_container", "remove_container",
		"remove_artifact_if_unshared",
	} {
		t.Run(operation, func(t *testing.T) {
			fixture := newNodeFixture(t)
			fixture.localExec.runContainer(servingContainer(fixture.single))
			reason := "the executor refused " + operation + " on this Spark"
			fixture.localExec.fail(operation, errors.New(reason))

			status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep(operation, "job-switch", fixture.distributed))
			if status != http.StatusOK {
				t.Fatalf("worker step status=%d body=%#v", status, body)
			}
			message, _ := body["error"].(string)
			if message != reason {
				t.Fatalf("the step reported %q, want the executor's own words %q", message, reason)
			}
			if body["receipt"] != nil {
				t.Fatalf("a failed step returned a receipt anyway: %#v", body["receipt"])
			}
		})
	}
}

// A stopped container holds no memory, so it excuses nothing. The pardon rests
// on the model this node runs right now, and a container it once ran is not
// that model.
func TestAStoppedManagedModelDoesNotExcuseAFullMachine(t *testing.T) {
	fixture := newNodeFixture(t)
	fixture.localExec.fail("verify_memory", liveMemoryShortfall(t, "spark-b", 6_300_000_000))
	stopped := servingContainer(fixture.single)
	stopped.Running = false
	fixture.localExec.runContainer(stopped)

	status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-switch"})
	if status != http.StatusOK || body["ready"] != false {
		t.Fatalf("a stopped container excused a machine with no memory free: status=%d body=%#v", status, body)
	}
	check := checkFor(body, "verify_memory")
	if check == nil || check["ok"] != false {
		t.Fatalf("worker live memory check=%#v", check)
	}
	if message, _ := check["error"].(string); !strings.Contains(message, "of memory free") {
		t.Fatalf("the check did not report the live shortfall: %q", message)
	}
	if receipt, _ := check["receipt"].(map[string]any); receipt["occupied_by_managed_recipe"] != nil {
		t.Fatalf("a stopped model was named as the holder of this node's memory: %#v", receipt)
	}
}

func TestWorkerAdmitsOneDelegatedJobAtATime(t *testing.T) {
	fixture := newNodeFixture(t)
	if status, _ := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-a"}); status != http.StatusOK {
		t.Fatalf("first head was refused, status=%d", status)
	}
	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("pull_image", "job-b", fixture.distributed))
	if status != http.StatusConflict {
		t.Fatalf("a second head was admitted while another job holds this Spark, status=%d body=%#v", status, body)
	}
	// The holder keeps working.
	if status, _ := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("pull_image", "job-a", fixture.distributed)); status != http.StatusOK {
		t.Fatalf("the lease holder was blocked by its own lease, status=%d", status)
	}
	// Teardown hands this Spark back.
	if status, _ := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("stop_container", "job-a", fixture.distributed)); status != http.StatusOK {
		t.Fatalf("teardown status=%d", status)
	}
	if status, _ := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("pull_image", "job-b", fixture.distributed)); status != http.StatusOK {
		t.Fatalf("the next head was not admitted after teardown, status=%d", status)
	}
}

func TestWorkerReservationExpiresSoADeadHeadCannotWedgeThisSpark(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-dead"}); status != http.StatusOK || body["ready"] != true {
		t.Fatalf("dead head preflight status=%d body=%#v", status, body)
	}
	if status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("start_container", "job-dead", fixture.distributed)); status != http.StatusOK || body["error"] != nil {
		t.Fatalf("dead head start status=%d body=%#v", status, body)
	}
	if !fixture.localExec.isRunning() {
		t.Fatal("the worker container never started")
	}
	allocator := fixture.api.engine.Reservations()
	reservationID := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-dead", fixture.distributed.ID, fixture.distributed.Version)
	if err := allocator.Renew(ctx, reservationID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.api.ReclaimExpiredDriverReservations(ctx); err != nil {
		t.Fatal(err)
	}
	if fixture.localExec.isRunning() || !fixture.localExec.executed("stop_container") {
		t.Fatal("the expired worker reservation was freed without stopping its container")
	}
	expired, err := allocator.Reservation(ctx, reservationID)
	if err != nil || expired.State != "expired" {
		t.Fatalf("dead head reservation=%+v err=%v", expired, err)
	}

	// A different driver can use the worker after cleanup, then hand it back
	// so the worker's own local manager can claim the same runtime slot.
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-next"}); status != http.StatusOK || body["ready"] != true {
		t.Fatalf("next driver preflight status=%d body=%#v", status, body)
	}
	if status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("stop_container", "job-next", fixture.distributed)); status != http.StatusOK || body["error"] != nil {
		t.Fatalf("next driver release status=%d body=%#v", status, body)
	}
	localID := fleet.ReservationID(fleet.ClaimKindLocalJob, "local-after-dead-head")
	if _, _, err := allocator.Prepare(ctx, fleet.ReservationRequest{
		ReservationID: localID, DeploymentID: "job:local-after-dead-head", DriverNodeID: allocator.NodeID(),
		RecipeID: fixture.single.ID, RecipeVersion: fixture.single.Version,
		Claims:       fleet.Claims{Version: fleet.ClaimsVersion, Kind: fleet.ClaimKindLocalJob, Runtime: true},
		PrepareToken: fleet.LocalPrepareToken(localID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.Commit(ctx, localID, fleet.LocalPrepareToken(localID), []byte(`{"kind":"local-engine"}`)); err != nil {
		t.Fatal(err)
	}
	if err := allocator.Activate(ctx, localID, ""); err != nil {
		t.Fatalf("local runtime remained wedged after dead head cleanup: %v", err)
	}
}

// The worker half of the 2026-08-29 wedge. This Spark held an active rank for
// the head's serve job. The head restarted, that job died with it, but its
// surviving reservation row kept the sweep renewing this rank, so the recovery
// start the head launched next was refused by its own worker and burned its
// retry budget against a conflict that could never clear. Once the head stops
// renewing a dead deployment, the ordinary lease deadline is all this node
// needs to hand itself to the recovery job. Each recovery attempt is a new
// job, so it asks under a job id of its own.
func TestARecoveryStartIsAdmittedOnceTheDeadHeadStopsRenewing(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-before-the-upgrade"}); status != http.StatusOK || body["ready"] != true {
		t.Fatalf("serving head preflight status=%d body=%#v", status, body)
	}
	if status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("start_container", "job-before-the-upgrade", fixture.distributed)); status != http.StatusOK || body["error"] != nil {
		t.Fatalf("serving head start status=%d body=%#v", status, body)
	}

	// The restarted head launches its recovery start. While the dead job's
	// rank still holds a lease, this Spark can only refuse it.
	if status, _ := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-reconcile-first"}); status != http.StatusConflict {
		t.Fatalf("the recovery start was admitted while a leased rank still held this Spark, status=%d", status)
	}
	if err := fixture.api.ReclaimExpiredDriverReservations(ctx); err != nil {
		t.Fatal(err)
	}
	if !fixture.localExec.isRunning() {
		t.Fatal("a rank whose lease is still live was reclaimed")
	}

	// Nothing renews a dead deployment any more, so the lease lapses and the
	// sweep takes this node back.
	allocator := fixture.api.engine.Reservations()
	reservationID := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-before-the-upgrade", fixture.distributed.ID, fixture.distributed.Version)
	if err := allocator.Renew(ctx, reservationID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.api.ReclaimExpiredDriverReservations(ctx); err != nil {
		t.Fatal(err)
	}
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-reconcile-next"}); status != http.StatusOK || body["ready"] != true {
		t.Fatalf("the recovery start was still locked out after the dead lease expired: status=%d body=%#v", status, body)
	}
}

// The live two-Spark failure of 2026-08-28: the head activated this worker's
// rank at preflight, then staged an image and weights for longer than the
// nine-heartbeat lease, so the sweep reclaimed the rank and settled the row.
// The rank's identity is deterministic for one job and recipe, so the head's
// next step found that dead row, Prepare returned it unchanged, and Activate
// refused it — "this node's runtime is already reserved by another
// deployment" — for the deployment that owned it. A settled row holds no
// claim, so the same job must be able to take its own Spark back.
func TestReclaimedWorkerRankDoesNotWedgeItsOwnJob(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-staging"}); status != http.StatusOK || body["ready"] != true {
		t.Fatalf("worker preflight status=%d body=%#v", status, body)
	}
	allocator := fixture.api.engine.Reservations()
	reservationID := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-staging", fixture.distributed.ID, fixture.distributed.Version)

	// The head stages for longer than the lease and never renews it.
	if err := allocator.Renew(ctx, reservationID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.api.ReclaimExpiredDriverReservations(ctx); err != nil {
		t.Fatal(err)
	}
	settled, err := allocator.Reservation(ctx, reservationID)
	if err != nil || settled.State != "expired" {
		t.Fatalf("reclaimed rank=%+v err=%v, want expired", settled, err)
	}

	// The head comes back with the first staging step of that same job.
	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("pull_image", "job-staging", fixture.distributed))
	if status != http.StatusOK {
		t.Fatalf("the deployment was locked out of the Spark it owns: status=%d body=%#v", status, body)
	}
	if body["error"] != nil {
		t.Fatalf("worker step reported %v", body["error"])
	}
	if !fixture.localExec.executed("pull_image") {
		t.Fatal("the worker never pulled the image for the job that owns it")
	}
	claimed, err := allocator.Reservation(ctx, reservationID)
	if err != nil || claimed.State != "active" {
		t.Fatalf("the rank was not claimed fresh: %+v err=%v", claimed, err)
	}

	// Clearing a dead row is not an amnesty: another head is still refused
	// while this one holds the Spark.
	if status, _ := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("pull_image", "job-other", fixture.distributed)); status != http.StatusConflict {
		t.Fatalf("a second head was admitted while this job holds the Spark, status=%d", status)
	}
}

// servingRank leaves this node exactly as a serving two-Spark model leaves it:
// the head took the rank at preflight, its rank container runs here, and the
// claim stays active for as long as the head renews it. It answers with the
// reservation the model serves under.
func servingRank(t *testing.T, fixture nodeFixture, jobID string, r recipe.Recipe) string {
	t.Helper()
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": r, "job_id": jobID}); status != http.StatusOK || body["ready"] != true {
		t.Fatalf("serving head preflight status=%d body=%#v", status, body)
	}
	if status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("start_container", jobID, r)); status != http.StatusOK || body["error"] != nil {
		t.Fatalf("serving head start status=%d body=%#v", status, body)
	}
	allocator := fixture.api.engine.Reservations()
	reservationID := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), jobID, r.ID, r.Version)
	serving, err := allocator.Reservation(context.Background(), reservationID)
	if err != nil || serving.State != "active" {
		t.Fatalf("serving rank=%+v err=%v", serving, err)
	}
	return reservationID
}

// The two-Spark stop of 2026-08-29. GLM 5.3 Flash served across both Sparks.
// The owner clicked Stop, the head stopped its own rank cleanly, and the
// delegated stop died on this node: "this node's runtime is already reserved by
// another deployment". A rank identity is per job, the serving rank belongs to
// the job that STARTED the model, and a stop arrives under a job of its own, so
// the stop had to take the slot in order to free it and this Spark refused its
// own head. The model was left half dead: the head rank exited, the worker rank
// still holding its memory, and the slot locked against everything after it.
//
// The switch flow arrives here the same way. The plan of the model being
// started delegates the stop of the model being replaced, so the job id belongs
// to the new install and the step carries the recipe being stopped.
func TestADelegatedStopAdoptsTheServingRankInsteadOfCompetingWithIt(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()
	serveID := servingRank(t, fixture, "job-serve", fixture.distributed)

	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("stop_container", "job-stop", fixture.distributed))
	if status != http.StatusOK || body["error"] != nil {
		t.Fatalf("the delegated stop was refused by the rank it exists to free: status=%d body=%#v", status, body)
	}
	if fixture.localExec.isRunning() || !fixture.localExec.executed("stop_container") {
		t.Fatal("the worker rank was never stopped")
	}
	if used := fixture.localExec.reservationFor("stop_container"); used != serveID {
		t.Fatalf("the stop ran under %q, want the serving rank %q", used, serveID)
	}
	handedBack, err := allocator.Reservation(ctx, serveID)
	if err != nil || handedBack.State != "released" {
		t.Fatalf("the serving rank was not handed back: %+v err=%v", handedBack, err)
	}
	// No second claim was raised against it under the stop job's own identity.
	stopID := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-stop", fixture.distributed.ID, fixture.distributed.Version)
	if _, err := allocator.Reservation(ctx, stopID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stop raised a competing claim on this Spark: err=%v", err)
	}
	// The Spark is free for the next deployment, which is what a stop is for.
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-after"}); status != http.StatusOK || body["ready"] != true {
		t.Fatalf("the slot stayed locked after the stop: status=%d body=%#v", status, body)
	}
}

// The switch shape of the same failure: the arriving job installs another
// model, and the step it delegates stops the model that serves here now. The
// job id belongs to a different recipe entirely, so nothing about it can be
// matched against the rank being handed back except the recipe of the step.
func TestASwitchStopsTheServingModelUnderTheNewInstallJob(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()
	serveID := servingRank(t, fixture, "job-serve", fixture.distributed)

	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("stop_container", "job-install-inkling", fixture.distributed))
	if status != http.StatusOK || body["error"] != nil {
		t.Fatalf("the switch could not stop the model it replaces: status=%d body=%#v", status, body)
	}
	if used := fixture.localExec.reservationFor("stop_container"); used != serveID {
		t.Fatalf("the switch stop ran under %q, want the serving rank %q", used, serveID)
	}
	handedBack, err := allocator.Reservation(ctx, serveID)
	if err != nil || handedBack.State != "released" {
		t.Fatalf("the replaced model's rank was not handed back: %+v err=%v", handedBack, err)
	}
}

// A stop of a model this node does not hold a rank for is still a stop. There
// is nothing to hand back, so nothing is claimed either: a releasing step frees
// this machine and must never take the slot in order to do it.
func TestADelegatedStopWithNoRankToHandBackStillStops(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()

	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("stop_container", "job-stop-again", fixture.distributed))
	if status != http.StatusOK || body["error"] != nil {
		t.Fatalf("a repeated stop was refused: status=%d body=%#v", status, body)
	}
	if !fixture.localExec.executed("stop_container") {
		t.Fatal("the repeated stop never reached this node's own executor")
	}
	if used := fixture.localExec.reservationFor("stop_container"); used != "" {
		t.Fatalf("a stop with nothing to hand back claimed %q", used)
	}
	stopID := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-stop-again", fixture.distributed.ID, fixture.distributed.Version)
	if _, err := allocator.Reservation(ctx, stopID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stop with nothing to hand back left a claim behind: err=%v", err)
	}
}

// A staging step can be the first thing a job asks of this node: its check may
// have run before a restart, or its rank may have lapsed while the head staged
// its own copy. Such a step takes the same claim the check takes, so it has to
// be able to name the same model.
func TestAWorkerAdmitsAStagingStepThatNamesTheModelItReplaces(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()
	serveID := servingRank(t, fixture, "job-serve", fixture.distributed)

	step := workerStepReplacing("pull_image", "job-install-inkling", fixture.sglang, fixture.distributed.ID)
	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, step)
	if status != http.StatusOK || body["error"] != nil {
		t.Fatalf("a staging step naming the model it replaces was refused: status=%d body=%#v", status, body)
	}
	replaced, err := allocator.Reservation(ctx, serveID)
	if err != nil || replaced.State != "released" {
		t.Fatalf("the replaced model kept its claim on this node: %+v err=%v", replaced, err)
	}
	arrived := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-install-inkling", fixture.sglang.ID, fixture.sglang.Version)
	claimed, err := allocator.Reservation(ctx, arrived)
	if err != nil || claimed.State != "active" {
		t.Fatalf("the arriving model did not take this node's rank: %+v err=%v", claimed, err)
	}
}

// A rank is handed back because it stopped, never because a stop was tried. A
// rank whose container is still up must keep its claim: this node is occupied,
// and it is the claim that says so. Releasing it would admit a second model
// onto memory the first one still holds, and it would put that rank out of
// reach of the sweep for good, because only an active or reclaiming row is ever
// reclaimed.
func TestAFailedDelegatedStopKeepsTheRankItCouldNotHandBack(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()
	serveID := servingRank(t, fixture, "job-serve", fixture.distributed)
	fixture.localExec.fail("stop_container", errors.New("the container manager refused to stop this rank"))

	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("stop_container", "job-stop", fixture.distributed))
	if status != http.StatusOK {
		t.Fatalf("worker step status=%d body=%#v", status, body)
	}
	if message, _ := body["error"].(string); message == "" {
		t.Fatalf("a stop that failed was reported as a success: %#v", body)
	}
	held, err := allocator.Reservation(ctx, serveID)
	if err != nil || held.State != "active" {
		t.Fatalf("a rank that never stopped was handed back anyway: %+v err=%v", held, err)
	}
	// This node stays closed while that rank runs.
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.sglang, "job_id": "job-install-inkling"}); status != http.StatusConflict {
		t.Fatalf("another model was admitted onto a rank that is still running: status=%d body=%#v", status, body)
	}

	// The head's teardown sends the stop again. It adopts the very same rank,
	// and this time the rank really does go back.
	fixture.localExec.succeed("stop_container")
	status, body = fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("stop_container", "job-stop", fixture.distributed))
	if status != http.StatusOK || body["error"] != nil {
		t.Fatalf("the retried stop was refused: status=%d body=%#v", status, body)
	}
	if used := fixture.localExec.reservationFor("stop_container"); used != serveID {
		t.Fatalf("the retry ran under %q, want the serving rank %q", used, serveID)
	}
	handedBack, err := allocator.Reservation(ctx, serveID)
	if err != nil || handedBack.State != "released" {
		t.Fatalf("the stopped rank was not handed back: %+v err=%v", handedBack, err)
	}
}

// The rank belongs to the model, not to the version of it that took the rank.
// A managed container is named after the recipe id alone, so a rank an earlier
// version left running is the rank this stop is about, and a stop that walked
// past it would leave this node holding memory nobody can free.
func TestADelegatedStopAdoptsTheRankOfAnyVersionOfTheSameModel(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()

	// A rank this node took for another version of the model being stopped.
	otherVersion := fixture.distributed.Version + 1
	rankID := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-serve-other-version", fixture.distributed.ID, otherVersion)
	if _, _, err := allocator.Prepare(ctx, fleet.ReservationRequest{
		ReservationID: rankID, DeploymentID: "legacy-rank:job-serve-other-version", DriverNodeID: "legacy-head",
		RecipeID: fixture.distributed.ID, RecipeVersion: otherVersion,
		Claims:       fleet.Claims{Version: fleet.ClaimsVersion, Kind: fleet.ClaimKindLegacyRank, Runtime: true},
		PrepareToken: fleet.LocalPrepareToken(rankID), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.Commit(ctx, rankID, fleet.LocalPrepareToken(rankID), []byte(`{"kind":"legacy-rank-compatibility"}`)); err != nil {
		t.Fatal(err)
	}
	if err := allocator.Activate(ctx, rankID, ""); err != nil {
		t.Fatal(err)
	}

	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("stop_container", "job-stop", fixture.distributed))
	if status != http.StatusOK || body["error"] != nil {
		t.Fatalf("the stop was refused by a rank of the model it stops: status=%d body=%#v", status, body)
	}
	if used := fixture.localExec.reservationFor("stop_container"); used != rankID {
		t.Fatalf("the stop ran under %q, want the rank of the other version %q", used, rankID)
	}
	handedBack, err := allocator.Reservation(ctx, rankID)
	if err != nil || handedBack.State != "released" {
		t.Fatalf("the rank of the other version was not handed back: %+v err=%v", handedBack, err)
	}
}

// The recipe is the boundary. One model serves across both Sparks while a stop
// of another model arrives here, and that stop must not touch the serving
// model's rank. Freeing it would leave this node's memory owned by nothing
// while the model that holds it keeps running.
func TestADelegatedStopNeverFreesAnotherModelsRank(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()
	serveID := servingRank(t, fixture, "job-serve-qwen", fixture.distributed)

	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("stop_container", "job-stop-inkling", fixture.sglang))
	if status != http.StatusOK || body["error"] != nil {
		t.Fatalf("stopping another model status=%d body=%#v", status, body)
	}
	if used := fixture.localExec.reservationFor("stop_container"); used != "" {
		t.Fatalf("a stop of %s ran under the claim %q, which belongs to another model", fixture.sglang.ID, used)
	}
	held, err := allocator.Reservation(ctx, serveID)
	if err != nil || held.State != "active" {
		t.Fatalf("another model's stop freed the serving rank: %+v err=%v", held, err)
	}
}

// Only a releasing step adopts. Everything that puts a rank in place still
// takes the slot for itself, so a second deployment is refused for as long as
// this one holds this Spark.
func TestADelegatedBringUpStepStillRefusesAnotherDeploymentsRank(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()
	serveID := servingRank(t, fixture, "job-serve", fixture.distributed)

	for _, operation := range []string{"verify_memory", "pull_image", "download_artifact", "write_generated_config", "create_container", "start_container"} {
		status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep(operation, "job-other", fixture.distributed))
		if status != http.StatusConflict {
			t.Fatalf("%s was admitted while another deployment holds this Spark: status=%d body=%#v", operation, status, body)
		}
	}
	held, err := allocator.Reservation(ctx, serveID)
	if err != nil || held.State != "active" {
		t.Fatalf("a refused step disturbed the serving rank: %+v err=%v", held, err)
	}
}

// A deployment that names no model to replace is refused for as long as one
// serves here, and so is a deployment that names a model this node does not
// hold. This is the plain install: nothing about it says this node's slot is
// free, so it gets the answer it has always got. Only a job whose own plan
// stops the model that serves may say so, and it has to say which model
// (TestAWorkerAdmitsADeploymentThatNamesTheModelItReplaces).
//
// The memory check is not the reason for the refusal. The model that serves
// holds the memory and this node forgives that at a check, so what answers here
// is the reservation and nothing else.
func TestAWorkerRefusesADeploymentThatNamesNoModelToReplace(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()
	serveID := servingRank(t, fixture, "job-serve", fixture.distributed)
	// The model that serves really runs here, and it really holds the memory.
	fixture.localExec.runContainer(servingContainer(fixture.distributed))
	fixture.localExec.fail("verify_memory", liveMemoryShortfall(t, "spark-b", 6_300_000_000))

	for _, named := range []string{"", "a-model-this-node-does-not-hold"} {
		check := map[string]any{"recipe": fixture.sglang, "job_id": "job-install-inkling", "replaces_recipe_id": named}
		status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, check)
		if status != http.StatusConflict {
			t.Fatalf("a check naming %q was admitted while another model serves: status=%d body=%#v", named, status, body)
		}
		message, _ := body["error"].(string)
		if !strings.Contains(message, "already reserved by another deployment") {
			t.Fatalf("the check was refused for another reason than the runtime slot: %q", message)
		}
		if strings.Contains(message, "of memory free") {
			t.Fatalf("the memory check refused the deployment, which the pardon must prevent: %q", message)
		}
		// Every staging step is refused for the same reason, and the plan runs
		// those before any stop too.
		for _, operation := range []string{"pull_image", "download_artifact", "write_generated_config"} {
			step := workerStepReplacing(operation, "job-install-inkling", fixture.sglang, named)
			if status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, step); status != http.StatusConflict {
				t.Fatalf("%s naming %q was admitted: status=%d body=%#v", operation, named, status, body)
			}
		}
	}
	// A refused deployment changes nothing. The model that serves keeps its rank.
	held, err := allocator.Reservation(ctx, serveID)
	if err != nil || held.State != "active" {
		t.Fatalf("a refused deployment disturbed the serving rank: %+v err=%v", held, err)
	}
}

// The owner's switch, from the check onward. The plan asks this node to check
// itself before it stops the model it replaces, so a check has to be able to
// take a slot that model holds. The head names the model, and this node hands
// the slot over there and then.
//
// That is early, and the whole timeline has to be read as one thing: the CLAIM
// moves at the check, the CONTAINER moves at the stop, and an hour of
// downloading can sit between them. Nothing may assume the two happen together.
// What the claim buys is admission for the arriving model; what stops the model
// that serves is still the plan, and until it runs the old rank keeps running.
func TestAWorkerAdmitsADeploymentThatNamesTheModelItReplaces(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()
	serveID := servingRank(t, fixture, "job-serve", fixture.distributed)
	fixture.localExec.runContainer(servingContainer(fixture.distributed))
	fixture.localExec.fail("verify_memory", liveMemoryShortfall(t, "spark-b", 6_300_000_000))

	check := map[string]any{"recipe": fixture.sglang, "job_id": "job-install-inkling", "replaces_recipe_id": fixture.distributed.ID}
	status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, check)
	if status != http.StatusOK || body["ready"] != true {
		t.Fatalf("this Spark refused a check that names the model it replaces: status=%d body=%#v", status, body)
	}
	replaced, err := allocator.Reservation(ctx, serveID)
	if err != nil || replaced.State != "released" {
		t.Fatalf("the replaced model kept its claim on this node: %+v err=%v", replaced, err)
	}
	arrived := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-install-inkling", fixture.sglang.ID, fixture.sglang.Version)
	claimed, err := allocator.Reservation(ctx, arrived)
	if err != nil || claimed.State != "active" {
		t.Fatalf("the arriving model did not take this node's rank: %+v err=%v", claimed, err)
	}
	// The claim moved. The model has not: nothing stopped it here.
	if fixture.localExec.executed("stop_container") || !fixture.localExec.isRunning() {
		t.Fatal("a check stopped the model it replaces; only the plan may do that")
	}

	// The plan's own stop arrives later, after the staging this node was just
	// admitted for. Its rank claim is already gone, so it adopts nothing, and it
	// still stops the model and leaves the arriving model's rank alone.
	status, body = fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStepReplacing("stop_container", "job-install-inkling", fixture.distributed, fixture.distributed.ID))
	if status != http.StatusOK || body["error"] != nil {
		t.Fatalf("the plan could not stop the model it replaced: status=%d body=%#v", status, body)
	}
	if used := fixture.localExec.reservationFor("stop_container"); used != "" {
		t.Fatalf("the stop ran under %q, but its rank claim was handed over at the check", used)
	}
	if !fixture.localExec.executed("stop_container") {
		t.Fatal("the replaced model was never stopped on this node")
	}
	stillClaimed, err := allocator.Reservation(ctx, arrived)
	if err != nil || stillClaimed.State != "active" {
		t.Fatalf("stopping the replaced model disturbed the arriving model's rank: %+v err=%v", stillClaimed, err)
	}
}

// The owner's click, replayed on this node from the stop onward. This is every
// step of the switch that this Spark answers correctly today: the delegated
// stop of the model being replaced, this node's own check for the model that
// arrives, and that model's rank going up. It starts at the stop because the
// check in front of the stop is refused, which the test above records.
//
// Read together the two tests say exactly one thing is missing: this node must
// admit a check that names the model it replaces. Everything after that point
// already works.
func TestAWorkerCompletesTheSwitchOnceTheDelegatedStopHasRun(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()
	serveID := servingRank(t, fixture, "job-serve", fixture.distributed)

	// The plan of the model that arrives delegates the stop of the model that
	// serves, so the job id is the new install's and the recipe is the old.
	status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("stop_container", "job-install-inkling", fixture.distributed))
	if status != http.StatusOK || body["error"] != nil {
		t.Fatalf("the switch could not stop the model it replaces: status=%d body=%#v", status, body)
	}
	if used := fixture.localExec.reservationFor("stop_container"); used != serveID {
		t.Fatalf("the stop ran under %q, want the serving rank %q", used, serveID)
	}
	handedBack, err := allocator.Reservation(ctx, serveID)
	if err != nil || handedBack.State != "released" {
		t.Fatalf("the replaced model's rank was not handed back: %+v err=%v", handedBack, err)
	}

	// The slot is free, so this node can now check itself for the new model.
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.sglang, "job_id": "job-install-inkling"}); status != http.StatusOK || body["ready"] != true {
		t.Fatalf("this Spark refused the model that replaces the one it stopped: status=%d body=%#v", status, body)
	}
	for _, operation := range []string{"pull_image", "create_container", "start_container"} {
		if status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep(operation, "job-install-inkling", fixture.sglang)); status != http.StatusOK || body["error"] != nil {
			t.Fatalf("%s for the new model failed: status=%d body=%#v", operation, status, body)
		}
	}
	if !fixture.localExec.isRunning() {
		t.Fatal("the new model's rank is not running on this node")
	}
	// The new model holds this node's rank under its own identity.
	arrived := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-install-inkling", fixture.sglang.ID, fixture.sglang.Version)
	claimed, err := allocator.Reservation(ctx, arrived)
	if err != nil || claimed.State != "active" {
		t.Fatalf("the new model did not take this node's rank: %+v err=%v", claimed, err)
	}
}

// A check that this node refuses aborts the claim it made for that job, and a
// rank identity is per job, so the same job meets its own dead row when it asks
// again. Nothing else clears it: the identity is the same bytes every time. So
// a job that was told "not yet" once could never be told "yes", and the answer
// it got was about the reservation, not about the machine.
//
// This is the guardrail half. The machine really is short of memory, the check
// says so, and the same job asks again once the memory is free.
func TestAWorkerCheckThatFailedIsAskedAgainByTheSameJob(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()
	identity := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-one", fixture.distributed.ID, fixture.distributed.Version)
	fixture.localExec.fail("verify_memory", liveMemoryShortfall(t, "spark-b", 6_300_000_000))

	status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-one"})
	if status != http.StatusOK || body["ready"] != false {
		t.Fatalf("a machine with no memory free was reported ready: status=%d body=%#v", status, body)
	}
	refused, err := allocator.Reservation(ctx, identity)
	if err != nil || refused.State != "aborted" {
		t.Fatalf("a refused check did not settle its own claim: %+v err=%v", refused, err)
	}

	// The memory is free now, and the job asks again under the same identity.
	fixture.localExec.succeed("verify_memory")
	status, body = fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-one"})
	if status != http.StatusOK || body["ready"] != true {
		t.Fatalf("the same job could never be admitted after one refusal: status=%d body=%#v", status, body)
	}
	admitted, err := allocator.Reservation(ctx, identity)
	if err != nil || admitted.State != "active" {
		t.Fatalf("the retried check did not take this node's rank: %+v err=%v", admitted, err)
	}
}

// The same trap on the other road to an aborted row. A check this node refuses
// for the runtime slot aborts its claim too, and a switch meets that refusal
// first: the head learns it must name the model it replaces, and asks again
// under the same job. Both of this node's abort paths have to leave an identity
// its own job can use again.
func TestARefusedSwitchCheckIsAskedAgainOnceItNamesWhatItReplaces(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	allocator := fixture.api.engine.Reservations()
	serveID := servingRank(t, fixture, "job-serve", fixture.distributed)
	fixture.localExec.runContainer(servingContainer(fixture.distributed))
	identity := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-install-inkling", fixture.sglang.ID, fixture.sglang.Version)

	status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.sglang, "job_id": "job-install-inkling"})
	if status != http.StatusConflict {
		t.Fatalf("a check naming nothing was admitted while a model serves: status=%d body=%#v", status, body)
	}
	refused, err := allocator.Reservation(ctx, identity)
	if err != nil || refused.State != "aborted" {
		t.Fatalf("a refused check did not settle its own claim: %+v err=%v", refused, err)
	}

	status, body = fixture.post(t, "/api/v1/internal/node/preflight", fixture.key,
		map[string]any{"recipe": fixture.sglang, "job_id": "job-install-inkling", "replaces_recipe_id": fixture.distributed.ID})
	if status != http.StatusOK || body["ready"] != true {
		t.Fatalf("the same job could not ask again after naming what it replaces: status=%d body=%#v", status, body)
	}
	admitted, err := allocator.Reservation(ctx, identity)
	if err != nil || admitted.State != "active" {
		t.Fatalf("the retried check did not take this node's rank: %+v err=%v", admitted, err)
	}
	replaced, err := allocator.Reservation(ctx, serveID)
	if err != nil || replaced.State != "released" {
		t.Fatalf("the replaced model kept its claim on this node: %+v err=%v", replaced, err)
	}
}

func TestWorkerReservationRenewalKeepsAHealthyHeadActive(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-live"}); status != http.StatusOK || body["ready"] != true {
		t.Fatalf("live head preflight status=%d body=%#v", status, body)
	}
	allocator := fixture.api.engine.Reservations()
	reservationID := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-live", fixture.distributed.ID, fixture.distributed.Version)
	if err := allocator.Renew(ctx, reservationID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	status, body := fixture.post(t, "/api/v1/internal/node/reservation/renew", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-live"})
	if status != http.StatusOK || body["expires_at"] == "" {
		t.Fatalf("worker renewal status=%d body=%#v", status, body)
	}
	if err := fixture.api.ReclaimExpiredDriverReservations(ctx); err != nil {
		t.Fatal(err)
	}
	active, err := allocator.Reservation(ctx, reservationID)
	if err != nil || active.State != "active" {
		t.Fatalf("renewed worker reservation=%+v err=%v", active, err)
	}
}

func TestWorkerRenewalAdoptsARankStartedByThePreviousManager(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-upgrade"}); status != http.StatusOK || body["ready"] != true {
		t.Fatalf("preflight status=%d body=%#v", status, body)
	}
	// The running container predates delegated_rank_liveness, so no row exists
	// even though the exact recipe container is healthy.
	fixture.localExec.mu.Lock()
	fixture.localExec.running = true
	fixture.localExec.mu.Unlock()
	allocator := fixture.api.engine.Reservations()
	reservationID := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-upgrade", fixture.distributed.ID, fixture.distributed.Version)
	if _, known, err := fixture.api.store.DelegatedRankRunning(ctx, reservationID); err != nil || known {
		t.Fatalf("pre-upgrade liveness known=%v err=%v", known, err)
	}

	status, body := fixture.post(t, "/api/v1/internal/node/reservation/renew", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-upgrade"})
	if status != http.StatusOK || body["expires_at"] == "" {
		t.Fatalf("upgrade adoption status=%d body=%#v", status, body)
	}
	if running, known, err := fixture.api.store.DelegatedRankRunning(ctx, reservationID); err != nil || !known || !running {
		t.Fatalf("adopted liveness running=%v known=%v err=%v", running, known, err)
	}
}

func TestWorkerRenewalRefusesARankThatStartedThenDied(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	if status, body := fixture.post(t, "/api/v1/internal/node/preflight", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-worker-died"}); status != http.StatusOK || body["ready"] != true {
		t.Fatalf("preflight status=%d body=%#v", status, body)
	}
	if status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("start_container", "job-worker-died", fixture.distributed)); status != http.StatusOK || body["error"] != nil {
		t.Fatalf("start status=%d body=%#v", status, body)
	}
	allocator := fixture.api.engine.Reservations()
	reservationID := fleet.ExactRecipeReservationID(fleet.ClaimKindLegacyRank, allocator.NodeID(), "job-worker-died", fixture.distributed.ID, fixture.distributed.Version)
	if running, known, err := fixture.api.store.DelegatedRankRunning(ctx, reservationID); err != nil || !known || !running {
		t.Fatalf("persisted rank liveness=(%v,%v,%v), want running known", running, known, err)
	}
	fixture.localExec.mu.Lock()
	fixture.localExec.running = false
	fixture.localExec.mu.Unlock()

	status, body := fixture.post(t, "/api/v1/internal/node/reservation/renew", fixture.key, map[string]any{"recipe": fixture.distributed, "job_id": "job-worker-died"})
	if status != http.StatusConflict || !strings.Contains(fmt.Sprint(body["error"]), "no longer running") {
		t.Fatalf("dead worker renewal status=%d body=%#v", status, body)
	}
}

func TestRenewalFailureToleratesABlipThenFailsAfterFreshness(t *testing.T) {
	fixture := newNodeFixture(t)
	base := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	fail := errors.New("worker model container is no longer running")
	var renewErr error = fail
	failed := 0
	fixture.api.renewDistributed = func(context.Context) error { return renewErr }
	fixture.api.recoverDistributed = func(_ context.Context, _ string, reason string, _ bool) error {
		if !strings.Contains(reason, "no longer running") {
			t.Fatalf("recovery reason=%q", reason)
		}
		failed++
		return nil
	}

	fixture.api.maintainDistributedServing(context.Background(), base)
	renewErr = nil
	fixture.api.maintainDistributedServing(context.Background(), base.Add(fleet.HeartbeatInterval))
	if failed != 0 || fixture.api.renewalFailures != 0 || !fixture.api.renewalFirstFailed.IsZero() {
		t.Fatalf("one recovered renewal was not treated as a transient blip: failed=%d failures=%d first=%s", failed, fixture.api.renewalFailures, fixture.api.renewalFirstFailed)
	}

	// Three failures alone are not enough: the head waits through the normal
	// heartbeat freshness window, still well ahead of the worker's 90s lease.
	renewErr = fail
	for _, at := range []time.Time{base.Add(2 * fleet.HeartbeatInterval), base.Add(3 * fleet.HeartbeatInterval), base.Add(4 * fleet.HeartbeatInterval)} {
		fixture.api.maintainDistributedServing(context.Background(), at)
	}
	if failed != 0 {
		t.Fatalf("failed before the freshness window elapsed: %d", failed)
	}
	fixture.api.maintainDistributedServing(context.Background(), base.Add(5*fleet.HeartbeatInterval))
	if failed != 1 {
		t.Fatalf("sustained renewal failures did not fail the group: %d", failed)
	}
}

func TestActiveDistributedHeadHealthFeedsTheRenewalFailureWindow(t *testing.T) {
	ctx := context.Background()
	fixture := newNodeFixture(t)
	if err := fixture.api.store.SetInstalled(ctx, store.InstalledModel{
		RecipeID: fixture.distributed.ID, RecipeVersion: fixture.distributed.Version,
		Status: "ready", ArtifactPath: "/managed/" + fixture.distributed.ID, Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	healthCalls, recovered := 0, 0
	fixture.api.renewDistributed = func(context.Context) error { return nil }
	fixture.api.headHealth = func(healthCtx context.Context, active recipe.Recipe) error {
		healthCalls++
		if active.ID != fixture.distributed.ID {
			t.Fatalf("health checked %s, want %s", active.ID, fixture.distributed.ID)
		}
		deadline, ok := healthCtx.Deadline()
		if !ok || time.Until(deadline) > distributedHeadHealthTimeout {
			t.Fatalf("head health context is not bounded: deadline=%s ok=%v", deadline, ok)
		}
		return errors.New("the active model did not answer its /health check")
	}
	fixture.api.recoverDistributed = func(_ context.Context, recipeID, reason string, _ bool) error {
		if recipeID != fixture.distributed.ID || !strings.Contains(reason, "/health") {
			t.Fatalf("recovery target=%q reason=%q", recipeID, reason)
		}
		recovered++
		return nil
	}
	for _, at := range []time.Time{base, base.Add(fleet.HeartbeatInterval), base.Add(2 * fleet.HeartbeatInterval), base.Add(3 * fleet.HeartbeatInterval)} {
		fixture.api.maintainDistributedServing(ctx, at)
	}
	if healthCalls != 4 || recovered != 1 {
		t.Fatalf("head health calls=%d recoveries=%d, want 4 and 1", healthCalls, recovered)
	}
}

func TestDistributedStagingDoesNotHealthFailTheHead(t *testing.T) {
	fixture := newNodeFixture(t)
	healthCalls := 0
	fixture.api.renewDistributed = func(context.Context) error { return nil }
	fixture.api.headHealth = func(context.Context, recipe.Recipe) error {
		healthCalls++
		return errors.New("must not health check staging")
	}
	fixture.api.maintainDistributedServing(context.Background(), time.Now())
	if healthCalls != 0 || fixture.api.renewalFailures != 0 {
		t.Fatalf("staging health calls=%d failures=%d", healthCalls, fixture.api.renewalFailures)
	}
}

func TestRecoveryJobRecordingRetriesAtABoundedHeartbeatPace(t *testing.T) {
	fixture := newNodeFixture(t)
	fixture.api.renewDistributed = func(context.Context) error { return nil }
	fixture.api.recoveryJobPending = true
	fixture.api.recoveryRecipeID = fixture.distributed.ID
	fixture.api.recoveryReason = "the worker model container is no longer running"
	fixture.api.recoveryJobAttempts = 1 // the original attempt already failed
	calls := 0
	fixture.api.recoverDistributed = func(_ context.Context, recipeID, reason string, retry bool) error {
		if recipeID != fixture.distributed.ID || !strings.Contains(reason, "no longer running") {
			t.Fatalf("retry target=%q reason=%q", recipeID, reason)
		}
		if !retry {
			t.Fatal("a recorded-job retry was not marked safe")
		}
		calls++
		return fmt.Errorf("%w: database temporarily unavailable", engine.ErrDistributedRecoveryJob)
	}
	base := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{base, base.Add(fleet.HeartbeatInterval), base.Add(2 * fleet.HeartbeatInterval)} {
		fixture.api.maintainDistributedServing(context.Background(), at)
	}
	if calls != distributedRecoveryJobAttemptLimit-1 || fixture.api.recoveryJobAttempts != distributedRecoveryJobAttemptLimit || !fixture.api.recoveryJobPending {
		t.Fatalf("record retries=%d attempts=%d pending=%v", calls, fixture.api.recoveryJobAttempts, fixture.api.recoveryJobPending)
	}
}

func TestWorkerReservationSurvivesRestartAndBlocksLocalRuntime(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	worker := fleet.NewAllocator(database, "node-worker")
	workerID := fleet.ReservationID(fleet.ClaimKindLegacyRank, "worker-job")
	request := fleet.ReservationRequest{ReservationID: workerID, DeploymentID: "legacy-rank:worker-job", RecipeID: "worker-recipe", RecipeVersion: 1,
		Claims: fleet.Claims{Version: fleet.ClaimsVersion, Kind: fleet.ClaimKindLegacyRank, Runtime: true}, PrepareToken: fleet.LocalPrepareToken(workerID), ExpiresAt: time.Now().Add(time.Hour)}
	if _, _, err := worker.Prepare(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Commit(ctx, workerID, fleet.LocalPrepareToken(workerID), []byte(`{"kind":"legacy-rank-compatibility"}`)); err != nil {
		t.Fatal(err)
	}
	if err := worker.Activate(ctx, workerID, ""); err != nil {
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
	restarted := fleet.NewAllocator(database, "node-worker")
	persisted, err := restarted.Reservation(ctx, workerID)
	if err != nil || persisted.State != "active" {
		t.Fatalf("worker reservation after restart=%+v err=%v", persisted, err)
	}
	localID := fleet.ReservationID(fleet.ClaimKindLocalJob, "local-job")
	if _, _, err := restarted.Prepare(ctx, fleet.ReservationRequest{ReservationID: localID, DeploymentID: "job:local-job", RecipeID: "local-recipe", RecipeVersion: 1,
		Claims: fleet.Claims{Version: fleet.ClaimsVersion, Kind: fleet.ClaimKindLocalJob, Runtime: true}, PrepareToken: fleet.LocalPrepareToken(localID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Commit(ctx, localID, fleet.LocalPrepareToken(localID), []byte(`{"kind":"local-engine"}`)); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Activate(ctx, localID, ""); !errors.Is(err, store.ErrReservationConflict) {
		t.Fatalf("competing local runtime claim error=%v", err)
	}
	if err := restarted.Release(ctx, workerID); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Activate(ctx, localID, ""); err != nil {
		t.Fatalf("local runtime remained blocked after worker release: %v", err)
	}
}

// TestDelegatedStepProgressIsReadableWhileItRuns is the worker half of live
// two-Spark progress: a delegated step runs outside this manager's engine
// and its call does not answer until it is over, so the head can only see a
// download move by reading this while the step runs.
func TestDelegatedStepProgressIsReadableWhileItRuns(t *testing.T) {
	fixture := newNodeFixture(t)

	// Nothing is running yet.
	status, body := fixture.post(t, "/api/v1/internal/node/step/progress", fixture.key, map[string]any{"job_id": "job-1"})
	if status != http.StatusOK || body["running"] != false {
		t.Fatalf("idle node reported status=%d body=%#v", status, body)
	}

	// The local executor reports one download receipt before it returns, so
	// the last receipt is readable once the step is over only if it is held
	// while the step runs.
	fixture.localExec.started = make(chan struct{})
	fixture.localExec.hold = make(chan struct{})
	polled := make(chan map[string]any, 1)
	go func() {
		<-fixture.localExec.started
		_, live := fixture.post(t, "/api/v1/internal/node/step/progress", fixture.key, map[string]any{"job_id": "job-1"})
		polled <- live
		close(fixture.localExec.hold)
	}()
	if status, body := fixture.post(t, "/api/v1/internal/node/step", fixture.key, workerStep("download_artifact", "job-1", fixture.distributed)); status != http.StatusOK || body["error"] != nil {
		t.Fatalf("worker download status=%d body=%#v", status, body)
	}
	live := <-polled
	if live["running"] != true || live["operation"] != "download_artifact" {
		t.Fatalf("a running delegated step was not reported: %#v", live)
	}
	receipt, ok := live["receipt"].(map[string]any)
	if !ok || receipt["bytes_complete"] != float64(100) || receipt["bytes_total"] != float64(100) {
		t.Fatalf("the running step's own numbers did not come through: %#v", live["receipt"])
	}

	// Progress belongs to the job that owns the step, and disappears with it.
	if _, other := fixture.post(t, "/api/v1/internal/node/step/progress", fixture.key, map[string]any{"job_id": "job-2"}); other["running"] != false {
		t.Fatalf("another head was told about this job's step: %#v", other)
	}
	if _, after := fixture.post(t, "/api/v1/internal/node/step/progress", fixture.key, map[string]any{"job_id": "job-1"}); after["running"] != false {
		t.Fatalf("a finished step is still reported as running: %#v", after)
	}

	// Same key-only rule as every other node endpoint.
	if status, _ := fixture.post(t, "/api/v1/internal/node/step/progress", "", map[string]any{"job_id": "job-1"}); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated progress read status=%d", status)
	}
}
