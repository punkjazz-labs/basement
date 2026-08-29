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
	"time"

	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
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
	renewals     []string
}

type gatedFleetExecutor struct {
	*fleetExecutor
	gateRecipe  string
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newGatedFleetExecutor(recipeID string) *gatedFleetExecutor {
	return &gatedFleetExecutor{
		fleetExecutor: newFleetExecutor(),
		gateRecipe:    recipeID,
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
	}
}

func (f *gatedFleetExecutor) Execute(ctx context.Context, execution operations.Execution, op recipe.Operation, r recipe.Recipe, progress operations.Progress) (map[string]any, error) {
	if op.Type == "download_artifact" && r.ID == f.gateRecipe {
		f.enterOnce.Do(func() { close(f.entered) })
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.fleetExecutor.Execute(ctx, execution, op, r, progress)
}

func (f *gatedFleetExecutor) unblock() {
	f.releaseOnce.Do(func() { close(f.release) })
}

func newFleetExecutor() *fleetExecutor {
	return &fleetExecutor{running: map[string]bool{}, peerReady: true}
}

// synchronizedDownloadExecutor holds both node downloads at one barrier.
// Reaching bothEntered proves neither call waited for the other to return.
type synchronizedDownloadExecutor struct {
	*fleetExecutor
	downloadMu  sync.Mutex
	entered     map[string]bool
	bothEntered chan struct{}
	release     chan struct{}
	failRole    string
	cancelled   map[string]bool
	jobIDs      map[string][]string
	resumeFrom  map[string]int64
}

func newSynchronizedDownloadExecutor() *synchronizedDownloadExecutor {
	return &synchronizedDownloadExecutor{
		fleetExecutor: newFleetExecutor(),
		entered:       map[string]bool{},
		bothEntered:   make(chan struct{}),
		release:       make(chan struct{}),
		cancelled:     map[string]bool{},
		jobIDs:        map[string][]string{},
		resumeFrom:    map[string]int64{},
	}
}

func (f *synchronizedDownloadExecutor) Execute(ctx context.Context, execution operations.Execution, op recipe.Operation, r recipe.Recipe, progress operations.Progress) (map[string]any, error) {
	if op.Type != "download_artifact" {
		return f.fleetExecutor.Execute(ctx, execution, op, r, progress)
	}
	role := f.node(execution.Placement)
	if role == operations.RoleWorker && execution.Peer == nil {
		return nil, errors.New("the other Spark was not pinned when this job was planned, so it cannot be acted on")
	}
	f.downloadMu.Lock()
	f.jobIDs[role] = append(f.jobIDs[role], execution.JobID)
	if !f.entered[role] {
		f.entered[role] = true
		if len(f.entered) == 2 {
			close(f.bothEntered)
		}
	}
	resumed := f.resumeFrom[role]
	f.downloadMu.Unlock()

	completed := int64(51)
	if role == operations.RoleWorker {
		completed = 67
	}
	receipt := map[string]any{
		"operation": op.Type, "node": execution.Placement.NodeName, "node_role": role,
		"bytes_verified": int64(100), "resumed_from_bytes": resumed,
	}
	if progress != nil {
		if err := progress(map[string]any{"node": execution.Placement.NodeName, "node_role": role, "bytes_complete": completed, "bytes_total": int64(100)}); err != nil {
			if errors.Is(err, context.Canceled) {
				f.markDownloadCancelled(role)
			}
			return receipt, err
		}
	}
	select {
	case <-f.bothEntered:
	case <-ctx.Done():
		f.markDownloadCancelled(role)
		return receipt, ctx.Err()
	}
	if role == f.failRole {
		return receipt, fmt.Errorf("download_artifact failed on %s", role)
	}
	select {
	case <-f.release:
		return receipt, nil
	case <-ctx.Done():
		f.markDownloadCancelled(role)
		return receipt, ctx.Err()
	}
}

func (f *synchronizedDownloadExecutor) markDownloadCancelled(role string) {
	f.downloadMu.Lock()
	f.cancelled[role] = true
	f.downloadMu.Unlock()
}

func (f *synchronizedDownloadExecutor) downloadSnapshot() (map[string]bool, map[string][]string) {
	f.downloadMu.Lock()
	defer f.downloadMu.Unlock()
	cancelled := make(map[string]bool, len(f.cancelled))
	for role, value := range f.cancelled {
		cancelled[role] = value
	}
	jobIDs := make(map[string][]string, len(f.jobIDs))
	for role, values := range f.jobIDs {
		jobIDs[role] = append([]string(nil), values...)
	}
	return cancelled, jobIDs
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

func (f *fleetExecutor) RenewWorkerReservation(_ context.Context, jobID string, r recipe.Recipe) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewals = append(f.renewals, jobID+"@"+r.ID)
	return nil
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
	// Mirrors operations.pinnedPeer: the real FleetExecutor refuses any
	// worker-placed step (and the head's own fabric/peer checks) unless the
	// job pinned a peer when it was planned. A fake that skipped this check
	// would let an engine bug where execution.Peer is never set for a
	// step outside the target's own deployment (see engine.go's switch
	// planning) pass silently here while failing on real hardware.
	requiresPeer := op.Type == operations.VerifyFabric || op.Type == operations.VerifyPeerNode || execution.Placement.Role == operations.RoleWorker
	if requiresPeer && (execution.Peer == nil || execution.Peer.BaseURL == "") {
		return nil, errors.New("the other Spark was not pinned when this job was planned, so it cannot be acted on")
	}
	key := op.Type + "@" + node
	f.events = append(f.events, key)
	f.detail = append(f.detail, op.Type+"/"+r.ID+"@"+node)
	receipt := map[string]any{"operation": op.Type, "node": execution.Placement.NodeName, "node_role": execution.Placement.Role}
	if op.Type == "measure_throughput" {
		receipt["tokens_per_second"] = 42.5
		receipt["time_to_first_token_ms"] = 120
		receipt["completion_tokens"] = 256
		receipt["measured"] = true
	}
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

func (f *fleetExecutor) recordedRenewals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.renewals...)
}

func indexOf(events []string, want string) int {
	for index, event := range events {
		if event == want {
			return index
		}
	}
	return -1
}

func newTwoSparkEngine(t *testing.T, fake operations.Executor) (*Engine, *store.Store, recipe.Recipe) {
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

func TestActiveTwoSparkInstallRenewsItsWorkerDriverLease(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	runner, s, r := newTwoSparkEngine(t, fake)
	job, _, err := s.CreateJob(ctx, "install", r.ID, "renew-active-worker", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	waitJob(t, s, job.ID, "ready")
	if err := runner.RenewDistributedReservations(ctx); err != nil {
		t.Fatal(err)
	}
	renewals := fake.recordedRenewals()
	if len(renewals) != 1 || renewals[0] != job.ID+"@"+r.ID {
		t.Fatalf("worker lease renewals=%v", renewals)
	}
}

// The worker's rank goes active at its own preflight, before the head stages
// a byte, and its lease is only nine heartbeats. Pulling a 20 GB image and
// downloading the weights takes far longer than that, while the head's own
// reservation stays committed until it claims the runtime slot just before
// the container starts. A renewal that waited for the active state therefore
// let the worker reclaim the rank in the middle of the install, and the first
// step the head sent was refused (hardware, 2026-08-28).
func TestWorkerLeaseIsRenewedWhileTheHeadIsStillStaging(t *testing.T) {
	ctx := context.Background()
	fake := newGatedFleetExecutor("staging-lease-model")
	t.Cleanup(fake.unblock)
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r := twoSparkRecipe(t)
	r.ID = "staging-lease-model"
	runner := New(s, fake, []recipe.Recipe{r})

	job, _, err := s.CreateJob(ctx, "install", r.ID, "renew-while-staging", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	select {
	case <-fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the install never reached the download gate")
	}

	held, err := runner.Reservations().Reservation(ctx, fleet.ReservationID(fleet.ClaimKindLocalJob, job.ID))
	if err != nil {
		t.Fatal(err)
	}
	if held.State != "committed" {
		t.Fatalf("the head reservation is %q during staging, so this test no longer covers the staging window", held.State)
	}
	if err := runner.RenewDistributedReservations(ctx); err != nil {
		t.Fatal(err)
	}
	if renewals := fake.recordedRenewals(); len(renewals) != 1 || renewals[0] != job.ID+"@"+r.ID {
		t.Fatalf("the worker lease was not renewed while the head staged: renewals=%v", renewals)
	}

	fake.unblock()
	waitJob(t, s, job.ID, "ready")
}

// Renewal follows this manager's own deployments and nothing else: a settled
// reservation, a claim that names no job, another node's rank, a job this
// process never drove, and a single-Spark model all have no worker lease to
// keep alive.
func TestDistributedLeaseRenewalCoversStagingAndSkipsEverythingElse(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// driving means this manager still has the job in memory, as it does
		// for every job it prepared itself. A row it never prepared was left
		// behind by a manager that is gone.
		driving     bool
		state       string
		kind        string
		jobID       string
		single      bool
		wantRenewed bool
	}{
		{name: "committed deployment in staging", driving: true, state: "committed", kind: fleet.ClaimKindLocalJob, jobID: "job-staging", wantRenewed: true},
		{name: "active deployment serving", driving: true, state: "active", kind: fleet.ClaimKindLocalJob, jobID: "job-serving", wantRenewed: true},
		{name: "released deployment", driving: true, state: "released", kind: fleet.ClaimKindLocalJob, jobID: "job-over"},
		{name: "claim naming no job", driving: true, state: "committed", kind: fleet.ClaimKindLocalJob, jobID: ""},
		{name: "another driver's rank on this node", driving: true, state: "active", kind: fleet.ClaimKindLegacyRank, jobID: "job-remote"},
		{name: "single-Spark deployment", driving: true, state: "committed", kind: fleet.ClaimKindLocalJob, jobID: "job-single", single: true},
		{name: "serving deployment of a manager that is gone", state: "active", kind: fleet.ClaimKindLocalJob, jobID: "job-of-a-dead-process"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fake := newFleetExecutor()
			runner, _, distributed, single := mixedFleet(t, fake)
			if test.driving {
				// Memory records the job, never the row's own kind: a rank
				// belonging to another driver must be skipped for what it is,
				// not because this fixture forgot it.
				runner.noteDriving(fleet.ClaimKindLocalJob, test.jobID)
			}
			selected := distributed
			if test.single {
				selected = single
			}
			fingerprint, err := fleet.RecipeFingerprint(selected)
			if err != nil {
				t.Fatal(err)
			}
			const reservationID = "reservation-under-test"
			allocator := runner.Reservations()
			if _, _, err := allocator.Prepare(ctx, fleet.ReservationRequest{
				ReservationID: reservationID, DeploymentID: "job:" + reservationID, DriverNodeID: allocator.NodeID(),
				RecipeID: selected.ID, RecipeVersion: selected.Version, RecipeFingerprint: fingerprint,
				Claims: fleet.ClaimsForRecipe(selected, fleet.RecipeClaimOptions{Kind: test.kind, JobID: test.jobID, Runtime: true}),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := allocator.Commit(ctx, reservationID, fleet.LocalPrepareToken(reservationID), []byte(`{"kind":"local-engine"}`)); err != nil {
				t.Fatal(err)
			}
			switch test.state {
			case "active":
				if err := allocator.Activate(ctx, reservationID, ""); err != nil {
					t.Fatal(err)
				}
			case "released":
				if err := allocator.Release(ctx, reservationID); err != nil {
					t.Fatal(err)
				}
			}

			if err := runner.RenewDistributedReservations(ctx); err != nil {
				t.Fatal(err)
			}
			var want []string
			if test.wantRenewed {
				want = []string{test.jobID + "@" + selected.ID}
			}
			if renewals := fake.recordedRenewals(); strings.Join(renewals, ",") != strings.Join(want, ",") {
				t.Fatalf("worker lease renewals=%v, want %v", renewals, want)
			}
		})
	}
}

// The live two-Spark failure of 2026-08-29: a manager upgrade restarted the
// head under a serving distributed model. The old serve job died with that
// process, but its local reservation came back active with the job id still
// attached, so the restarted head renewed the worker's rank from the row every
// ten seconds forever. The worker's lease never went stale, its sweep never
// reclaimed the rank, and the designed recovery start, a stop, and a start
// were all refused with "this node's runtime is already reserved" until an
// operator stopped the head manager by hand. A dead job's row must be settled
// at startup and must never be a renewal source again.
func TestARestartedHeadNeverRenewsADeadDeploymentsWorkerRank(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	restarted, s, r, install := servingHeadAfterRestart(t, fake)
	reservationID := fleet.ReservationID(fleet.ClaimKindLocalJob, install)
	held, err := restarted.Reservations().Reservation(ctx, reservationID)
	if err != nil {
		t.Fatal(err)
	}
	if held.State != "active" || held.Claims.JobID != install {
		t.Fatalf("the dead serve job left a %q reservation for job %q, so this test no longer replays the incident", held.State, held.Claims.JobID)
	}

	if err := restarted.ReconcileReservations(ctx); err != nil {
		t.Fatal(err)
	}
	settled, err := restarted.Reservations().Reservation(ctx, reservationID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != "released" {
		t.Fatalf("the dead serve job's claim is %q after startup, so the restarted head is still a renewal source for it", settled.State)
	}
	if err := restarted.RenewDistributedReservations(ctx); err != nil {
		t.Fatal(err)
	}
	if renewals := fake.recordedRenewals(); len(renewals) != 0 {
		t.Fatalf("the restarted head renewed the worker rank of a job that died with the previous process: %v", renewals)
	}

	// Recovery can now take the pair back, because the worker is free to
	// reclaim its own rank, and the head renews for that job alone.
	if err := restarted.ReconcileActiveModel(ctx); err != nil {
		t.Fatal(err)
	}
	waitModelStatus(t, s, r.ID, "ready")
	waitJobsSettled(t, s)
	if err := restarted.RenewDistributedReservations(ctx); err != nil {
		t.Fatal(err)
	}
	recovery := onlyStartJob(t, s, r.ID)
	if renewals := fake.recordedRenewals(); len(renewals) != 1 || renewals[0] != recovery+"@"+r.ID {
		t.Fatalf("after recovery the head renews %v, want only its own job %s", renewals, recovery)
	}
}

// servingHeadAfterRestart serves a two-Spark model, then restarts the manager
// over the same database the way an upgrade does: the store marks the active
// model recovering, and all that is left of the deployment is the serve job's
// reservation and a manager that never drove it. It returns the new manager
// and the job that died with the old one.
func servingHeadAfterRestart(t *testing.T, fake operations.Executor) (*Engine, *store.Store, recipe.Recipe, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := twoSparkRecipe(t)
	r.ID = "restarted-head-model"
	install, _, err := s.CreateJob(ctx, "install", r.ID, "serving-before-the-upgrade", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	before := New(s, newFleetExecutor(), []recipe.Recipe{r})
	before.Start(install.ID)
	waitJob(t, s, install.ID, "ready")
	// A first activation also measures the model, and that job outlives the
	// install. Closing the database under it is a test artefact, not part of
	// the incident, so the fixture waits for this manager to go quiet.
	waitJobsSettled(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	restarted := New(s, fake, []recipe.Recipe{r})
	restarted.recoveryRetryDelay = 20 * time.Millisecond
	return restarted, s, r, install.ID
}

// Startup must settle the dead rows BEFORE the allocator rebuilds this node's
// runtime claim, and the order is load-bearing rather than cosmetic. The
// allocator reads the active rows to decide which models already hold the
// runtime slot, so a dead row still active at that moment is counted as the
// serving model's own claim and no recovery claim is built for it; releasing
// the row afterwards then leaves the slot owned by nothing at all. The model
// stays installed and active, but any other model can walk onto the machine,
// and it stays that way until a recovery start succeeds.
func TestStartupSettlesTheDeadRowBeforeItRebuildsTheRuntimeClaim(t *testing.T) {
	ctx := context.Background()
	restarted, _, r, _ := servingHeadAfterRestart(t, newFleetExecutor())
	if err := restarted.ReconcileReservations(ctx); err != nil {
		t.Fatal(err)
	}
	allocator := restarted.Reservations()
	recovered, err := allocator.Reservation(ctx, fleet.ReservationID(fleet.ClaimKindRecovered, r.ID))
	if err != nil {
		t.Fatalf("startup left the serving model with no recovery claim at all: %v", err)
	}
	if recovered.State != "active" {
		t.Fatalf("the serving model's recovery claim is %q, so this node's runtime slot is owned by nothing", recovered.State)
	}

	// Owned, and guarded: a stranger must not be able to take the runtime slot
	// of a machine whose own model is still installed and active.
	if err := refuseStranger(t, allocator, "another-model"); !errors.Is(err, store.ErrReservationConflict) {
		t.Fatalf("an unrelated model took the runtime slot of a serving machine: err=%v", err)
	}
}

// holdUpgradeLatch takes the maintenance reservation a fleet upgrade holds
// across the manager restart it causes, exactly as the updater does.
func holdUpgradeLatch(t *testing.T, allocator *fleet.Allocator) string {
	t.Helper()
	ctx := context.Background()
	latchID := fleet.ReservationID(fleet.ClaimKindUpdate, "fleet-upgrade")
	if _, _, err := allocator.Prepare(ctx, fleet.ReservationRequest{
		ReservationID: latchID, DeploymentID: "upgrade:fleet-upgrade", DriverNodeID: allocator.NodeID(),
		RecipeID: "basement-manager", RecipeVersion: 1,
		Claims: fleet.Claims{Version: fleet.ClaimsVersion, Kind: fleet.ClaimKindUpdate, Runtime: true,
			Ports: []int{}, FabricInterfaces: []string{}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.Commit(ctx, latchID, fleet.LocalPrepareToken(latchID), []byte(`{"kind":"test-upgrade"}`)); err != nil {
		t.Fatal(err)
	}
	if err := allocator.ActivateMaintenance(ctx, latchID); err != nil {
		t.Fatal(err)
	}
	return latchID
}

// refuseStranger walks an unrelated model all the way to the runtime slot and
// reports the step that refuses it. The refusal moves with the guard: the
// upgrade latch turns even a prepare away, while an ordinary active claim lets
// the row exist and refuses the activation, and both are the same property.
func refuseStranger(t *testing.T, allocator *fleet.Allocator, name string) error {
	t.Helper()
	ctx := context.Background()
	reservationID := "reservation-of-" + name
	if _, _, err := allocator.Prepare(ctx, fleet.ReservationRequest{
		ReservationID: reservationID, DeploymentID: "job:" + reservationID, DriverNodeID: allocator.NodeID(),
		RecipeID: name, RecipeVersion: 1,
		Claims: fleet.Claims{Version: fleet.ClaimsVersion, Kind: fleet.ClaimKindLocalJob, JobID: "job-of-" + name,
			Runtime: true, Ports: []int{}, FabricInterfaces: []string{}},
	}); err != nil {
		return err
	}
	if _, err := allocator.Commit(ctx, reservationID, fleet.LocalPrepareToken(reservationID), []byte(`{"kind":"local-engine"}`)); err != nil {
		return err
	}
	return allocator.Activate(ctx, reservationID, "")
}

// The v0.5.26 field failure of 2026-08-29. A fleet upgrade holds its
// maintenance latch across the manager restart it causes, and that latch closes
// runtime admission. With the dead serve row correctly settled first, the
// allocator went on to rebuild the recovering model's runtime claim, the latch
// refused it, and the manager treated the refusal as a corrupt state: exit 1,
// restarted by systemd every five seconds, past sixty restarts. The latch is
// only released when the upgrade run finalizes, and that needs this manager
// serving, so the machine could not leave the loop without hand surgery on the
// database. Startup must survive its own upgrade.
func TestStartupSurvivesTheUpgradeLatchItRestartedUnder(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	restarted, _, r, install := servingHeadAfterRestart(t, fake)
	allocator := restarted.Reservations()
	latchID := holdUpgradeLatch(t, allocator)

	// The production startup sequence, in the order main runs it.
	if err := restarted.ReconcileReservations(ctx); err != nil {
		t.Fatalf("startup died on the fleet upgrade's own latch, which is the crash loop: %v", err)
	}
	if err := restarted.ResumeInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReconcileActiveModel(ctx); err != nil {
		t.Fatal(err)
	}

	// Surviving the latch must not cost the round 1 property: the dead
	// deployment's row is still settled.
	settled, err := allocator.Reservation(ctx, fleet.ReservationID(fleet.ClaimKindLocalJob, install))
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != "released" {
		t.Fatalf("the dead serve job's claim is %q after a latched startup", settled.State)
	}
	// Nothing is open while the latch is held: the latch is the guard.
	if err := refuseStranger(t, allocator, "stranger-during-upgrade"); !errors.Is(err, store.ErrReservationConflict) {
		t.Fatalf("an unrelated model took the runtime slot while the upgrade held the node: err=%v", err)
	}

	// The upgrade finalizes, which it can now do, and the slot must end up
	// guarded again by a claim that names this machine's own model.
	if err := allocator.Release(ctx, latchID); err != nil {
		t.Fatal(err)
	}
	waitRuntimeClaim(t, allocator, r.ID)
	if err := refuseStranger(t, allocator, "stranger-after-upgrade"); !errors.Is(err, store.ErrReservationConflict) {
		t.Fatalf("the runtime slot was left open after the upgrade latch cleared: err=%v", err)
	}
}

// The deferred pass is what rebuilds the claim the latch refused, so it holds
// even when no recovery start ever runs — the case where the gap would
// otherwise be unbounded.
func TestTheClaimTheUpgradeLatchRefusedIsRebuiltWhenItClears(t *testing.T) {
	ctx := context.Background()
	restarted, _, r, _ := servingHeadAfterRestart(t, newFleetExecutor())
	allocator := restarted.Reservations()
	latchID := holdUpgradeLatch(t, allocator)

	if err := restarted.ReconcileReservations(ctx); err != nil {
		t.Fatalf("startup died on the fleet upgrade's own latch: %v", err)
	}
	recoveredID := fleet.ReservationID(fleet.ClaimKindRecovered, r.ID)
	if held, err := allocator.Reservation(ctx, recoveredID); err == nil && held.State == "active" {
		t.Fatal("the recovery claim was activated while the upgrade latch was still held")
	}

	if err := allocator.Release(ctx, latchID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		recovered, err := allocator.Reservation(ctx, recoveredID)
		if err == nil && recovered.State == "active" {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the recovery claim was never rebuilt after the upgrade latch cleared: %+v err=%v", recovered.State, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A job the previous manager left interrupted is resumed with the reservation
// it already holds, so it never prepares a new one: the resuming manager
// adopts the existing row instead. That adoption is the only thing that puts a
// resumed job back into this process's memory, and a resumed two-Spark install
// goes straight back to staging, where the worker's rank is active on a
// nine-heartbeat lease and the head is the only thing that can keep it alive.
// Losing the adoption would let the worker reclaim the rank in the middle of
// the resumed download, which is the 2026-08-28 failure.
func TestAResumedInstallKeepsRenewingItsWorkerRank(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := twoSparkRecipe(t)
	r.ID = "resumed-staging-model"
	install, _, err := s.CreateJob(ctx, "install", r.ID, "staging-before-the-restart", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	// What the previous manager durably left behind when it stopped in the
	// middle of staging: a committed local claim with its job attached.
	reservationID := fleet.ReservationID(fleet.ClaimKindLocalJob, install.ID)
	fingerprint, err := fleet.RecipeFingerprint(r)
	if err != nil {
		t.Fatal(err)
	}
	allocator := fleet.NewAllocator(s, "local")
	if _, _, err := allocator.Prepare(ctx, fleet.ReservationRequest{
		ReservationID: reservationID, DeploymentID: "job:" + install.ID, DriverNodeID: allocator.NodeID(),
		RecipeID: r.ID, RecipeVersion: r.Version, RecipeFingerprint: fingerprint,
		Claims: fleet.ClaimsForRecipe(r, fleet.RecipeClaimOptions{
			Kind: fleet.ClaimKindLocalJob, JobID: install.ID, ReserveDisk: true, Runtime: true,
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.Commit(ctx, reservationID, fleet.LocalPrepareToken(reservationID), []byte(`{"kind":"local-engine"}`)); err != nil {
		t.Fatal(err)
	}
	if err := allocator.AttachJob(ctx, reservationID, install.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	fake := newGatedFleetExecutor(r.ID)
	t.Cleanup(fake.unblock)
	resuming := New(s, fake, []recipe.Recipe{r})
	if err := resuming.ReconcileReservations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := resuming.ResumeInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the interrupted install never resumed to its download")
	}

	held, err := resuming.Reservations().Reservation(ctx, reservationID)
	if err != nil {
		t.Fatal(err)
	}
	if held.State != "committed" {
		t.Fatalf("the resumed job's reservation is %q, so this test no longer covers the staging window", held.State)
	}
	if err := resuming.RenewDistributedReservations(ctx); err != nil {
		t.Fatal(err)
	}
	if renewals := fake.recordedRenewals(); len(renewals) != 1 || renewals[0] != install.ID+"@"+r.ID {
		t.Fatalf("the resumed job's worker rank was left to expire: renewals=%v", renewals)
	}

	fake.unblock()
	waitJob(t, s, install.ID, "ready")
}

func waitJobsSettled(t *testing.T, s *store.Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := s.ListJobs(context.Background(), 100)
		if err != nil {
			t.Fatal(err)
		}
		settled := true
		for _, job := range jobs {
			if !terminal(job.State) {
				settled = false
			}
		}
		if settled {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	jobs, _ := s.ListJobs(context.Background(), 100)
	t.Fatalf("jobs never settled: %+v", jobs)
}

// waitRuntimeClaim waits until this node's runtime slot is owned by a claim
// that names one model. Which legitimate claim it is deliberately does not
// matter: the recovery claim and the recovery start's own job claim are both
// correct owners, and the second is designed to replace the first.
func waitRuntimeClaim(t *testing.T, allocator *fleet.Allocator, recipeID string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		reservations, err := allocator.AllReservations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, reservation := range reservations {
			if reservation.State == "active" && reservation.RecipeID == recipeID {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("nothing ever claimed this node's runtime slot for %s", recipeID)
}

func waitModelStatus(t *testing.T, s *store.Store, recipeID, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		model, err := s.Model(context.Background(), recipeID)
		if err != nil {
			t.Fatal(err)
		}
		if model.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	jobs, _ := s.ListJobs(context.Background(), 5)
	t.Fatalf("model %s never became %s; jobs=%+v", recipeID, want, jobs)
}

func onlyStartJob(t *testing.T, s *store.Store, recipeID string) string {
	t.Helper()
	jobs, err := s.ListJobs(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	found := ""
	for _, job := range jobs {
		if job.Kind != "start" || job.RecipeID != recipeID {
			continue
		}
		if found != "" {
			t.Fatalf("recovery ran more than one start job for %s", recipeID)
		}
		found = job.ID
	}
	if found == "" {
		t.Fatalf("recovery never created a start job for %s", recipeID)
	}
	return found
}

func TestTwoSparkArtifactDownloadsOverlapAndKeepReceiptsPerNode(t *testing.T) {
	ctx := context.Background()
	fake := newSynchronizedDownloadExecutor()
	runner, s, r := newTwoSparkEngine(t, fake)
	job, _, err := s.CreateJob(ctx, "install", r.ID, "overlapping-downloads", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	select {
	case <-fake.bothEntered:
		// Each call waits at the barrier, so this can happen only when both
		// transfers are in Execute at the same time.
	case <-time.After(5 * time.Second):
		_ = runner.Cancel(ctx, job.ID)
		t.Fatal("head and worker downloads never overlapped")
	}
	close(fake.release)
	finished := waitJob(t, s, job.ID, "ready")

	receipts := map[string]map[string]any{}
	for _, step := range finished.Steps {
		if !strings.HasPrefix(step.Operation, "download_artifact:") {
			continue
		}
		var receipt map[string]any
		if err := json.Unmarshal(step.Receipt, &receipt); err != nil {
			t.Fatalf("decode %s receipt: %v", step.Operation, err)
		}
		receipts[step.Operation] = receipt
	}
	for _, role := range []string{operations.RoleHead, operations.RoleWorker} {
		operation := "download_artifact:" + role
		receipt := receipts[operation]
		if receipt["node_role"] != role {
			t.Fatalf("%s receipt was attributed to %v: %#v", operation, receipt["node_role"], receipt)
		}
		wantNode := map[string]string{operations.RoleHead: "spark-a", operations.RoleWorker: "spark-b"}[role]
		if receipt["node"] != wantNode {
			t.Fatalf("%s receipt names %v, want %s: %#v", operation, receipt["node"], wantNode, receipt)
		}
	}
}

func TestTwoSparkDownloadFailureCancelsAndRecordsTheSibling(t *testing.T) {
	ctx := context.Background()
	fake := newSynchronizedDownloadExecutor()
	fake.failRole = operations.RoleWorker
	runner, s, r := newTwoSparkEngine(t, fake)
	job, _, err := s.CreateJob(ctx, "install", r.ID, "worker-download-fails", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if !strings.Contains(failed.Error, "download_artifact failed on worker") {
		t.Fatalf("worker download failure was not preserved: %s", failed.Error)
	}
	cancelled, _ := fake.downloadSnapshot()
	if !cancelled[operations.RoleHead] {
		t.Fatal("the head download was not cancelled after the worker failed")
	}
	states := map[string]store.Step{}
	for _, step := range failed.Steps {
		states[step.Operation] = step
	}
	head := states["download_artifact:"+operations.RoleHead]
	worker := states["download_artifact:"+operations.RoleWorker]
	if head.State != "failed" || !strings.Contains(head.Error, "other Spark's download failed") {
		t.Fatalf("cancelled sibling outcome was not recorded: %#v", head)
	}
	if worker.State != "failed" || !strings.Contains(worker.Error, "failed on worker") {
		t.Fatalf("primary worker failure was not recorded: %#v", worker)
	}
	assertBothNodesTornDown(t, s, job.ID, fake.recorded())
}

func TestInterruptedTwoSparkDownloadsResumeWithTheSameJobIdentity(t *testing.T) {
	ctx := context.Background()
	fake := newSynchronizedDownloadExecutor()
	fake.resumeFrom[operations.RoleHead] = 40
	fake.resumeFrom[operations.RoleWorker] = 60
	runner, s, r := newTwoSparkEngine(t, fake)
	job, _, err := s.CreateJob(ctx, "install", r.ID, "resume-both-downloads", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := runner.deployment(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := runner.plan(ctx, job, r, deployment)
	if err != nil {
		t.Fatal(err)
	}
	for index, plan := range planned.plans {
		if plan.Operation.Type != "download_artifact" {
			continue
		}
		if err := s.BeginStep(ctx, job.ID, index, stepName(plan.Operation, plan.Placement)); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateStepReceipt(ctx, job.ID, index, map[string]any{"node_role": plan.Placement.Role, "bytes_complete": fake.resumeFrom[plan.Placement.Role]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateJobState(ctx, job.ID, "interrupted", "manager stopped during download"); err != nil {
		t.Fatal(err)
	}

	runner.Start(job.ID)
	select {
	case <-fake.bothEntered:
	case <-time.After(5 * time.Second):
		_ = runner.Cancel(ctx, job.ID)
		t.Fatal("interrupted node downloads did not resume together")
	}
	close(fake.release)
	finished := waitJob(t, s, job.ID, "ready")
	_, jobIDs := fake.downloadSnapshot()
	for _, role := range []string{operations.RoleHead, operations.RoleWorker} {
		if len(jobIDs[role]) != 1 || jobIDs[role][0] != job.ID {
			t.Fatalf("%s resumed with job identities %v, want only %s", role, jobIDs[role], job.ID)
		}
		operation := "download_artifact:" + role
		found := false
		for _, step := range finished.Steps {
			if step.Operation != operation {
				continue
			}
			found = true
			var receipt map[string]any
			if err := json.Unmarshal(step.Receipt, &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt["resumed_from_bytes"] != float64(fake.resumeFrom[role]) {
				t.Fatalf("%s receipt lost its resume point: %#v", operation, receipt)
			}
		}
		if !found {
			t.Fatalf("no resumed receipt for %s", operation)
		}
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

// Every two-Spark job proves the cable before it does anything else. An hour
// of downloading over a link that cannot carry the model is the failure this
// prevents, so first means first, not "before the containers".
func TestTwoSparkJobsCheckTheCableFirst(t *testing.T) {
	ctx := context.Background()
	done := map[string]string{"install": "ready", "start": "ready", "stop": "stopped", "remove": "removed"}
	for _, kind := range []string{"install", "start", "stop", "remove"} {
		t.Run(kind, func(t *testing.T) {
			fake := newFleetExecutor()
			runner, s, r := newTwoSparkEngine(t, fake)
			if kind != "install" {
				install, _, err := s.CreateJob(ctx, "install", r.ID, "install-before-"+kind, map[string]any{"confirmed": true})
				if err != nil {
					t.Fatal(err)
				}
				runner.Start(install.ID)
				waitJob(t, s, install.ID, "ready")
			}
			var payload any = map[string]any{"confirmed": true}
			if kind == "remove" {
				payload = RemovePayload{RemoveArtifacts: true}
			}
			job, _, err := s.CreateJob(ctx, kind, r.ID, "cable-first-"+kind, payload)
			if err != nil {
				t.Fatal(err)
			}
			runner.Start(job.ID)
			waitJob(t, s, job.ID, done[kind])

			// The job's own recorded steps, not the executor's log: a
			// follow-on job of the install (a benchmark) shares the executor
			// and would otherwise be read as this job's first move.
			finished, err := s.GetJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(finished.Steps) == 0 || finished.Steps[0].Operation != operations.VerifyFabric+":"+operations.RoleHead {
				t.Fatalf("%s recorded %v as its first step", kind, finished.Steps)
			}
		})
	}
}

// A cable that cannot carry the model stops the job where it stands: nothing
// is pulled, nothing is downloaded, and the worker is never even asked to
// check itself.
func TestTwoSparkInstallStopsWhenTheCableCannotCarryIt(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	fake.failStepNode = operations.VerifyFabric + "@head"
	runner, s, r := newTwoSparkEngine(t, fake)
	job, _, err := s.CreateJob(ctx, "install", r.ID, "no-cable", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if !strings.Contains(failed.Error, operations.VerifyFabric+" failed on head") {
		t.Fatalf("unexpected failure: %s", failed.Error)
	}
	if fake.peerAsked != 0 {
		t.Fatalf("the worker was asked to check itself over a cable that does not work")
	}
	for _, event := range fake.recorded() {
		if strings.HasPrefix(event, "pull_image") || strings.HasPrefix(event, "download_artifact") {
			t.Fatalf("staging started over a cable that does not work: %v", fake.recorded())
		}
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
	job, _, err := s.CreateJob(ctx, "install", singleSpark(recipes).ID, "single-spark", map[string]any{"confirmed": true})
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

func TestConcurrentInstallReplansForDistributedPredecessor(t *testing.T) {
	ctx := context.Background()
	builtin, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	single := singleSpark(builtin)
	distributed := twoSparkRecipe(t)
	distributed.ID = "concurrent-distributed-predecessor"
	fake := newGatedFleetExecutor(single.ID)
	t.Cleanup(fake.unblock)
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runner := New(s, fake, []recipe.Recipe{single, distributed})

	first, _, err := s.CreateJob(ctx, "install", single.ID, "single-planned-first", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(first.ID)
	select {
	case <-fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("single-node install never reached the download gate")
	}
	second, _, err := s.CreateJob(ctx, "install", distributed.ID, "distributed-finishes-first", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(second.ID)
	waitJob(t, s, second.ID, "ready")
	fake.unblock()
	waitJob(t, s, first.ID, "ready")

	detail := fake.recordedDetail()
	for _, want := range []string{
		"stop_container/" + distributed.ID + "@head",
		"stop_container/" + distributed.ID + "@worker",
	} {
		if indexOf(detail, want) < 0 {
			t.Fatalf("activation-time switch did not stop %s: %v", want, detail)
		}
	}
	if indexOf(detail, "stop_container/"+distributed.ID+"@head") > indexOf(detail, "stop_container/"+distributed.ID+"@worker") {
		t.Fatalf("the outgoing head must stop before its worker: %v", detail)
	}
	if indexOf(detail, "stop_container/"+distributed.ID+"@worker") > indexOf(detail, "start_container/"+single.ID+"@local") {
		t.Fatalf("the single-node target started before both predecessor ranks stopped: %v", detail)
	}

	fake.mu.Lock()
	running := make(map[string]bool, len(fake.running))
	for node, value := range fake.running {
		running[node] = value
	}
	fake.mu.Unlock()
	runningCount := 0
	for _, value := range running {
		if value {
			runningCount++
		}
	}
	if runningCount != 1 || !running["local"] {
		t.Fatalf("running containers=%v, want only the single-node target", running)
	}
	assertActiveModel(t, s, single.ID, distributed.ID, "stopped")
}

func TestConcurrentDistributedTargetReplansForSinglePredecessor(t *testing.T) {
	ctx := context.Background()
	builtin, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	single := singleSpark(builtin)
	distributed := twoSparkRecipe(t)
	distributed.ID = "concurrent-distributed-target"
	fake := newGatedFleetExecutor(distributed.ID)
	t.Cleanup(fake.unblock)
	s, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runner := New(s, fake, []recipe.Recipe{single, distributed})

	first, _, err := s.CreateJob(ctx, "install", distributed.ID, "distributed-planned-first", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(first.ID)
	select {
	case <-fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("distributed install never reached the download gate")
	}
	second, _, err := s.CreateJob(ctx, "install", single.ID, "single-finishes-first", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(second.ID)
	waitJob(t, s, second.ID, "ready")
	fake.unblock()
	waitJob(t, s, first.ID, "ready")

	detail := fake.recordedDetail()
	stop := indexOf(detail, "stop_container/"+single.ID+"@local")
	startWorker := indexOf(detail, "start_container/"+distributed.ID+"@worker")
	if stop < 0 || startWorker < 0 || stop > startWorker {
		t.Fatalf("the distributed target did not stop the discovered single-node predecessor before bringing up its worker: %v", detail)
	}
	fake.mu.Lock()
	running := make(map[string]bool, len(fake.running))
	for node, value := range fake.running {
		running[node] = value
	}
	fake.mu.Unlock()
	if running["local"] || !running[operations.RoleHead] || !running[operations.RoleWorker] {
		t.Fatalf("running containers=%v, want only the distributed target's two ranks", running)
	}
	assertActiveModel(t, s, distributed.ID, single.ID, "stopped")
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

// TestStartingASingleNodeModelStopsBothRanksOfADistributedPredecessor is
// task #50's exact real-hardware scenario: a two-Spark model is already
// serving (head + worker containers running), and the operator starts an
// already-downloaded single-Spark model instead of installing a fresh one.
// That "start" job is a different code path through plan() than "install"
// (see the switchStopPlanned loop), and it is the one that shipped without
// ever pinning a peer for the predecessor's worker stop.
func TestStartingASingleNodeModelStopsBothRanksOfADistributedPredecessor(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	runner, s, distributed, single := mixedFleet(t, fake)

	first, _, err := s.CreateJob(ctx, "install", distributed.ID, "install-distributed", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(first.ID)
	waitJob(t, s, first.ID, "ready")

	// Download the single-node model without activating it, exactly like an
	// operator queuing up a model to switch to later.
	download, _, err := s.CreateJob(ctx, "install", single.ID, "download-single", map[string]any{"confirmed": true, "activate": false})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(download.ID)
	waitJob(t, s, download.ID, "ready")

	fake.mu.Lock()
	fake.events, fake.detail = nil, nil
	fake.mu.Unlock()

	start, _, err := s.CreateJob(ctx, "start", single.ID, "start-single-while-distributed-serves", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(start.ID)
	job := waitJob(t, s, start.ID, "ready")
	if job.Error != "" {
		t.Fatalf("starting the single-node model reported an error despite reaching ready: %s", job.Error)
	}

	detail := fake.recordedDetail()
	for _, want := range []string{"stop_container/" + distributed.ID + "@head", "stop_container/" + distributed.ID + "@worker"} {
		if indexOf(detail, want) < 0 {
			t.Fatalf("%s did not run when the single-node model started: %v", want, detail)
		}
	}
	if indexOf(detail, "stop_container/"+distributed.ID+"@head") > indexOf(detail, "stop_container/"+distributed.ID+"@worker") {
		t.Fatalf("the outgoing head must stop serving before its worker rank: %v", detail)
	}

	models, err := s.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		if model.RecipeID == distributed.ID && model.Active {
			t.Fatalf("the distributed predecessor is still marked active: %#v", model)
		}
		if model.RecipeID == single.ID && !model.Active {
			t.Fatalf("the single-node model did not become active: %#v", model)
		}
	}
}

// TestWorkerStopFailureDuringSwitchFailsTheJob covers the other half of task
// #50's contract: once the worker's stop is actually reached (the peer is
// pinned so it no longer fails with "not pinned"), a genuine failure to stop
// that container must fail the job with a clear reason, never be swallowed.
func TestWorkerStopFailureDuringSwitchFailsTheJob(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	runner, s, distributed, single := mixedFleet(t, fake)

	first, _, err := s.CreateJob(ctx, "install", distributed.ID, "install-distributed", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(first.ID)
	waitJob(t, s, first.ID, "ready")

	fake.failStepNode = "stop_container@worker"

	second, _, err := s.CreateJob(ctx, "install", single.ID, "install-single-worker-stop-fails", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(second.ID)
	failed := waitJob(t, s, second.ID, "failed")
	if !strings.Contains(failed.Error, "stop_container failed on worker") {
		t.Fatalf("worker stop failure was not reported plainly: %s", failed.Error)
	}

	// The distributed predecessor's worker rank never actually stopped, so
	// this must not read as a clean switch: the job failed, and (best
	// effort) the predecessor is restored rather than left half torn down.
	if !strings.Contains(failed.Error, "restored and verified") {
		t.Fatalf("failure did not report what happened to the previous model: %s", failed.Error)
	}
}

// TestTwoSparkBenchmarkPersistsMeasuredThroughput is task #53's regression:
// a distributed recipe's measure_throughput step is recorded as
// "measure_throughput:head" (see stepName), so a benchmarkResult that only
// matched the bare "measure_throughput" operation name never found it. The
// job itself still reported "ready" with a real number in the step receipt,
// which is what made this bug invisible from the job's own state and only
// showed up as the model's stored metrics silently staying empty.
func TestTwoSparkBenchmarkPersistsMeasuredThroughput(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	runner, s, r := newTwoSparkEngine(t, fake)
	install, _, err := s.CreateJob(ctx, "install", r.ID, "install-before-benchmark", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(install.ID)
	waitJob(t, s, install.ID, "ready")

	job, _, err := s.CreateJob(ctx, "benchmark", r.ID, "two-spark-benchmark", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(job.ID)
	finished := waitJob(t, s, job.ID, "ready")
	if finished.Error != "" {
		t.Fatalf("benchmark job reported an error despite reaching ready: %s", finished.Error)
	}

	found := false
	for _, step := range finished.Steps {
		if step.Operation == "measure_throughput:head" {
			found = true
		}
	}
	if !found {
		t.Fatalf("benchmark job did not record measure_throughput:head: %v", finished.Steps)
	}

	model, err := s.Model(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if model.TokensPerSecond != 42.5 {
		t.Fatalf("measured throughput was not persisted to the model: got %v tok/s, want 42.5", model.TokensPerSecond)
	}
	if model.MeasuredAt == "" {
		t.Fatal("measured_at was not set once the benchmark completed")
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
