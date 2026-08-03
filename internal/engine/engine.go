package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/punkjazz-labs/runonspark-manager/internal/operations"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
	"github.com/punkjazz-labs/runonspark-manager/internal/redact"
	"github.com/punkjazz-labs/runonspark-manager/internal/store"
)

type Engine struct {
	store    *store.Store
	executor operations.Executor
	// effective is the current merged catalog (recipe.Merge): one recipe per
	// ID, the newest version known. It is what a NEW install or update
	// targets. all is every distinct (id, version) this manager has ever
	// verified — embedded, cached, or fetched — and only ever grows; it is
	// what an ALREADY-INSTALLED model must be operated on with (see
	// recipeFor), so a background recipe-index refresh can never change
	// which container name, port, or config an existing install resolves
	// to. Both are swapped atomically by SetRecipes as a unit; readers never
	// take a lock.
	effective atomic.Pointer[[]recipe.Recipe]
	all       atomic.Pointer[[]recipe.Recipe]
	mu        sync.Mutex
	running   map[string]context.CancelFunc

	recipeLocks map[string]*sync.Mutex
	// runtime serializes container and activation mutations across recipes;
	// downloads and preflight run outside it so a long install cannot block
	// stopping an unrelated model.
	runtime chan struct{}
	// reserved holds each running install job's conservative disk footprint
	// (recipe.Recipe.RequiredBytes), keyed by job ID and guarded by mu. It
	// lets two concurrent installs see each other's claim on free space
	// instead of each checking disk in isolation.
	reserved map[string]int64
}

type RemovePayload struct {
	RemoveArtifacts bool `json:"remove_artifacts"`
}

// InstallPayload carries the console's activation choice. A nil Activate
// means true, so jobs created before this field existed keep installing and
// serving immediately.
type InstallPayload struct {
	Activate *bool `json:"activate,omitempty"`
}

func (p InstallPayload) activate() bool {
	return p.Activate == nil || *p.Activate
}

type plannedOperation struct {
	Operation   recipe.Operation
	Recipe      recipe.Recipe
	BeginSwitch bool
	Receipt     map[string]any
	// Placement names the node this step runs on. Its zero value means the
	// local node with no distributed semantics, which is every single-Spark
	// recipe.
	Placement operations.Placement
}

// New starts the engine with recipes as both the effective catalog and the
// full version history — correct at startup, when nothing has ever been
// overlaid yet. SetRecipes takes over both from the first background
// recipe-index refresh onward.
func New(s *store.Store, executor operations.Executor, recipes []recipe.Recipe) *Engine {
	e := &Engine{store: s, executor: executor, running: map[string]context.CancelFunc{}, recipeLocks: map[string]*sync.Mutex{}, runtime: make(chan struct{}, 1), reserved: map[string]int64{}}
	e.SetRecipes(recipes, recipes)
	return e
}

// SetRecipes replaces the recipe catalog and history. all must be
// monotonically growing (never drop a version some installed model may
// still depend on); the caller (internal/recipefeed) owns that guarantee.
// Safe to call from any goroutine at any time, including while jobs run.
func (e *Engine) SetRecipes(all, effective []recipe.Recipe) {
	e.all.Store(&all)
	e.effective.Store(&effective)
}

func (e *Engine) effectiveRecipes() []recipe.Recipe {
	if p := e.effective.Load(); p != nil {
		return *p
	}
	return nil
}

func (e *Engine) allRecipes() []recipe.Recipe {
	if p := e.all.Load(); p != nil {
		return *p
	}
	return nil
}

// recipeFor resolves the recipe to operate a job with. A fresh install
// always targets the current effective (latest) catalog entry for the
// recipe ID — that is what "install" and "update" mean. Every other job
// kind operates on an EXISTING installed model, and must resolve to the
// exact version recorded in store.InstalledModel.RecipeVersion: the
// catalog's entry for that ID may have moved on to a newer version in the
// background since the model was installed, and using that newer
// definition would compute the wrong container name (host.go's
// containerName embeds the version) or the wrong requirements for a
// container that was never created. When the exact installed version is not
// resolvable (should not normally happen: history only ever grows), this
// falls back to the effective entry rather than failing outright.
func (e *Engine) recipeFor(ctx context.Context, kind, recipeID string) (recipe.Recipe, bool) {
	effective := e.effectiveRecipes()
	if kind == "install" {
		return recipe.Find(effective, recipeID)
	}
	if model, err := e.store.Model(ctx, recipeID); err == nil {
		if pinned, ok := recipe.FindVersion(e.allRecipes(), recipeID, model.RecipeVersion); ok {
			return pinned, true
		}
	}
	return recipe.Find(effective, recipeID)
}

// reserveDisk records jobID's conservative disk footprint so concurrent
// installs see it. Only install jobs call this; every other kind leaves
// shared disk budget alone.
func (e *Engine) reserveDisk(jobID string, bytes int64) {
	e.mu.Lock()
	e.reserved[jobID] = bytes
	e.mu.Unlock()
}

func (e *Engine) releaseDisk(jobID string) {
	e.mu.Lock()
	delete(e.reserved, jobID)
	e.mu.Unlock()
}

// ReservedDiskBytes reports the total disk currently reserved by running
// install jobs, for callers outside any job — the advisory preflight uses
// it so the dialog's disk check agrees with what the real verify_disk step
// will conclude while another install runs.
func (e *Engine) ReservedDiskBytes() int64 {
	return e.reservedByOthers("")
}

// reservedByOthers sums every other job's disk reservation, excluding
// jobID's own, so a job never counts its own bytes against itself.
func (e *Engine) reservedByOthers(jobID string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var total int64
	for id, bytes := range e.reserved {
		if id != jobID {
			total += bytes
		}
	}
	return total
}

func (e *Engine) recipeLock(recipeID string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	lock, ok := e.recipeLocks[recipeID]
	if !ok {
		lock = &sync.Mutex{}
		e.recipeLocks[recipeID] = lock
	}
	return lock
}

func (e *Engine) acquireRuntime(ctx context.Context) error {
	select {
	case e.runtime <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) releaseRuntime() { <-e.runtime }

// runtimeOperations mutate or verify the shared runtime (containers, live
// memory, the single active slot) and therefore take the runtime lock.
var runtimeOperations = map[string]bool{
	"stop_container": true, "create_container": true, "verify_memory": true,
	"start_container": true, "wait_http": true, "verify_openai_inference": true,
	"remove_container": true, "measure_throughput": true,
}

func (e *Engine) ResumeInterrupted(ctx context.Context) error {
	jobs, err := e.store.ListJobs(ctx, 100)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.State == "interrupted" {
			e.Start(job.ID)
		}
	}
	return nil
}

func (e *Engine) ReconcileActiveModel(ctx context.Context) error {
	models, err := e.store.Models(ctx)
	if err != nil {
		return err
	}
	for _, model := range models {
		if !model.Active || model.Status != "recovering" {
			continue
		}
		job, _, err := e.store.CreateJob(ctx, "start", model.RecipeID, fmt.Sprintf("reconcile-%d", time.Now().UnixNano()), map[string]any{"recovery": true})
		if err != nil {
			return err
		}
		e.Start(job.ID)
	}
	return nil
}

func (e *Engine) Start(jobID string) {
	e.mu.Lock()
	if _, exists := e.running[jobID]; exists {
		e.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.running[jobID] = cancel
	e.mu.Unlock()
	go func() {
		defer func() { e.mu.Lock(); delete(e.running, jobID); e.mu.Unlock() }()
		e.run(ctx, jobID)
	}()
}

func (e *Engine) Cancel(ctx context.Context, jobID string) error {
	e.mu.Lock()
	cancel := e.running[jobID]
	e.mu.Unlock()
	if cancel == nil {
		job, err := e.store.GetJob(ctx, jobID)
		if err != nil {
			return err
		}
		if terminal(job.State) {
			return errors.New("job is already complete")
		}
		if err := e.store.UpdateJobState(ctx, jobID, "cancelled", "cancelled at a safe operation boundary"); err != nil {
			return err
		}
		e.cleanupAfterCancel(jobID)
		return nil
	}
	cancel()
	// The running goroutine owns the final state: it may still need to roll
	// back a partial switch, so only cancellation intent is recorded here and
	// the job stays non-terminal until the goroutine finishes.
	marked, err := e.store.MarkCancelling(ctx, jobID)
	if err != nil {
		return err
	}
	_ = marked // false means the goroutine already wrote a terminal state
	return nil
}

func (e *Engine) run(ctx context.Context, jobID string) {
	job, err := e.store.GetJob(ctx, jobID)
	if err != nil {
		return
	}
	r, ok := e.recipeFor(ctx, job.Kind, job.RecipeID)
	if !ok {
		_ = e.store.UpdateJobState(ctx, jobID, "failed", "recipe is no longer available")
		return
	}
	lock := e.recipeLock(r.ID)
	lock.Lock()
	defer lock.Unlock()
	if job.Kind == "install" {
		// A download-only install (activate: false) still writes the same
		// bytes to disk as one that switches, so it reserves the same way.
		e.reserveDisk(jobID, r.RequiredBytes())
		defer e.releaseDisk(jobID)
	}
	runtimeHeld := false
	defer func() {
		if runtimeHeld {
			e.releaseRuntime()
		}
	}()
	execution := operations.Execution{JobID: job.ID, Kind: job.Kind}
	if job.Kind == "remove" {
		var payload RemovePayload
		_ = json.Unmarshal(job.Payload, &payload)
		execution.RemoveArtifacts = payload.RemoveArtifacts
		if payload.RemoveArtifacts {
			shared, sharedErr := e.sharedArtifacts(ctx, r.ID)
			if sharedErr != nil {
				_ = e.store.UpdateJobState(ctx, jobID, "failed", redact.String(sharedErr.Error()))
				return
			}
			execution.SharedArtifacts = shared
		}
	}
	deployment, err := e.deployment(ctx, r)
	if err != nil {
		_ = e.store.UpdateJobState(ctx, jobID, "failed", redact.String(err.Error()))
		return
	}
	// Pinned once, for every step this job will ever run, including its
	// teardown.
	if deployment.Distributed() {
		peer := deployment.Peer
		execution.Peer = &peer
	}
	planned, err := e.plan(ctx, job, r, deployment)
	if err != nil {
		_ = e.store.UpdateJobState(ctx, jobID, "failed", redact.String(err.Error()))
		return
	}
	plans, previous := planned.plans, planned.previous
	if previous != nil && rollbackWasVerified(job) {
		if err := e.store.SetModelState(ctx, r.ID, "stopped", false); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.fail(ctx, jobID, len(plans)-1, fmt.Errorf("recover completed rollback target state: %w", err))
			return
		}
		if err := e.store.SetOnlyActive(ctx, previous.ID); err != nil {
			e.fail(ctx, jobID, len(plans)-1, fmt.Errorf("recover completed rollback active model: %w", err))
			return
		}
		message := strings.TrimSpace(job.Error)
		if message == "" {
			message = "target switch failed before manager restart"
		}
		_ = e.store.UpdateJobState(ctx, jobID, "failed", fmt.Sprintf("%s; previous model %s restored and verified", redact.String(message), previous.ID))
		return
	}
	switchStarted := false
	// abort is the single failure path for this job. A two-Spark job first
	// returns BOTH nodes to stopped, so a failure can never leave one rank
	// running and holding memory on the other Spark, and only then records
	// the failure (restoring a previously active model when this job had
	// already begun switching away from one).
	abort := func(index int, cause error) {
		next := len(plans)
		if deployment.Distributed() {
			next = e.teardownDistributed(job, r, next, deployment)
		}
		if switchStarted && previous != nil {
			e.failSwitch(job, r, deployment, *previous, planned.previousDeployment, next, index, cause)
			return
		}
		e.fail(ctx, jobID, index, cause)
	}
	for index, plan := range plans {
		if !runtimeHeld && (plan.BeginSwitch || runtimeOperations[plan.Operation.Type]) {
			if err := e.acquireRuntime(ctx); err != nil {
				abort(index, err)
				return
			}
			runtimeHeld = true
		}
		if plan.BeginSwitch {
			if previous == nil {
				abort(index, errors.New("switch plan lost the previous model"))
				return
			}
			if err := e.store.BeginSwitch(ctx, previous.ID, r.ID); err != nil {
				abort(index, fmt.Errorf("record switch intent: %w", err))
				return
			}
			switchStarted = true
		}
		if ctx.Err() != nil {
			if switchStarted && previous != nil {
				abort(index, ctx.Err())
			} else {
				_ = e.store.UpdateJobState(context.Background(), jobID, "cancelled", "cancelled at a safe operation boundary")
				e.cleanupAfterCancel(jobID)
			}
			return
		}
		op, target := plan.Operation, plan.Recipe
		execution.Placement = plan.Placement
		previousStep, exists, err := e.store.Step(ctx, jobID, index)
		if err != nil {
			abort(index, err)
			return
		}
		if exists && previousStep.State == "completed" && (plan.Receipt != nil || e.executor.Completed(ctx, execution, op, target, previousStep.Receipt)) {
			continue
		}
		// A state write that fails must not return silently: on a two-Spark
		// job that would leave a worker rank running with nothing left to
		// stop it.
		if err := e.store.UpdateJobState(ctx, jobID, stateFor(job.Kind, op.Type), ""); err != nil {
			abort(index, err)
			return
		}
		if err := e.store.BeginStep(ctx, jobID, index, stepName(op, plan.Placement)); err != nil {
			abort(index, err)
			return
		}
		if plan.Receipt != nil {
			if err := e.store.CompleteStep(ctx, jobID, index, redact.JSON(plan.Receipt)); err != nil {
				abort(index, err)
				return
			}
			continue
		}
		progress := func(value any) error { return e.store.UpdateStepReceipt(ctx, jobID, index, redact.JSON(value)) }
		// Recomputed per step, not once for the whole job: a long download
		// spans other installs starting and finishing around it.
		execution.ReservedBytes = e.reservedByOthers(jobID)
		receipt, err := e.executor.Execute(ctx, execution, op, target, progress)
		if err != nil {
			abort(index, err)
			return
		}
		if ctx.Err() != nil {
			abort(index, ctx.Err())
			return
		}
		if err := e.store.CompleteStep(ctx, jobID, index, redact.JSON(receipt)); err != nil {
			abort(index, err)
			return
		}
	}
	if ctx.Err() != nil {
		if switchStarted && previous != nil {
			abort(len(plans)-1, ctx.Err())
		} else {
			_ = e.store.UpdateJobState(context.Background(), jobID, "cancelled", "cancelled at a safe operation boundary")
			e.cleanupAfterCancel(jobID)
		}
		return
	}
	if err := e.finish(ctx, job, r); err != nil {
		abort(len(plans)-1, err)
		return
	}
}

// jobPlan is everything run() needs to execute and, if necessary, undo one
// job: the steps, the model being switched away from, and the nodes THAT
// model runs on. The previous model's topology is its own and can differ
// from the target's, so it is resolved separately.
type jobPlan struct {
	plans              []plannedOperation
	previous           *recipe.Recipe
	previousDeployment operations.Deployment
}

func (e *Engine) plan(ctx context.Context, job store.Job, target recipe.Recipe, deployment operations.Deployment) (jobPlan, error) {
	var ops []recipe.Operation
	switch job.Kind {
	case "install":
		ops = target.Operations
		var payload InstallPayload
		_ = json.Unmarshal(job.Payload, &payload)
		if !payload.activate() {
			ops = downloadOnlyOperations(ops)
		}
	case "start":
		ops = []recipe.Operation{{Type: "verify_memory"}, {Type: "start_container"}, {Type: "wait_http"}, {Type: "verify_openai_inference"}}
		// A download-only install never wrote the runtime config or created
		// the container (see downloadOnlyOperations); the first start after
		// one has to do both before the usual start sequence, and only then
		// does the host port actually get bound.
		if model, modelErr := e.store.Model(ctx, target.ID); modelErr == nil && model.ContainerID == "" {
			ops = []recipe.Operation{{Type: "verify_port"}, {Type: "write_generated_config"}, {Type: "create_container"}, {Type: "verify_memory"}, {Type: "start_container"}, {Type: "wait_http"}, {Type: "verify_openai_inference"}}
		}
	case "stop":
		ops = []recipe.Operation{{Type: "stop_container"}}
	case "smoke-test":
		ops = []recipe.Operation{{Type: "wait_http"}, {Type: "verify_openai_inference"}}
	case "benchmark":
		ops = []recipe.Operation{{Type: "wait_http"}, {Type: "measure_throughput"}}
	case "remove":
		var payload RemovePayload
		_ = json.Unmarshal(job.Payload, &payload)
		ops = target.Uninstall
	default:
		return jobPlan{}, fmt.Errorf("unknown job kind %s", job.Kind)
	}
	plans := make([]plannedOperation, 0, len(ops)+1)
	if job.Kind != "install" && job.Kind != "start" {
		if target.Distributed() {
			return jobPlan{plans: distributedPlans(ops, target, deployment, nil, operations.Deployment{})}, nil
		}
		for _, op := range ops {
			plans = append(plans, plannedOperation{Operation: op, Recipe: target})
		}
		return jobPlan{plans: plans}, nil
	}
	previous, err := e.activeRecipe(ctx, target)
	if err != nil {
		return jobPlan{}, err
	}
	// The model being switched away from is stopped on the nodes IT runs on.
	// Reading the target's topology here is what leaves a distributed
	// predecessor's worker rank alive when a single-node model takes over.
	var previousDeployment operations.Deployment
	if previous != nil {
		previousDeployment, err = e.deployment(ctx, *previous)
		if err != nil {
			return jobPlan{}, err
		}
	}
	if target.Distributed() {
		return jobPlan{plans: distributedPlans(ops, target, deployment, previous, previousDeployment), previous: previous, previousDeployment: previousDeployment}, nil
	}
	if previous == nil {
		for _, op := range ops {
			plans = append(plans, plannedOperation{Operation: op, Recipe: target})
		}
		return jobPlan{plans: plans}, nil
	}
	switchStopPlanned := false
	for _, op := range ops {
		if (job.Kind == "install" || job.Kind == "start") && op.Type == "verify_port" && previous.Service.DefaultHostPort == target.Service.DefaultHostPort {
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Receipt: map[string]any{"host_port": target.Service.DefaultHostPort, "occupied_by_managed_recipe": previous.ID, "available_after_switch": true}})
			continue
		}
		if !switchStopPlanned && (op.Type == "verify_memory" || op.Type == "start_container") {
			plans = append(plans, previousStopPlans(*previous, previousDeployment)...)
			switchStopPlanned = true
		}
		plans = append(plans, plannedOperation{Operation: op, Recipe: target})
	}
	return jobPlan{plans: plans, previous: previous, previousDeployment: previousDeployment}, nil
}

// previousStopPlans stops every rank of the model being switched away from.
// A distributed predecessor needs both of its containers stopped, head first
// so it stops serving before its worker rank goes away.
func previousStopPlans(previous recipe.Recipe, deployment operations.Deployment) []plannedOperation {
	stop := recipe.Operation{Type: "stop_container"}
	if !deployment.Distributed() {
		return []plannedOperation{{Operation: stop, Recipe: previous, BeginSwitch: true}}
	}
	return []plannedOperation{
		{Operation: stop, Recipe: previous, BeginSwitch: true, Placement: deployment.Head},
		{Operation: stop, Recipe: previous, Placement: deployment.Worker},
	}
}

// placements resolves both nodes of a distributed serve before any step
// runs, so a missing peer, an unusable interconnect, or a manager that
// cannot reach a second Spark at all fails the job before it touches
// anything. Single-Spark recipes get two zero placements and never consult
// the fleet.
func (e *Engine) deployment(ctx context.Context, r recipe.Recipe) (operations.Deployment, error) {
	if !r.Distributed() {
		return operations.Deployment{}, nil
	}
	fleet, ok := e.executor.(operations.Fleet)
	if !ok {
		return operations.Deployment{}, errors.New("this manager is not configured to run a model across two Sparks")
	}
	return fleet.Plan(ctx, r)
}

// distributedPlans expands a single-node operation list into the two-node
// order the community two-Spark recipe requires: the head verifies itself,
// the worker manager verifies itself, both nodes stage the image, weights
// and config, the worker container starts first, the head container starts
// second, and only the head is health-checked and inference-tested.
// Teardown steps run head first so serving stops before the worker rank
// disappears underneath it.
func distributedPlans(ops []recipe.Operation, target recipe.Recipe, deployment operations.Deployment, previous *recipe.Recipe, previousDeployment operations.Deployment) []plannedOperation {
	head, worker := deployment.Head, deployment.Worker
	var plans, workerBringUp, headBringUp, tail []plannedOperation
	peerChecked := false
	checkPeer := func() {
		if peerChecked {
			return
		}
		plans = append(plans, plannedOperation{Operation: recipe.Operation{Type: operations.VerifyPeerNode}, Recipe: target, Placement: worker})
		peerChecked = true
	}
	for _, op := range ops {
		switch op.Type {
		case "create_container", "verify_memory", "start_container":
			workerBringUp = append(workerBringUp, plannedOperation{Operation: op, Recipe: target, Placement: worker})
			headBringUp = append(headBringUp, plannedOperation{Operation: op, Recipe: target, Placement: head})
		case "wait_http", "verify_openai_inference", "measure_throughput":
			tail = append(tail, plannedOperation{Operation: op, Recipe: target, Placement: head})
		case "pull_image", "download_artifact", "write_generated_config":
			checkPeer()
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Placement: head})
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Placement: worker})
		case "stop_container", "remove_container", "remove_artifact_if_unshared":
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Placement: head})
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Placement: worker})
		case "verify_port":
			if previous != nil && previous.Service.DefaultHostPort == target.Service.DefaultHostPort {
				plans = append(plans, plannedOperation{Operation: op, Recipe: target, Placement: head, Receipt: map[string]any{"host_port": target.Service.DefaultHostPort, "occupied_by_managed_recipe": previous.ID, "available_after_switch": true}})
				continue
			}
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Placement: head})
		default:
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Placement: head})
		}
	}
	if len(workerBringUp) > 0 {
		// A start job carries no staging operations, so the worker's own
		// guardrails would otherwise never be consulted before its rank runs.
		checkPeer()
		if previous != nil {
			plans = append(plans, previousStopPlans(*previous, previousDeployment)...)
		}
	}
	plans = append(plans, workerBringUp...)
	plans = append(plans, headBringUp...)
	return append(plans, tail...)
}

// stepName is what a step is recorded as. A distributed job runs the same
// operation twice, so the node's role is part of the name and a stored
// timeline can never be read as one node doing the work twice.
func stepName(op recipe.Operation, placement operations.Placement) string {
	if !placement.Distributed() {
		return op.Type
	}
	return op.Type + ":" + placement.Role
}

// teardownDistributed returns both Sparks to stopped after any step of a
// two-node job failed, and reports the next free step index. The head is
// stopped first so it stops serving before its worker rank goes away. Best
// effort by design: a teardown problem is recorded on its own step and never
// allowed to mask the original failure.
func (e *Engine) teardownDistributed(job store.Job, target recipe.Recipe, base int, deployment operations.Deployment) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	peer := deployment.Peer
	execution := operations.Execution{JobID: job.ID, Kind: "teardown", Peer: &peer}
	for offset, placement := range []operations.Placement{deployment.Head, deployment.Worker} {
		index := base + offset
		// Receipts are best effort here. Stopping the containers is not: a
		// database that cannot be written must never be the reason a rank
		// keeps holding memory on either Spark.
		_ = e.store.BeginStep(ctx, job.ID, index, stepName(recipe.Operation{Type: "teardown_stop_container"}, placement))
		execution.Placement = placement
		receipt, err := e.executor.Execute(ctx, execution, recipe.Operation{Type: "stop_container"}, target, nil)
		if err != nil {
			_ = e.store.FailStep(context.Background(), job.ID, index, redact.String(err.Error()))
			continue
		}
		_ = e.store.CompleteStep(ctx, job.ID, index, redact.JSON(receipt))
	}
	return base + 2
}

// downloadOnlyOperations trims an install plan to what a download-only
// install needs: the runtime image and model artifacts, verified. It stops
// before the first operation that touches the container, and drops
// verify_port because a job that never binds the port cannot conflict on it
// - required so a download-only install stays possible while another model
// serves on the (currently shared) host port.
func downloadOnlyOperations(ops []recipe.Operation) []recipe.Operation {
	trimmed := make([]recipe.Operation, 0, len(ops))
	for _, op := range ops {
		switch op.Type {
		case "write_generated_config", "create_container", "verify_memory", "start_container", "wait_http", "verify_openai_inference":
			return trimmed
		case "verify_port":
			continue
		default:
			trimmed = append(trimmed, op)
		}
	}
	return trimmed
}

func (e *Engine) sharedArtifacts(ctx context.Context, excludeRecipeID string) (map[string]bool, error) {
	models, err := e.store.Models(ctx)
	if err != nil {
		return nil, err
	}
	shared := map[string]bool{}
	for _, model := range models {
		if model.RecipeID == excludeRecipeID {
			continue
		}
		if model.ArtifactPath != "" {
			shared[model.ArtifactPath] = true
		}
		// Use the exact version this other model was installed with, not
		// whatever the catalog now has for its ID: an artifact revision can
		// change between recipe versions, and understating what is still in
		// use here is what protects it from remove_artifact_if_unshared.
		other, ok := e.pinnedOrEffective(model.RecipeID, model.RecipeVersion)
		if !ok {
			continue
		}
		for _, artifact := range other.Artifacts {
			shared[operations.ArtifactKey(artifact)] = true
		}
	}
	return shared, nil
}

// pinnedOrEffective resolves a recipe the same way recipeFor does for a
// non-install job: prefer the exact installed version, falling back to
// whatever the effective catalog currently has for that ID.
func (e *Engine) pinnedOrEffective(id string, version int) (recipe.Recipe, bool) {
	if pinned, ok := recipe.FindVersion(e.allRecipes(), id, version); ok {
		return pinned, true
	}
	return recipe.Find(e.effectiveRecipes(), id)
}

// activeRecipe finds the model this job's target must be switched from, if
// any: another actively-serving recipe, or the same recipe ID actively
// serving an older version (an in-place update). Either way the result is
// resolved to the EXACT version that model was installed with (never the
// live catalog's current entry for that ID), because plan() uses it both to
// stop the real running container and, on rollback, to restart it — and
// only the exact installed version reliably names that container
// (host.go's containerName embeds the version).
func (e *Engine) activeRecipe(ctx context.Context, target recipe.Recipe) (*recipe.Recipe, error) {
	models, err := e.store.Models(ctx)
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		if !model.Active {
			continue
		}
		if model.RecipeID == target.ID && model.RecipeVersion == target.Version {
			continue // already exactly this version; nothing to switch from
		}
		active, ok := e.pinnedOrEffective(model.RecipeID, model.RecipeVersion)
		if !ok {
			return nil, fmt.Errorf("active model recipe %s is no longer available", model.RecipeID)
		}
		return &active, nil
	}
	return nil, nil
}

func rollbackWasVerified(job store.Job) bool {
	for _, step := range job.Steps {
		// A distributed rollback names the node it ran on, so the recorded
		// step is "rollback_verify_openai_inference:head".
		if strings.HasPrefix(step.Operation, "rollback_verify_openai_inference") && step.State == "completed" {
			return true
		}
	}
	return false
}

// rollbackPlans stops every rank of the model that failed and brings every
// rank of the previous model back, each according to its own topology. A
// worker rank comes up before the head that talks to it, exactly as a fresh
// distributed start does.
func rollbackPlans(target recipe.Recipe, targetDeployment operations.Deployment, previous recipe.Recipe, previousDeployment operations.Deployment) []plannedOperation {
	stop := recipe.Operation{Type: "stop_container"}
	plans := []plannedOperation{{Operation: stop, Recipe: target, Placement: targetDeployment.Head}}
	if targetDeployment.Distributed() {
		plans = append(plans, plannedOperation{Operation: stop, Recipe: target, Placement: targetDeployment.Worker})
	}
	if previousDeployment.Distributed() {
		for _, op := range []string{"verify_memory", "start_container"} {
			plans = append(plans, plannedOperation{Operation: recipe.Operation{Type: op}, Recipe: previous, Placement: previousDeployment.Worker})
		}
	}
	for _, op := range []string{"verify_memory", "start_container", "wait_http", "verify_openai_inference"} {
		plans = append(plans, plannedOperation{Operation: recipe.Operation{Type: op}, Recipe: previous, Placement: previousDeployment.Head})
	}
	return plans
}

// failSwitch restores the model this job switched away from, on the nodes
// THAT model runs on. nextIndex is the first step index still free for this
// job, so rollback receipts never overwrite a planned step or a distributed
// teardown receipt.
func (e *Engine) failSwitch(job store.Job, target recipe.Recipe, targetDeployment operations.Deployment, previous recipe.Recipe, previousDeployment operations.Deployment, nextIndex, failedIndex int, cause error) {
	original := redact.String(cause.Error())
	_ = e.store.FailStep(context.Background(), job.ID, failedIndex, original)
	_ = e.store.UpdateJobState(context.Background(), job.ID, "rolling_back", original)
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	rollback := rollbackPlans(target, targetDeployment, previous, previousDeployment)
	var rollbackErr error
	for offset, plan := range rollback {
		index := nextIndex + offset
		name := "rollback_" + stepName(plan.Operation, plan.Placement)
		execution := operations.Execution{JobID: job.ID, Kind: "rollback", Placement: plan.Placement}
		if plan.Placement.Distributed() {
			// Each half of the rollback is pinned to the deployment it
			// belongs to: the target's peer for stopping the target, the
			// previous model's peer for bringing it back.
			peer := previousDeployment.Peer
			if plan.Recipe.ID == target.ID {
				peer = targetDeployment.Peer
			}
			execution.Peer = &peer
		}
		if err := e.store.BeginStep(rollbackCtx, job.ID, index, name); err != nil {
			rollbackErr = err
			break
		}
		if e.executor.Completed(rollbackCtx, execution, plan.Operation, plan.Recipe, nil) {
			if err := e.store.CompleteStep(rollbackCtx, job.ID, index, redact.JSON(map[string]any{"operation": plan.Operation.Type, "already_satisfied": true})); err != nil {
				rollbackErr = err
				break
			}
			continue
		}
		receipt, err := e.executor.Execute(rollbackCtx, execution, plan.Operation, plan.Recipe, nil)
		if err != nil {
			rollbackErr = err
			_ = e.store.FailStep(context.Background(), job.ID, index, redact.String(err.Error()))
			break
		}
		if err := e.store.CompleteStep(rollbackCtx, job.ID, index, redact.JSON(receipt)); err != nil {
			rollbackErr = err
			break
		}
	}
	finalState := "failed"
	if errors.Is(cause, context.Canceled) {
		finalState = "cancelled"
	}
	message := original
	if rollbackErr == nil {
		if err := e.store.SetModelState(context.Background(), target.ID, "stopped", false); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErr = err
		}
	}
	if rollbackErr == nil {
		if err := e.store.SetOnlyActive(context.Background(), previous.ID); err != nil {
			rollbackErr = err
		}
	}
	if rollbackErr == nil {
		message = fmt.Sprintf("%s; previous model %s restored and verified", original, previous.ID)
	} else {
		_ = e.store.SetModelState(context.Background(), target.ID, "failed", false)
		_ = e.store.SetModelState(context.Background(), previous.ID, "failed", false)
		message = fmt.Sprintf("%s; rollback to %s failed: %s", original, previous.ID, redact.String(rollbackErr.Error()))
	}
	_ = e.store.UpdateJobState(context.Background(), job.ID, finalState, message)
}

func (e *Engine) finish(ctx context.Context, job store.Job, r recipe.Recipe) error {
	switch job.Kind {
	case "install", "start":
		if job.Kind == "install" {
			var payload InstallPayload
			_ = json.Unmarshal(job.Payload, &payload)
			if !payload.activate() {
				model := store.InstalledModel{RecipeID: r.ID, RecipeVersion: r.Version, Status: "stopped", ArtifactPath: e.executor.ArtifactPath(r), Active: false}
				if err := e.store.SetInstalled(ctx, model); err != nil {
					return err
				}
				return e.store.UpdateJobState(ctx, job.ID, "ready", "")
			}
		}
		containerID := e.containerID(ctx, job.ID)
		if existing, err := e.store.Model(ctx, r.ID); err == nil && containerID == "" {
			containerID = existing.ContainerID
		}
		model := store.InstalledModel{RecipeID: r.ID, RecipeVersion: r.Version, Status: "ready", ArtifactPath: e.executor.ArtifactPath(r), ContainerID: containerID, Active: true}
		if err := e.store.ActivateExclusively(ctx, model); err != nil {
			return err
		}
		if err := e.store.UpdateJobState(ctx, job.ID, "ready", ""); err != nil {
			return err
		}
		e.autoBenchmark(ctx, r)
		return nil
	case "stop":
		model, err := e.store.Model(ctx, r.ID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			model.Status, model.Active = "stopped", false
			if err := e.store.SetInstalled(ctx, model); err != nil {
				return err
			}
		}
		return e.store.UpdateJobState(ctx, job.ID, "stopped", "")
	case "remove":
		if err := e.store.DeleteModel(ctx, r.ID); err != nil {
			return err
		}
		return e.store.UpdateJobState(ctx, job.ID, "removed", "")
	case "smoke-test":
		return e.store.UpdateJobState(ctx, job.ID, "ready", "")
	case "benchmark":
		if tps, ttft, ok := e.benchmarkResult(ctx, job.ID); ok {
			if err := e.store.SetModelMetrics(ctx, r.ID, tps, ttft); err != nil {
				return err
			}
		}
		return e.store.UpdateJobState(ctx, job.ID, "ready", "")
	default:
		return fmt.Errorf("cannot finish job kind %s", job.Kind)
	}
}

// autoBenchmark measures real throughput once per model after its first
// successful activation, so the catalog can show numbers observed on this
// device instead of editorial claims. Failures never affect the parent job.
func (e *Engine) autoBenchmark(ctx context.Context, r recipe.Recipe) {
	model, err := e.store.Model(ctx, r.ID)
	if err != nil || model.MeasuredAt != "" {
		return
	}
	job, created, err := e.store.CreateJob(ctx, "benchmark", r.ID, fmt.Sprintf("auto-benchmark-%s-v%d", r.ID, r.Version), map[string]any{"auto": true})
	if err != nil || !created {
		return
	}
	e.Start(job.ID)
}

func (e *Engine) benchmarkResult(ctx context.Context, jobID string) (float64, int64, bool) {
	job, err := e.store.GetJob(ctx, jobID)
	if err != nil {
		return 0, 0, false
	}
	for _, step := range job.Steps {
		if step.Operation != "measure_throughput" {
			continue
		}
		var receipt struct {
			TokensPerSecond    float64 `json:"tokens_per_second"`
			TimeToFirstTokenMS int64   `json:"time_to_first_token_ms"`
		}
		if json.Unmarshal(step.Receipt, &receipt) == nil && receipt.TokensPerSecond > 0 {
			return receipt.TokensPerSecond, receipt.TimeToFirstTokenMS, true
		}
	}
	return 0, 0, false
}

func (e *Engine) containerID(ctx context.Context, jobID string) string {
	job, err := e.store.GetJob(ctx, jobID)
	if err != nil {
		return ""
	}
	for _, step := range job.Steps {
		var receipt map[string]any
		if json.Unmarshal(step.Receipt, &receipt) == nil {
			if id, ok := receipt["container_id"].(string); ok && id != "" {
				return id
			}
		}
	}
	return ""
}

func (e *Engine) fail(ctx context.Context, jobID string, index int, err error) {
	message := redact.String(err.Error())
	state := "failed"
	if errors.Is(err, context.Canceled) {
		state = "cancelled"
		// A cancellation is the user's own decision, not a fault; the raw
		// "context canceled" chain reads like a crash in the console.
		message = "cancelled while this step was running"
	}
	_ = e.store.FailStep(context.Background(), jobID, index, message)
	_ = e.store.UpdateJobState(context.Background(), jobID, state, message)
	if state == "cancelled" {
		e.cleanupAfterCancel(jobID)
	}
}

// cleanupAfterCancel frees what a cancelled install or start left running: a
// started container otherwise keeps holding its port and memory, and the
// next install fails preflight on its own leftovers. Best-effort by design.
func (e *Engine) cleanupAfterCancel(jobID string) {
	background, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	job, err := e.store.GetJob(background, jobID)
	if err != nil || (job.Kind != "install" && job.Kind != "start") {
		return
	}
	r, ok := e.recipeFor(background, job.Kind, job.RecipeID)
	if !ok {
		return
	}
	if r.Distributed() {
		if deployment, err := e.deployment(background, r); err == nil {
			peer := deployment.Peer
			for _, placement := range []operations.Placement{deployment.Head, deployment.Worker} {
				_, _ = e.executor.Execute(background, operations.Execution{Kind: job.Kind, Placement: placement, Peer: &peer}, recipe.Operation{Type: "stop_container"}, r, nil)
			}
			return
		}
	}
	_, _ = e.executor.Execute(background, operations.Execution{Kind: job.Kind}, recipe.Operation{Type: "stop_container"}, r, nil)
}

func stateFor(kind, operation string) string {
	states := map[string]string{
		"verify_architecture": "preflighting", "verify_dgx_spark": "preflighting", "verify_memory_capacity": "preflighting", "verify_memory": "checking_memory", "verify_disk": "preflighting", "verify_port": "preflighting", "verify_docker": "preflighting", "verify_nvidia_runtime": "preflighting", "verify_artifact_access": "preflighting",
		"pull_image": "downloading_runtime", "download_artifact": "downloading_models", "write_generated_config": "configuring", "create_container": "configuring",
		operations.VerifyPeerNode: "preflighting",
		"start_container":         "starting", "wait_http": "verifying_health", "verify_openai_inference": "verifying_inference", "stop_container": "stopping", "remove_container": "removing", "remove_artifact_if_unshared": "removing", "measure_throughput": "benchmarking",
	}
	if state := states[operation]; state != "" {
		return state
	}
	return kind
}

func terminal(state string) bool {
	switch state {
	case "ready", "failed", "cancelled", "stopped", "removed":
		return true
	}
	return false
}
