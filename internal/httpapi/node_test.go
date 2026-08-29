package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
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

func workerStep(operation, jobID string, r recipe.Recipe) map[string]any {
	return map[string]any{
		"operation": operation, "recipe": r, "job_id": jobID,
		"placement": map[string]any{"role": "worker", "node": "spark-b", "node_count": 2},
	}
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
