package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/redact"
	"github.com/punkjazz-labs/basement/internal/store"
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
	// reservations is this node's persistent disk, runtime, and port
	// authority. The runtime channel still serializes execution inside this
	// process, but only the allocator decides whether local or remote work may
	// own the node across a manager restart.
	reservations *fleet.Allocator
	// tokenSampler is called just before a local container is stopped; see
	// SetTokenSampler. Held atomically because it is installed after the
	// engine may already be resuming interrupted jobs.
	tokenSampler atomic.Pointer[TokenSampler]
	// switchGuard is taken around the part of a job that changes which model
	// serves; see SetSwitchGuard. Held atomically for the same reason.
	switchGuard atomic.Pointer[SwitchGuard]
}

// TokenSampler takes a reading of a model's runtime token counters.
type TokenSampler func(ctx context.Context, r recipe.Recipe)

// SetTokenSampler installs the hook the engine calls immediately before it
// stops a container on this machine. The counters the sampler reads live
// inside that container, so this is the last moment they can be read; with
// no sampler installed, nothing is counted. Safe to call from any goroutine.
func (e *Engine) SetTokenSampler(sample TokenSampler) { e.tokenSampler.Store(&sample) }

func (e *Engine) sampleTokens(ctx context.Context, r recipe.Recipe) {
	if sample := e.tokenSampler.Load(); sample != nil && *sample != nil {
		(*sample)(ctx, r)
	}
}

type RemovePayload struct {
	RemoveArtifacts bool `json:"remove_artifacts"`
}

// InstallPayload carries the console's activation choice. A nil Activate
// means true, so jobs created before this field existed keep installing and
// serving immediately.
type InstallPayload struct {
	Activate      *bool  `json:"activate,omitempty"`
	ReservationID string `json:"reservation_id,omitempty"`
	DeploymentID  string `json:"deployment_id,omitempty"`
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
	e := &Engine{store: s, executor: executor, running: map[string]context.CancelFunc{}, recipeLocks: map[string]*sync.Mutex{}, runtime: make(chan struct{}, 1), reservations: fleet.NewAllocator(s, "local")}
	e.SetRecipes(recipes, recipes)
	return e
}

// SetReservationAllocator installs the allocator coupled to this manager's
// durable node identity. New provides a local default so single-node callers
// and tests keep the same construction path, while production replaces it
// before any job starts.
func (e *Engine) SetReservationAllocator(allocator *fleet.Allocator) {
	if allocator != nil {
		e.reservations = allocator
	}
}

func (e *Engine) Reservations() *fleet.Allocator { return e.reservations }

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

// recipeForJob keeps a prepared job on the exact recipe that its durable
// reservation admitted. A catalogue refresh can advance the effective recipe
// while the manager is down, but resuming that job with the newer recipe would
// change both its resource meaning and the bytes its completed steps describe.
func (e *Engine) recipeForJob(ctx context.Context, job store.Job) (recipe.Recipe, bool) {
	if job.Kind != "install" && job.Kind != "start" {
		return e.recipeFor(ctx, job.Kind, job.RecipeID)
	}
	var payload reservationPayload
	_ = json.Unmarshal(job.Payload, &payload)
	reservationID := payload.ReservationID
	if reservationID == "" {
		reservationID = fleet.ReservationID(fleet.ClaimKindLocalJob, job.ID)
	}
	reservation, err := e.reservations.Reservation(ctx, reservationID)
	if errors.Is(err, os.ErrNotExist) {
		return e.recipeFor(ctx, job.Kind, job.RecipeID)
	}
	if err != nil || reservation.RecipeID != job.RecipeID {
		return recipe.Recipe{}, false
	}
	pinned, ok := recipe.FindVersion(e.allRecipes(), reservation.RecipeID, reservation.RecipeVersion)
	if !ok {
		return recipe.Recipe{}, false
	}
	fingerprint, err := fleet.RecipeFingerprint(pinned)
	if err != nil || fingerprint != reservation.RecipeFingerprint {
		return recipe.Recipe{}, false
	}
	return pinned, true
}

type reservationPayload struct {
	ReservationID string `json:"reservation_id,omitempty"`
	DeploymentID  string `json:"deployment_id,omitempty"`
}

func (e *Engine) prepareJobReservation(ctx context.Context, job store.Job, selected recipe.Recipe) (string, bool, error) {
	if job.Kind != "install" && job.Kind != "start" {
		return "", false, nil
	}
	var payload reservationPayload
	_ = json.Unmarshal(job.Payload, &payload)
	reservationID := payload.ReservationID
	if reservationID == "" {
		reservationID = fleet.ReservationID(fleet.ClaimKindLocalJob, job.ID)
		if err := e.recordJobReservationID(ctx, job, reservationID); err != nil {
			return "", false, err
		}
	} else if payload.DeploymentID == "" {
		if existing, err := e.reservations.Reservation(ctx, reservationID); err == nil && reservationStateTerminal(existing.State) {
			reservationID = fleet.ReservationID(fleet.ClaimKindLocalJob, job.ID+":"+job.UpdatedAt+":"+fmt.Sprint(time.Now().UnixNano()))
			if err := e.recordJobReservationID(ctx, job, reservationID); err != nil {
				return "", false, err
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
	}
	activate := job.Kind == "start"
	if job.Kind == "install" {
		var install InstallPayload
		_ = json.Unmarshal(job.Payload, &install)
		activate = install.activate()
	}
	kind := fleet.ClaimKindLocalJob
	if payload.DeploymentID != "" {
		kind = fleet.ClaimKindIndependent
	}
	claimJobID := ""
	if kind == fleet.ClaimKindLocalJob {
		claimJobID = job.ID
	}
	claims := fleet.ClaimsForRecipe(selected, fleet.RecipeClaimOptions{
		Kind: kind, JobID: claimJobID, ReserveDisk: job.Kind == "install", Runtime: activate,
	})
	fingerprint, err := fleet.RecipeFingerprint(selected)
	if err != nil {
		return "", false, err
	}
	if existing, err := e.reservations.Reservation(ctx, reservationID); err == nil {
		if existing.RecipeID != selected.ID || existing.RecipeVersion != selected.Version || existing.RecipeFingerprint != fingerprint ||
			existing.Claims.Kind != claims.Kind || existing.Claims.JobID != claims.JobID || existing.Claims.DiskBytes != claims.DiskBytes ||
			existing.Claims.MemoryBytes != claims.MemoryBytes || existing.Claims.Runtime != claims.Runtime ||
			!samePorts(existing.Claims.Ports, claims.Ports) || !sameStrings(existing.Claims.FabricInterfaces, claims.FabricInterfaces) {
			return "", false, store.ErrReservationRetryConflict
		}
		if existing.State != "committed" && existing.State != "active" {
			return "", false, fmt.Errorf("reservation is %s and cannot run job %s", existing.State, job.ID)
		}
		if err := e.reservations.AttachJob(ctx, reservationID, job.ID); err != nil {
			return "", false, err
		}
		return reservationID, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	deploymentID := payload.DeploymentID
	if deploymentID == "" {
		deploymentID = "job:" + job.ID
	}
	prepared, _, err := e.reservations.Prepare(ctx, fleet.ReservationRequest{
		ReservationID: reservationID, DeploymentID: deploymentID, DriverNodeID: e.reservations.NodeID(),
		RecipeID: selected.ID, RecipeVersion: selected.Version, RecipeFingerprint: fingerprint, Claims: claims,
	})
	if err != nil {
		return "", false, err
	}
	if prepared.State == "prepared" {
		if _, err := e.reservations.Commit(ctx, reservationID, fleet.LocalPrepareToken(reservationID), []byte(`{"kind":"local-engine"}`)); err != nil {
			return "", false, err
		}
	}
	if err := e.reservations.AttachJob(ctx, reservationID, job.ID); err != nil {
		return "", false, err
	}
	return reservationID, true, nil
}

func (e *Engine) recordJobReservationID(ctx context.Context, job store.Job, reservationID string) error {
	payload := map[string]any{}
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("decode job payload before reserving resources: %w", err)
		}
	}
	payload["reservation_id"] = reservationID
	return e.store.UpdateJobPayload(ctx, job.ID, payload)
}

func reservationStateTerminal(state string) bool {
	switch state {
	case "released", "aborted", "expired":
		return true
	}
	return false
}

func samePorts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// ReconcileReservations restores persistent admission before startup resumes
// jobs or health-checks a serving model. It is separate from New because the
// caller must surface a database or catalogue inconsistency rather than hide
// it inside a constructor that historically cannot fail.
func (e *Engine) ReconcileReservations(ctx context.Context) error {
	return e.reservations.Reconcile(ctx, e.allRecipes())
}

// RenewDistributedReservations proves that this manager still owns every
// worker rank supporting one of its active local jobs. The job id and exact
// recipe come from the durable local reservation, so restart does not depend
// on an in-memory lease or whichever recipe is currently effective.
func (e *Engine) RenewDistributedReservations(ctx context.Context) error {
	renewer, ok := e.executor.(operations.DriverLeaseRenewer)
	if !ok {
		return nil
	}
	reservations, err := e.reservations.AllReservations(ctx)
	if err != nil {
		return err
	}
	var joined error
	for _, reservation := range reservations {
		if reservation.State != "active" || reservation.Claims.Kind != fleet.ClaimKindLocalJob || reservation.Claims.JobID == "" {
			continue
		}
		selected, ok := recipe.FindVersion(e.allRecipes(), reservation.RecipeID, reservation.RecipeVersion)
		if !ok || !selected.Distributed() {
			continue
		}
		fingerprint, fingerprintErr := fleet.RecipeFingerprint(selected)
		if fingerprintErr != nil || fingerprint != reservation.RecipeFingerprint {
			joined = errors.Join(joined, fmt.Errorf("resolve worker reservation recipe %s version %d", reservation.RecipeID, reservation.RecipeVersion))
			continue
		}
		if err := renewer.RenewWorkerReservation(ctx, reservation.Claims.JobID, selected); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

// PrepareJob makes local API acknowledgement follow durable admission. Start
// still repeats the same idempotent check because jobs may also be created by
// startup recovery or package-level callers that do not use the HTTP layer.
func (e *Engine) PrepareJob(ctx context.Context, jobID string) error {
	job, err := e.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	selected, ok := e.recipeForJob(ctx, job)
	if !ok {
		return errors.New("recipe is no longer available")
	}
	_, _, err = e.prepareJobReservation(ctx, job, selected)
	return err
}

// ReservedDiskBytes reports the total disk currently reserved by running
// install jobs, for callers outside any job — the advisory preflight uses
// it so the dialog's disk check agrees with what the real verify_disk step
// will conclude while another install runs.
func (e *Engine) ReservedDiskBytes() int64 {
	return e.reservedByOthers("")
}

func (e *Engine) ReservedDiskBytesExcept(reservationID string) int64 {
	return e.reservedByOthers(reservationID)
}

// reservedByOthers sums every other job's disk reservation, excluding
// jobID's own, so a job never counts its own bytes against itself.
func (e *Engine) reservedByOthers(reservationID string) int64 {
	total, err := e.reservations.ReservedDiskBytes(context.Background(), reservationID)
	if err != nil {
		// Understating a reservation after a database error could admit two
		// downloads that do not fit. MaxInt64 makes verify_disk fail closed.
		return math.MaxInt64
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
	"verify_media_generation": true,
	"remove_container":        true, "measure_throughput": true,
}

// switchesServing lists the job kinds that change which model this machine
// serves, and therefore have to announce themselves to whatever is waiting on
// the answer (see SetSwitchGuard). A smoke test and a benchmark run against
// the model that is already serving and change nothing, so they are not here.
var switchesServing = map[string]bool{
	"start": true, "install": true, "stop": true, "remove": true,
}

// SwitchGuard is called just before a job starts changing which model serves,
// and the function it returns is called when that job is finished changing it.
// It is how the HTTP layer keeps a request that has been let through to one
// model from being cut off by a switch to another one, whoever asked for the
// switch: a request naming a role, the console's Start button, or an install
// that activates what it downloaded.
type SwitchGuard func(ctx context.Context, recipeID string) func()

// SetSwitchGuard installs that hook. With none installed nothing coordinates,
// which is what the engine's own tests want. Safe to call from any goroutine.
func (e *Engine) SetSwitchGuard(guard SwitchGuard) { e.switchGuard.Store(&guard) }

func (e *Engine) holdSwitch(ctx context.Context, recipeID string) func() {
	if guard := e.switchGuard.Load(); guard != nil && *guard != nil {
		return (*guard)(ctx, recipeID)
	}
	return func() {}
}

func sameRecipeVersion(left, right *recipe.Recipe) bool {
	return left != nil && right != nil && left.ID == right.ID && left.Version == right.Version
}

func samePlannedStep(left, right plannedOperation) bool {
	return left.Operation.Type == right.Operation.Type &&
		left.Recipe.ID == right.Recipe.ID && left.Recipe.Version == right.Recipe.Version &&
		left.Placement == right.Placement
}

// adaptActivationPlan re-reads what serves only after this job owns the
// runtime lock and the switch hold. An install can spend an hour outside
// those locks, but no engine job can change the serving model after this
// reading until the adapted switch finishes. Reusing plan is important: it
// resolves the serving model's installed recipe version and its own
// deployment, then sends both single-node and distributed predecessors
// through the same stop, peer, BeginSwitch, and rollback machinery used by
// a plan made immediately before activation.
func (e *Engine) adaptActivationPlan(ctx context.Context, job store.Job, target recipe.Recipe, deployment operations.Deployment, current jobPlan, completed int) (jobPlan, bool, error) {
	actual, err := e.activeRecipe(ctx, target)
	if err != nil {
		return jobPlan{}, false, fmt.Errorf("re-read the serving model before activating %s: %w; fix the reported manager or catalog error, then run this job again", target.DisplayName, err)
	}
	if actual == nil || sameRecipeVersion(actual, current.previous) {
		return current, false, nil
	}
	refreshed, err := e.plan(ctx, job, target, deployment)
	if err != nil {
		return jobPlan{}, false, fmt.Errorf("%s took over serving while %s was preparing, but its switch plan could not be resolved: %w; fix that model's fleet or recipe configuration, then run this job again", actual.DisplayName, target.DisplayName, err)
	}
	if !sameRecipeVersion(actual, refreshed.previous) {
		return jobPlan{}, false, fmt.Errorf("%s took over serving while %s was preparing, but the serving model changed again before a safe switch plan could be recorded; run this job again", actual.DisplayName, target.DisplayName)
	}
	// completed is the index of the step about to run, so both plans must have
	// a step at that index for the caller to resume into. Allowing equality
	// here would let the caller index one past the end of the refreshed plan
	// and panic the engine goroutine, which takes the whole manager down.
	if completed >= len(current.plans) || completed >= len(refreshed.plans) {
		return jobPlan{}, false, fmt.Errorf("%s took over serving while %s was preparing, but the completed steps no longer fit a safe switch plan; run this job again", actual.DisplayName, target.DisplayName)
	}
	for index := 0; index < completed; index++ {
		// Completed steps keep their existing indices and receipts. If a
		// refreshed plan changed any operation, recipe version, or node in
		// that prefix, inserting a switch would make the durable timeline
		// claim those old receipts proved different work. That case cannot be
		// adapted without rewriting job history, so it fails with a retry
		// instruction instead.
		if !samePlannedStep(current.plans[index], refreshed.plans[index]) {
			return jobPlan{}, false, fmt.Errorf("%s took over serving while %s was preparing, but completed step %d no longer matches a safe switch plan; run this job again", actual.DisplayName, target.DisplayName, index)
		}
	}
	if err := e.refreshCompletedPortReceipt(ctx, job.ID, target, current.plans, refreshed.plans, completed); err != nil {
		return jobPlan{}, false, fmt.Errorf("record the activation-time port check for %s: %w", target.DisplayName, err)
	}
	return refreshed, true, nil
}

// reconcileRunningContainers compares Docker reality against the switch plan,
// in the same window adaptActivationPlan runs in: this job holds the runtime
// lock and the switch hold, so nothing can legitimately start a container
// until the adapted plan finishes. The plan so far was built entirely from
// the store's active-model pointer, and hardware proved that pointer can lie:
// a failed install whose rollback restarted the previous model's container at
// the Docker level can leave the store naming a model that is not the one
// actually serving, so the plan's stop step no-ops and the target starts
// BESIDE the running model — two models in unified memory, and the first real
// request dies in the driver. Any running basement-labeled container that is
// neither the exact target (reinstalling the running version reuses its
// container: the Create 409 and Start 304 paths, and verify_memory's
// already_running short-circuit) nor already covered by a planned stop gets a
// real stop_container step inserted where previousStopPlans would have put
// it, so receipts and the job timeline record the stop like any other switch.
//
// Only THIS node's daemon is consulted (see FleetExecutor.ManagedContainers).
// A desynced worker rank on the peer Spark cannot be reconciled from here
// with a per-container stop: the peer runs its own manager and its own
// preflight, and reaching into its daemon outside a planned worker placement
// would act on a machine this job never resolved. That case is deliberately
// left to the peer's own admission checks rather than handled halfway.
func (e *Engine) reconcileRunningContainers(ctx context.Context, target recipe.Recipe, current jobPlan, completed int) (jobPlan, bool, error) {
	lister, ok := e.executor.(operations.ManagedContainerLister)
	if !ok {
		return current, false, nil
	}
	containers, err := lister.ManagedContainers(ctx)
	if err != nil {
		return jobPlan{}, false, fmt.Errorf("list this machine's running containers before activating %s: %w; fix the reported Docker error, then run this job again", target.DisplayName, err)
	}
	// A stop step anywhere in the plan covers its whole recipe ID: the host
	// executor's stop_container stops every running container carrying that
	// recipe's label, not only the named version, and a completed stop that
	// somehow left the container running again is re-executed on resume
	// (run() re-verifies completed runtime steps through Completed).
	stopsPlanned := map[string]bool{}
	for _, plan := range current.plans {
		if plan.Operation.Type == "stop_container" {
			stopsPlanned[plan.Recipe.ID] = true
		}
	}
	var extras []plannedOperation
	for _, container := range containers {
		if !container.Running || stopsPlanned[container.RecipeID] {
			continue
		}
		version, _ := strconv.Atoi(container.Version)
		if container.RecipeID == target.ID && version == target.Version {
			continue // the target's own container; reused, never stopped here
		}
		orphan, ok := e.pinnedOrEffective(container.RecipeID, version)
		if !ok {
			// Starting the target anyway could put two models in memory, and
			// killing the container outside the step machinery would leave no
			// receipt of what was stopped or why. Refuse with instructions.
			return jobPlan{}, false, fmt.Errorf("container %s is running a model (%s) whose recipe this manager cannot resolve, and %s cannot start safely beside it; stop that container, then run this job again", container.Name, container.RecipeID, target.DisplayName)
		}
		stopsPlanned[container.RecipeID] = true
		extras = append(extras, plannedOperation{Operation: recipe.Operation{Type: "stop_container"}, Recipe: orphan})
	}
	if len(extras) == 0 {
		return current, false, nil
	}
	// Insert where previousStopPlans sits: before the first step that needs
	// the memory (verify_memory) or takes over serving (start_container).
	// Everything at or after the insertion point has not run yet — completed
	// is the index of the step about to run — so completed steps keep their
	// indices and their receipts.
	insert := -1
	for index := completed; index < len(current.plans); index++ {
		if kind := current.plans[index].Operation.Type; kind == "verify_memory" || kind == "start_container" {
			insert = index
			break
		}
	}
	if insert < 0 {
		// Nothing in the remaining plan starts or measures the runtime, so
		// nothing new can collide with the running container.
		return current, false, nil
	}
	plans := make([]plannedOperation, 0, len(current.plans)+len(extras))
	plans = append(plans, current.plans[:insert]...)
	plans = append(plans, extras...)
	plans = append(plans, current.plans[insert:]...)
	adapted := current
	adapted.plans = plans
	return adapted, true, nil
}

// refreshCompletedPortReceipt keeps preflight history honest when the model
// to stop changed after verify_port completed. A refreshed synthetic receipt
// names the managed model that now occupies the target port. When only the
// old predecessor shared that port, its successful switch to the newly active
// model already stopped it under the same runtime lock, so the target port is
// available and the obsolete occupant claim must be removed.
func (e *Engine) refreshCompletedPortReceipt(ctx context.Context, jobID string, target recipe.Recipe, oldPlans, newPlans []plannedOperation, completed int) error {
	for index := 0; index < completed; index++ {
		if newPlans[index].Operation.Type != "verify_port" {
			continue
		}
		receipt := newPlans[index].Receipt
		if receipt == nil && oldPlans[index].Receipt != nil {
			receipt = map[string]any{"host_port": target.Service.DefaultHostPort, "available": true}
		}
		if receipt != nil {
			if err := e.store.UpdateStepReceipt(ctx, jobID, index, redact.JSON(receipt)); err != nil {
				return err
			}
		}
	}
	return nil
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
	r, ok := e.recipeForJob(ctx, job)
	if !ok {
		var payload reservationPayload
		_ = json.Unmarshal(job.Payload, &payload)
		reservationID := payload.ReservationID
		if reservationID == "" && (job.Kind == "install" || job.Kind == "start") {
			reservationID = fleet.ReservationID(fleet.ClaimKindLocalJob, job.ID)
		}
		if reservationID != "" {
			_ = e.reservations.Release(context.Background(), reservationID)
		}
		_ = e.store.UpdateJobState(ctx, jobID, "failed", "recipe is no longer available")
		return
	}
	reservationID, hasReservation, err := e.prepareJobReservation(ctx, job, r)
	if err != nil {
		_ = e.store.UpdateJobState(ctx, jobID, "failed", redact.String(err.Error()))
		return
	}
	keepReservation := false
	defer func() {
		if !hasReservation || keepReservation {
			return
		}
		_ = e.reservations.Release(context.Background(), reservationID)
		// A failed switch may have transferred the allocation before its
		// durable model transaction completed. Re-reading installed_models
		// restores the predecessor's claim and releases the failed target.
		_ = e.reservations.Reconcile(context.Background(), e.allRecipes())
	}()
	lock := e.recipeLock(r.ID)
	lock.Lock()
	defer lock.Unlock()
	runtimeHeld := false
	runtimeClaimed := false
	defer func() {
		if runtimeHeld {
			e.releaseRuntime()
		}
	}()
	switchHeld := func() {}
	defer func() { switchHeld() }()
	execution := operations.Execution{JobID: job.ID, ReservationID: reservationID, Kind: job.Kind}
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
	for index := 0; index < len(plans); index++ {
		plan := plans[index]
		if !runtimeHeld && (plan.BeginSwitch || runtimeOperations[plan.Operation.Type]) {
			if err := e.acquireRuntime(ctx); err != nil {
				abort(index, err)
				return
			}
			runtimeHeld = true
			// The mutating phase starts here, and this is where a job that
			// changes what this machine serves tells the rest of the manager
			// so (see SetSwitchGuard). Preflight, downloads and a two-Spark
			// cable check all happen before this point and hold nothing, so a
			// long install or an unreachable worker never blocks an unrelated
			// stop while the current model is still serving perfectly well.
			if switchesServing[job.Kind] {
				switchHeld = e.holdSwitch(ctx, r.ID)
			}
			if job.Kind == "install" || job.Kind == "start" {
				refreshed, changed, err := e.adaptActivationPlan(ctx, job, r, deployment, planned, index)
				if err != nil {
					abort(index, err)
					return
				}
				if changed {
					planned = refreshed
					plans, previous = planned.plans, planned.previous
					plan = plans[index]
				}
				// The store's answer is settled; now Docker's. A container the
				// store no longer points at can still be serving (task #48),
				// and the plan must stop it before anything of the target's
				// claims memory or the port.
				reconciled, stopsAdded, err := e.reconcileRunningContainers(ctx, r, planned, index)
				if err != nil {
					abort(index, err)
					return
				}
				if stopsAdded {
					planned = reconciled
					plans, previous = planned.plans, planned.previous
					plan = plans[index]
				}
				if hasReservation && !runtimeClaimed {
					replaceRecipeID, err := e.reservationPredecessor(ctx, r, previous)
					if err != nil {
						abort(index, fmt.Errorf("resolve this node's runtime reservation predecessor: %w", err))
						return
					}
					if err := e.reservations.Activate(ctx, reservationID, replaceRecipeID); err != nil {
						abort(index, fmt.Errorf("claim this node's runtime slot: %w", err))
						return
					}
					runtimeClaimed = true
				}
			}
		}
		if plan.BeginSwitch {
			if previous == nil {
				abort(index, errors.New("switch plan lost the previous model"))
				return
			}
			if err := e.store.BeginSwitch(ctx, previous.ID, r.ID); err != nil {
				if errors.Is(err, store.ErrSwitchTargetChanged) {
					abort(index, fmt.Errorf("another model took over serving while this job was running, so %s was not started; start it again from the console", r.DisplayName))
					return
				}
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
		if concurrentDownloadPair(plans, index) {
			failedIndex, err := e.executeConcurrentDownloads(ctx, execution, index, plans[index:index+maxConcurrentNodeDownloads], deployment, previous, planned.previousDeployment)
			if err != nil {
				abort(failedIndex, err)
				return
			}
			index++
			continue
		}
		op, target := plan.Operation, plan.Recipe
		execution.Placement = plan.Placement
		execution.Peer = peerFor(plan, deployment, previous, planned.previousDeployment)
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
		execution.ReservedBytes = e.reservedByOthers(reservationID)
		// The token counters live inside the container this is about to
		// stop, so the last reading has to be taken now. A worker rank runs
		// on the other Spark, which counts its own.
		if op.Type == "stop_container" && plan.Placement.Role != operations.RoleWorker {
			e.sampleTokens(ctx, target)
		}
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
	if hasReservation && jobActivates(job) {
		keepReservation = true
	}
	if job.Kind == "stop" || job.Kind == "remove" {
		_ = e.reservations.ReleaseRecipe(context.Background(), r.ID, "")
	}
}

func jobActivates(job store.Job) bool {
	if job.Kind == "start" {
		return true
	}
	if job.Kind != "install" {
		return false
	}
	var payload InstallPayload
	_ = json.Unmarshal(job.Payload, &payload)
	return payload.activate()
}

// reservationPredecessor names only a model that the local engine is allowed
// to replace under its runtime lock. activeRecipe omits the exact target
// version because no container switch is needed, but a fresh local job still
// has to transfer that model's persistent reservation. A worker rank is not
// an installed local model, so it never gains replacement authority here.
func (e *Engine) reservationPredecessor(ctx context.Context, target recipe.Recipe, previous *recipe.Recipe) (string, error) {
	if previous != nil {
		return previous.ID, nil
	}
	model, err := e.store.Model(ctx, target.ID)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if model.Active && model.RecipeVersion == target.Version {
		return target.ID, nil
	}
	return "", nil
}

// A distributed plan has exactly one head and one worker today. Keeping this
// limit beside the scheduling exception makes a future wider topology stay
// bounded until it has an explicit concurrency policy of its own.
const maxConcurrentNodeDownloads = 2

func concurrentDownloadPair(plans []plannedOperation, index int) bool {
	if index+maxConcurrentNodeDownloads > len(plans) {
		return false
	}
	head, worker := plans[index], plans[index+1]
	return head.Operation.Type == "download_artifact" && worker.Operation.Type == "download_artifact" &&
		head.Placement.Role == operations.RoleHead && worker.Placement.Role == operations.RoleWorker &&
		head.Recipe.ID == worker.Recipe.ID && head.Recipe.Version == worker.Recipe.Version
}

type concurrentDownload struct {
	index     int
	plan      plannedOperation
	execution operations.Execution
}

type concurrentDownloadResult struct {
	position int
	receipt  map[string]any
	err      error
}

// executeConcurrentDownloads is the only parallel step scheduler in the
// engine. Both step rows are prepared before either transfer starts, then the
// head and worker fetch the same pinned artifacts from upstream at the same
// time. A failure cancels the shared context, waits for the sibling to stop,
// and records both outcomes before the ordinary job failure path takes over.
func (e *Engine) executeConcurrentDownloads(ctx context.Context, base operations.Execution, start int, plans []plannedOperation, deployment operations.Deployment, previous *recipe.Recipe, previousDeployment operations.Deployment) (int, error) {
	downloads := make([]concurrentDownload, 0, maxConcurrentNodeDownloads)
	for offset, plan := range plans {
		index := start + offset
		execution := base
		execution.Placement = plan.Placement
		execution.Peer = peerFor(plan, deployment, previous, previousDeployment)
		execution.ReservedBytes = e.reservedByOthers(base.ReservationID)
		previousStep, exists, err := e.store.Step(ctx, base.JobID, index)
		if err != nil {
			return index, err
		}
		if exists && previousStep.State == "completed" && e.executor.Completed(ctx, execution, plan.Operation, plan.Recipe, previousStep.Receipt) {
			continue
		}
		downloads = append(downloads, concurrentDownload{index: index, plan: plan, execution: execution})
	}
	if len(downloads) == 0 {
		return -1, nil
	}
	if err := e.store.UpdateJobState(ctx, base.JobID, stateFor(base.Kind, "download_artifact"), ""); err != nil {
		return downloads[0].index, err
	}
	for position, download := range downloads {
		if err := e.store.BeginStep(ctx, base.JobID, download.index, stepName(download.plan.Operation, download.plan.Placement)); err != nil {
			for _, begun := range downloads[:position] {
				_ = e.store.FailStep(context.Background(), base.JobID, begun.index, "download did not start because both node steps could not be recorded")
			}
			return download.index, err
		}
	}

	pairCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan concurrentDownloadResult, len(downloads))
	var progressWrites sync.Mutex
	for position, download := range downloads {
		go func(position int, download concurrentDownload) {
			progress := func(value any) error {
				// SQLite already has one connection, and this mutex makes the
				// narrower promise explicit: receipts serialize, transfers do not.
				progressWrites.Lock()
				defer progressWrites.Unlock()
				return e.store.UpdateStepReceipt(pairCtx, base.JobID, download.index, redact.JSON(value))
			}
			receipt, err := e.executor.Execute(pairCtx, download.execution, download.plan.Operation, download.plan.Recipe, progress)
			if err != nil {
				cancel()
			}
			results <- concurrentDownloadResult{position: position, receipt: receipt, err: err}
		}(position, download)
	}
	outcomes := make([]concurrentDownloadResult, len(downloads))
	for range downloads {
		result := <-results
		outcomes[result.position] = result
	}

	if ctx.Err() != nil {
		for position := range outcomes {
			if outcomes[position].err == nil {
				outcomes[position].err = ctx.Err()
			}
		}
	} else {
		// A sibling that finished before cancellation keeps its verified
		// receipt. That makes a later retry skip work already completed.
		for position, outcome := range outcomes {
			if outcome.err != nil {
				continue
			}
			if err := e.store.CompleteStep(ctx, base.JobID, downloads[position].index, redact.JSON(outcome.receipt)); err != nil {
				outcomes[position].err = err
			}
		}
	}

	primary := -1
	if ctx.Err() == nil {
		for position, outcome := range outcomes {
			if outcome.err != nil && !errors.Is(outcome.err, context.Canceled) {
				primary = position
				break
			}
		}
	}
	if primary < 0 {
		for position, outcome := range outcomes {
			if outcome.err != nil {
				primary = position
				break
			}
		}
	}
	if primary < 0 {
		return -1, nil
	}
	for position, outcome := range outcomes {
		if position == primary || outcome.err == nil {
			continue
		}
		message := redact.String(outcome.err.Error())
		if errors.Is(outcome.err, context.Canceled) {
			message = "cancelled after the other Spark's download failed"
			if ctx.Err() != nil {
				message = "cancelled while this step was running"
			}
		}
		_ = e.store.FailStep(context.Background(), base.JobID, downloads[position].index, message)
	}
	return downloads[primary].index, outcomes[primary].err
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
		verify := recipe.Operation{Type: recipe.InferenceVerification(target.Runtime.Kind)}
		ops = []recipe.Operation{{Type: "verify_memory"}, {Type: "start_container"}, {Type: "wait_http"}, verify}
		// A download-only install never wrote the runtime config or created
		// the container (see downloadOnlyOperations); the first start after
		// one has to do both before the usual start sequence, and only then
		// does the host port actually get bound.
		if model, modelErr := e.store.Model(ctx, target.ID); modelErr == nil && model.ContainerID == "" {
			ops = []recipe.Operation{{Type: "verify_port"}, {Type: "write_generated_config"}, {Type: "create_container"}, {Type: "verify_memory"}, {Type: "start_container"}, {Type: "wait_http"}, verify}
		}
	case "stop":
		ops = []recipe.Operation{{Type: "stop_container"}}
	case "smoke-test":
		ops = []recipe.Operation{{Type: "wait_http"}, {Type: recipe.InferenceVerification(target.Runtime.Kind)}}
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

// peerFor resolves the peer a single step is pinned to. A switch plan can
// carry steps from two different deployments in the same job: stopping the
// model being switched away from (previousStopPlans, built from
// previousDeployment) and, when the target itself is distributed, bringing
// the target's own worker up (built from deployment). Both peers were
// resolved once, when the job was planned; this only ever picks between
// those two already-resolved values by which recipe the step belongs to; it
// never re-resolves a peer live. Matched on (ID, version) rather than ID
// alone: an in-place update of a distributed recipe switches away from an
// older version of the SAME ID, and ID alone would misclassify the target's
// own steps as the predecessor's. A plan step naming neither model's
// deployment, or one that is not part of a distributed placement at all,
// needs no peer.
func peerFor(plan plannedOperation, deployment operations.Deployment, previous *recipe.Recipe, previousDeployment operations.Deployment) *operations.PeerTarget {
	if !plan.Placement.Distributed() {
		return nil
	}
	if previous != nil && plan.Recipe.ID == previous.ID && plan.Recipe.Version == previous.Version {
		peer := previousDeployment.Peer
		return &peer
	}
	peer := deployment.Peer
	return &peer
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
	// The cable comes first, always. Every other step of a two-Spark job
	// depends on the two machines meeting over it, so an hour of downloading
	// must never happen before the link that would carry the model is known
	// to work. It is placed on the head because the head is what dials.
	plans := []plannedOperation{{Operation: recipe.Operation{Type: operations.VerifyFabric}, Recipe: target, Placement: head}}
	var workerBringUp, headBringUp, tail []plannedOperation
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
		case "wait_http", "verify_openai_inference", "verify_media_generation", "measure_throughput":
			tail = append(tail, plannedOperation{Operation: op, Recipe: target, Placement: head})
		case "pull_image", "download_artifact", "write_generated_config":
			checkPeer()
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Placement: head})
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Placement: worker})
		case "stop_container", "remove_container", "remove_artifact_if_unshared":
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Placement: head})
			plans = append(plans, plannedOperation{Operation: op, Recipe: target, Placement: worker})
		case "verify_port":
			// Only the head's port is checked from here. A worker rank that
			// binds a port of its own (every SGLang rank; a vLLM worker is
			// headless and binds none) is checked by the worker's own
			// preflight, on the machine that holds the port and against that
			// machine's containers, which is ADR 0004's per-node evaluation
			// and not something a head can answer for it.
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
		case "write_generated_config", "create_container", "verify_memory", "start_container", "wait_http", "verify_openai_inference", "verify_media_generation":
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
		// step is "rollback_verify_openai_inference:head". A media model is
		// proved by generating rather than by answering, so either
		// verification counts as the rollback having been proved.
		if step.State != "completed" {
			continue
		}
		if strings.HasPrefix(step.Operation, "rollback_verify_openai_inference") || strings.HasPrefix(step.Operation, "rollback_verify_media_generation") {
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
	// The model coming back is the one whose kind decides how it is proved:
	// a rollback onto a media model has to generate, not ask for tokens.
	for _, op := range []string{"verify_memory", "start_container", "wait_http", recipe.InferenceVerification(previous.Runtime.Kind)} {
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
		// A distributed (multi-Spark) recipe records this step as
		// "measure_throughput:head" (see stepName): the role suffix keeps a
		// two-node timeline honest about which machine ran it, but it must
		// not stop the result from ever reaching the model's stored metrics.
		if step.Operation != "measure_throughput" && !strings.HasPrefix(step.Operation, "measure_throughput:") {
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
		operations.VerifyFabric:   "preflighting",
		operations.VerifyPeerNode: "preflighting",
		"start_container":         "starting", "wait_http": "verifying_health", "verify_openai_inference": "verifying_inference", "verify_media_generation": "verifying_generation", "stop_container": "stopping", "remove_container": "removing", "remove_artifact_if_unshared": "removing", "measure_throughput": "benchmarking",
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
