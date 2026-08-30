package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/redact"
)

// workerOperations is the entire surface a head Spark may drive on this one.
// Anything that decides policy (which model is active, which job runs, what
// the console shows) stays local to each manager; only the mechanical steps
// of putting this node's own rank in place are remotely callable.
var workerOperations = map[string]bool{
	"verify_memory": true, "pull_image": true, "download_artifact": true,
	"write_generated_config": true, "create_container": true, "start_container": true,
	"stop_container": true, "remove_container": true, "remove_artifact_if_unshared": true,
}

// releasingOperations end this node's part in a delegated deployment, so its
// persistent runtime reservation is handed back once one of them runs.
var releasingOperations = map[string]bool{"stop_container": true, "remove_container": true, "remove_artifact_if_unshared": true}

// The preparation window covers long preflight and download staging before a
// rank is active. Once active, the head renews every fleet heartbeat interval,
// from the moment its own deployment reservation is committed and through
// staging and serving alike, and the worker allows nine missed renewals before
// reclaiming the rank. That is three times HeartbeatFreshness, so one stale
// heartbeat or a brief network interruption cannot tear down a model that is
// still serving.
const (
	legacyRankPrepareTTL       = 45 * time.Minute
	legacyDriverLeaseTTL       = 9 * fleet.HeartbeatInterval
	legacyReservationSweepRate = fleet.HeartbeatInterval
	// Three consecutive misses, sustained for the normal heartbeat freshness
	// window, are enough evidence that this head can no longer prove its worker
	// is alive. That is well before the worker's nine-heartbeat reclaim deadline
	// while still tolerating a short management-path interruption.
	distributedRenewalFailureLimit = 3
	// Keep one health proof below the heartbeat cadence. A slow scheduler must
	// not make maintenance pile up behind a long unbounded HTTP request.
	distributedHeadHealthTimeout = 5 * time.Second
	// A stopped group is safe to retry only when its start job could not be
	// recorded. Bound those database-path retries; a failed recovery start is
	// already a durable job and is never spun again automatically.
	distributedRecoveryJobAttemptLimit = 3
)

// delegatedProgress holds the latest receipt of the step this node is
// currently running for a head Spark. A delegated step runs outside this
// manager's engine, so no job row of ours records it, and the head's own
// call does not return until the step is over; polling this is the only way
// the console driving the job can watch a worker download move. It is in
// memory by design: it describes work in flight, never history.
type delegatedProgress struct {
	mu        sync.Mutex
	jobID     string
	operation string
	receipt   json.RawMessage
}

func (d *delegatedProgress) begin(jobID, operation string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.jobID, d.operation, d.receipt = jobID, operation, nil
}

func (d *delegatedProgress) update(jobID string, receipt json.RawMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.jobID != jobID {
		return
	}
	d.receipt = receipt
}

// finish clears the step only when it is still the one that ran, so a head
// admitted after this job handed the node back never has its own progress
// erased by the previous job's completion.
func (d *delegatedProgress) finish(jobID, operation string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.jobID == jobID && d.operation == operation {
		d.jobID, d.operation, d.receipt = "", "", nil
	}
}

// snapshot answers only the job that owns the running step. Another head's
// job id is told nothing, which is also what a job with no step in flight
// gets.
func (d *delegatedProgress) snapshot(jobID string) (string, json.RawMessage, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if jobID == "" || d.jobID != jobID {
		return "", nil, false
	}
	return d.operation, d.receipt, true
}

// withNodeAuth accepts an API-key bearer token and nothing else. A console
// session is deliberately not accepted: these endpoints mutate this node's
// containers, and refusing cookies means a browser can never be walked into
// calling them, so no CSRF token is involved.
func (s *Server) withNodeAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") || !s.store.VerifyAPIKey(r.Context(), strings.TrimPrefix(header, "Bearer ")) {
			writeError(w, http.StatusUnauthorized, errors.New("a fleet API key is required"))
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		next(w, r)
	}
}

// localExecutor is what a delegated step must run through. This server is
// wired with the fleet executor, which forwards a worker-placed step to a
// peer; running a delegated step through it would send the work straight
// back out instead of putting a rank on this machine.
func (s *Server) localExecutor() operations.Executor {
	if fleet, ok := s.executor.(interface {
		Local() operations.Executor
	}); ok {
		return fleet.Local()
	}
	return s.executor
}

func (s *Server) prepareLegacyRankReservation(ctx context.Context, jobID string, selected recipe.Recipe) (string, error) {
	if strings.TrimSpace(jobID) == "" {
		return "", errors.New("the worker job id is required")
	}
	placement := operations.Placement{Role: operations.RoleWorker, NodeCount: selected.Topology.SparkCount}
	fingerprint, err := fleet.RecipeFingerprint(selected)
	if err != nil {
		return "", err
	}
	// A reservation ID is an idempotency key for one exact claim. The legacy
	// job ID alone is insufficient because a retried preflight can present a
	// different locally trusted recipe. The pinned identity is stable across a
	// manager restart, while the stored fingerprint still verifies its body.
	reservationID := fleet.ExactRecipeReservationID(
		fleet.ClaimKindLegacyRank, s.engine.Reservations().NodeID(), jobID, selected.ID, selected.Version,
	)
	// This identity is deterministic, so one job's own reclaimed rank is left
	// here as a settled row: Prepare would return it unchanged and Activate
	// would refuse it, and every later step of that same deployment would be
	// told this Spark belongs to another one. A settled row holds no claim, so
	// the identity is cleared for reuse before it is prepared again.
	if err := s.engine.Reservations().ClearSettled(ctx, reservationID); err != nil {
		return "", err
	}
	prepared, _, err := s.engine.Reservations().Prepare(ctx, fleet.ReservationRequest{
		ReservationID: reservationID, DeploymentID: "legacy-rank:" + jobID,
		DriverNodeID: "legacy-head", RecipeID: selected.ID, RecipeVersion: selected.Version,
		RecipeFingerprint: fingerprint,
		Claims: fleet.ClaimsForRecipe(selected, fleet.RecipeClaimOptions{
			Kind: fleet.ClaimKindLegacyRank, JobID: jobID, ReserveDisk: true, Runtime: true, Placement: placement,
		}),
		PrepareToken: fleet.LocalPrepareToken(reservationID), ExpiresAt: time.Now().Add(legacyRankPrepareTTL),
	})
	if err != nil {
		return "", err
	}
	if prepared.State == "prepared" {
		if _, err := s.engine.Reservations().Commit(ctx, reservationID, fleet.LocalPrepareToken(reservationID), []byte(`{"kind":"legacy-rank-compatibility"}`)); err != nil {
			return "", err
		}
	}
	return reservationID, nil
}

func (s *Server) renewLegacyRankReservation(ctx context.Context, reservationID string) (time.Time, error) {
	expires := time.Now().Add(legacyDriverLeaseTTL)
	if err := s.engine.Reservations().Renew(ctx, reservationID, expires); err != nil {
		return time.Time{}, err
	}
	return expires, nil
}

// adoptableRankReservation names the rank claim this node already holds for
// one recipe, whatever deployment made it, so a step that ends this node's
// part in that deployment can hand back the very claim the model serves under
// instead of raising a second one against it. Only a live claim can be adopted:
// a settled or reclaiming row holds nothing to hand back, and a reclaiming rank
// is being stopped by this node's own sweep already.
//
// The recipe is the boundary and it is never crossed. A stop of one model must
// not free the rank another model is serving on, so a row for another recipe is
// left exactly as it is, whether or not it blocks anything. An active row wins
// over a committed one because it is the claim that owns the runtime slot.
func (s *Server) adoptableRankReservation(ctx context.Context, recipeID string) (string, error) {
	reservations, err := s.engine.Reservations().AllReservations(ctx)
	if err != nil {
		return "", err
	}
	committed := ""
	for _, reservation := range reservations {
		if reservation.Claims.Kind != fleet.ClaimKindLegacyRank || reservation.RecipeID != recipeID {
			continue
		}
		if reservation.State == "active" {
			return reservation.ReservationID, nil
		}
		if reservation.State == "committed" && committed == "" {
			committed = reservation.ReservationID
		}
	}
	return committed, nil
}

func (s *Server) renewActiveLegacyJob(ctx context.Context, jobID string) (time.Time, error) {
	reservations, err := s.engine.Reservations().AllReservations(ctx)
	if err != nil {
		return time.Time{}, err
	}
	for _, reservation := range reservations {
		if reservation.State == "active" && reservation.Claims.Kind == fleet.ClaimKindLegacyRank && reservation.Claims.JobID == jobID {
			return s.renewLegacyRankReservation(ctx, reservation.ReservationID)
		}
	}
	return time.Time{}, errors.New("the delegated job does not own an active worker reservation")
}

// nodeFabric reports where on the cable this node can be met, and starts
// listening there for exactly one connection from the head. It takes no lease
// and runs no operation: it detects a port, opens an ephemeral socket bound
// to that port's own address, and lets the socket close itself. A detection
// that fails is a 200 carrying the reason, so the head records a real check
// receipt saying what this Spark reported rather than a transport error.
func (s *Server) nodeFabric(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Recipe recipe.Recipe `json:"recipe"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	trusted, err := s.trustedWorkerRecipe(request.Recipe)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	probe, err := operations.ServeFabricProbe(trusted)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": redact.String(err.Error())})
		return
	}
	writeJSON(w, http.StatusOK, probe)
}

// nodePreflight runs this node's own guardrails for a recipe another Spark
// proposes to run here. Each node evaluates itself (ADR 0004); this never
// aggregates capacity with the caller's.
func (s *Server) nodePreflight(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Recipe recipe.Recipe `json:"recipe"`
		JobID  string        `json:"job_id"`
		// ReplacesRecipeID is the model the calling head is taking this node's
		// runtime slot from. It is an id and nothing more: an id this node
		// holds no active claim for matches nothing, and the claim is then
		// refused exactly as it is when no model is named at all.
		ReplacesRecipeID string `json:"replaces_recipe_id"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	trusted, err := s.trustedWorkerRecipe(request.Recipe)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	reservationID, err := s.prepareLegacyRankReservation(r.Context(), request.JobID, trusted)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	// Whether the host port matters here is the runtime's answer, not a rule
	// written down twice: a vLLM worker is launched --headless and binds
	// nothing, so failing it on the head's port would be inventing a problem,
	// while an SGLang worker binds that same port on THIS machine and a port
	// already taken here has to be said now. Left unchecked it surfaces an hour
	// later as a head that will not start, on the machine that is not the one
	// holding the port.
	skip := map[string]bool{}
	if _, binds := operations.RankBindsHostPort(trusted, operations.Placement{Role: operations.RoleWorker, NodeCount: trusted.Topology.SparkCount}); !binds {
		skip["verify_port"] = true
	}
	preflight := s.runPreflightSkippingReservation(r.Context(), trusted, skip, reservationID)
	if !preflight.Ready {
		_ = s.engine.Reservations().Abort(r.Context(), reservationID)
		writeJSON(w, http.StatusOK, preflight)
		return
	}
	// A node evaluates itself before the job that asks stages anything, and a
	// check that passes takes this node's runtime slot for that job. The slot
	// can belong to the model the same job is going to stop, and this node
	// cannot know that by itself: it keeps no installed-model rows, so the head
	// names the model it replaces. Without that name a switch was refused here,
	// one step before the stop that would have freed the rank, and no two-Spark
	// model could be replaced from the console.
	if err := s.engine.Reservations().Activate(r.Context(), reservationID, request.ReplacesRecipeID); err != nil {
		_ = s.engine.Reservations().Abort(r.Context(), reservationID)
		writeError(w, http.StatusConflict, fmt.Errorf("this node's runtime is already reserved by another deployment: %w", err))
		return
	}
	if _, err := s.renewLegacyRankReservation(r.Context(), reservationID); err != nil {
		_ = s.engine.Reservations().Release(r.Context(), reservationID)
		writeError(w, http.StatusConflict, fmt.Errorf("renew this node's delegated runtime reservation: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, preflight)
}

func (s *Server) nodeRenewReservation(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Recipe recipe.Recipe `json:"recipe"`
		JobID  string        `json:"job_id"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	trusted, err := s.trustedWorkerRecipe(request.Recipe)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	reservationID := fleet.ExactRecipeReservationID(
		fleet.ClaimKindLegacyRank, s.engine.Reservations().NodeID(), request.JobID, trusted.ID, trusted.Version,
	)
	reservation, err := s.engine.Reservations().Reservation(r.Context(), reservationID)
	if err != nil || reservation.State != "active" || reservation.Claims.Kind != fleet.ClaimKindLegacyRank || reservation.Claims.JobID != request.JobID {
		writeError(w, http.StatusConflict, errors.New("the delegated job does not own an active worker reservation"))
		return
	}
	// Renewals keep an active distributed worker from being reclaimed during a
	// long stage, but after this worker has successfully started its rank they
	// must also prove that the rank still exists. Without this check a dead rank
	// can keep a fresh lease while the head's HTTP process continues to answer.
	// A missing liveness row is deliberately tolerated while staging. When the
	// exact container is already running, adopt it into this table as part of
	// renewal: that is how a rank started by the previous manager version gains
	// the new liveness evidence without requiring an outage.
	running, known, err := s.store.DelegatedRankRunning(r.Context(), reservationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	execution := operations.Execution{
		ReservationID: reservationID, Kind: "worker",
		Placement: operations.Placement{Role: operations.RoleWorker, NodeCount: trusted.Topology.SparkCount},
	}
	containerRunning := s.localExecutor().Completed(r.Context(), execution, recipe.Operation{Type: "start_container"}, trusted, nil)
	if known && running && !containerRunning {
		writeError(w, http.StatusConflict, errors.New("the worker model container is no longer running"))
		return
	}
	if !known && containerRunning {
		if err := s.store.SetDelegatedRankRunning(r.Context(), reservationID, true); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("adopt this running worker rank: %w", err))
			return
		}
	}
	expires, err := s.renewLegacyRankReservation(r.Context(), reservationID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"expires_at": expires.UTC().Format(time.RFC3339Nano)})
}

// nodeStep runs exactly one typed operation on this node on behalf of the
// head Spark, and returns its receipt. A failed operation is still a 200
// carrying the reason, so the head records a real step receipt rather than a
// transport error.
func (s *Server) nodeStep(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Operation       string               `json:"operation"`
		Recipe          recipe.Recipe        `json:"recipe"`
		Placement       operations.Placement `json:"placement"`
		RemoveArtifacts bool                 `json:"remove_artifacts"`
		JobID           string               `json:"job_id"`
		// ReplacesRecipeID carries the same answer the check carried: the
		// staging steps of a switch also arrive before its stop does.
		ReplacesRecipeID string `json:"replaces_recipe_id"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	trusted, err := s.trustedWorkerRecipe(request.Recipe)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !workerOperations[request.Operation] {
		writeError(w, http.StatusBadRequest, errors.New("operation "+request.Operation+" cannot be run on behalf of another Spark"))
		return
	}
	if request.Placement.Role != operations.RoleWorker {
		writeError(w, http.StatusBadRequest, errors.New("this Spark can only be driven as a worker node"))
		return
	}
	reservationID := ""
	// handedBack is set once the operation has really ended this node's part in
	// the deployment. A rank is given back because it stopped, never because
	// something was attempted: an adopted claim released after a FAILED stop
	// would open this node to a second model while the first one still holds
	// the memory, and it would also take the failed rank out of reach of
	// ReclaimExpiredDriverReservations for good, because only an active or
	// reclaiming row is ever swept (LegacyRanksDueForReclaim). The head's
	// teardown retry adopts the very same row and hands it back when it
	// succeeds, so keeping it wedges nothing.
	handedBack := false
	if releasingOperations[request.Operation] {
		// A releasing step hands this node's rank back; it must never compete
		// for the slot it exists to free. A rank identity is per job, and the
		// rank that serves belongs to the job that STARTED the model, while a
		// stop always arrives under a job of its own. Preparing a fresh claim
		// here therefore made the stop of a serving two-Spark model race the
		// model's own serve reservation, and this Spark refused its own head:
		// the model could not be stopped from the console at all, and it was
		// left half dead, with the head rank exited and the worker rank still
		// holding the memory (hardware, 2026-08-29). A releasing step adopts
		// the rank this node already holds for that same recipe, whichever
		// deployment made it, and hands that one back. The switch flow rides
		// this same path: it stops the previous model under the new install
		// job's id.
		adopted, err := s.adoptableRankReservation(r.Context(), trusted.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if adopted != "" {
			reservationID = adopted
			defer func() {
				if handedBack {
					_ = s.engine.Reservations().Release(context.Background(), adopted)
				}
			}()
		}
		// An empty answer means this node holds no rank for that recipe, so
		// there is nothing to hand back and nothing to claim either. A
		// releasing step only frees this machine's own resources, and the
		// executor reads a reservation id for one purpose alone, discounting a
		// disk claim in verify_disk, which no releasing operation runs. So the
		// step runs unreserved instead of taking the slot in order to free it,
		// and an unrelated model serving here cannot block it.
	} else {
		prepared, err := s.prepareLegacyRankReservation(r.Context(), request.JobID, trusted)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		if err := s.engine.Reservations().Activate(r.Context(), prepared, request.ReplacesRecipeID); err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("this node's runtime is already reserved by another deployment: %w", err))
			return
		}
		if _, err := s.renewLegacyRankReservation(r.Context(), prepared); err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("renew this node's delegated runtime reservation: %w", err))
			return
		}
		reservationID = prepared
	}
	execution := operations.Execution{ReservationID: reservationID, Kind: "worker", RemoveArtifacts: request.RemoveArtifacts, Placement: request.Placement}
	if request.RemoveArtifacts {
		shared, err := s.sharedArtifacts(r.Context(), trusted.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		execution.SharedArtifacts = shared
	}
	s.nodeProgress.begin(request.JobID, request.Operation)
	defer s.nodeProgress.finish(request.JobID, request.Operation)
	// Redacted where it is held, not where it is read: the head folds this
	// receipt straight into its own job timeline.
	progress := func(value any) error {
		s.nodeProgress.update(request.JobID, redact.JSON(value))
		return nil
	}
	// The recipe executed is this Spark's own copy, never the bytes the
	// caller sent, and it runs locally rather than being forwarded onward.
	receipt, err := s.localExecutor().Execute(r.Context(), execution, recipe.Operation{Type: request.Operation}, trusted, progress)
	handedBack = err == nil
	if err == nil {
		switch request.Operation {
		case "start_container":
			if persistErr := s.store.SetDelegatedRankRunning(r.Context(), reservationID, true); persistErr != nil {
				err = fmt.Errorf("record that this worker rank started: %w", persistErr)
			}
		case "stop_container", "remove_container", "remove_artifact_if_unshared":
			if persistErr := s.store.ClearDelegatedRankLiveness(r.Context(), reservationID); persistErr != nil {
				err = fmt.Errorf("clear this worker rank's liveness: %w", persistErr)
			}
		}
	}
	response := map[string]any{"receipt": receipt}
	if err != nil {
		response["error"] = redact.String(err.Error())
	}
	writeJSON(w, http.StatusOK, response)
}

// nodeStepProgress reports how far the step this node runs for the calling
// job has got. It takes no lease, runs no operation and changes nothing: the
// head polls it while its own step call is still blocked, so it must never
// wait on anything that step holds.
func (s *Server) nodeStepProgress(w http.ResponseWriter, r *http.Request) {
	var request struct {
		JobID string `json:"job_id"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	operation, receipt, running := s.nodeProgress.snapshot(request.JobID)
	if running {
		// A step running under an ADOPTED rank is answered with a refusal here,
		// on purpose: the adopted row carries the job id of the deployment that
		// took the rank, not of the job that is handing it back, so this job
		// owns no active rank of its own to renew. That costs nothing. A failed
		// poll is skipped by the head (PeerClient.follow) and no releasing
		// operation reports progress. Matching the poller to the row instead
		// would widen who may renew a rank, which is the surface the reservation
		// incidents of 2026-08-29 were about.
		if _, err := s.renewActiveLegacyJob(r.Context(), request.JobID); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"operation": operation, "running": running, "receipt": receipt})
}

// ReclaimExpiredDriverReservations stops an orphan worker rank before freeing
// its reservation. The intermediate reclaiming state remains an admission
// conflict, so a replacement cannot overlap the old container even if stop
// needs several maintenance passes to succeed.
func (s *Server) ReclaimExpiredDriverReservations(ctx context.Context) error {
	if s.engine == nil {
		return nil
	}
	now := time.Now()
	due, err := s.engine.Reservations().LegacyRanksDueForReclaim(ctx, now)
	if err != nil {
		return err
	}
	var joined error
	for _, candidate := range due {
		reservation, err := s.engine.Reservations().BeginReclaim(ctx, candidate.ReservationID, now)
		if err != nil {
			joined = errors.Join(joined, err)
			continue
		}
		selected, ok := recipe.FindVersion(s.allRecipes(), reservation.RecipeID, reservation.RecipeVersion)
		if !ok {
			joined = errors.Join(joined, fmt.Errorf("the expired worker recipe %s version %d is unavailable", reservation.RecipeID, reservation.RecipeVersion))
			continue
		}
		fingerprint, err := fleet.RecipeFingerprint(selected)
		if err != nil || fingerprint != reservation.RecipeFingerprint {
			joined = errors.Join(joined, fmt.Errorf("the expired worker recipe %s version %d no longer matches its reservation", reservation.RecipeID, reservation.RecipeVersion))
			continue
		}
		placement := operations.Placement{Role: operations.RoleWorker, NodeCount: selected.Topology.SparkCount}
		execution := operations.Execution{JobID: reservation.Claims.JobID, ReservationID: reservation.ReservationID, Kind: "worker", Placement: placement}
		if _, err := s.localExecutor().Execute(ctx, execution, recipe.Operation{Type: "stop_container"}, selected, nil); err != nil {
			joined = errors.Join(joined, fmt.Errorf("stop expired worker rank: %w", err))
			continue
		}
		if err := s.store.ClearDelegatedRankLiveness(ctx, reservation.ReservationID); err != nil {
			joined = errors.Join(joined, fmt.Errorf("clear expired worker rank liveness: %w", err))
			continue
		}
		if err := s.engine.Reservations().FinishReclaim(ctx, reservation.ReservationID); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func (s *Server) RunReservationMaintenance(ctx context.Context) {
	ticker := time.NewTicker(legacyReservationSweepRate)
	defer ticker.Stop()
	for {
		s.maintainDistributedServing(ctx, time.Now())
		_ = s.ReclaimExpiredDriverReservations(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.closing:
			return
		case <-ticker.C:
		}
	}
}

// maintainDistributedServing turns renewal failure into a bounded group
// reaction. A worker lease is a liveness proof only while it is renewed; once
// that proof is stale the head must stop presenting its model as ready and
// must not wait for the worker's later reclaim to leave rank 0 serving alone.
func (s *Server) maintainDistributedServing(ctx context.Context, observed time.Time) {
	if s.renewDistributed == nil || s.recoverDistributed == nil {
		return
	}
	err := s.renewDistributed(ctx)
	active, activeDistributed := s.activeReadyRecipe(ctx)
	if err == nil && activeDistributed && active.Distributed() && s.headHealth != nil {
		healthCtx, cancel := context.WithTimeout(ctx, distributedHeadHealthTimeout)
		err = s.headHealth(healthCtx, active)
		cancel()
	}
	s.renewalMu.Lock()
	if err == nil {
		if activeDistributed && active.Distributed() {
			s.resetDistributedRecoveryLocked()
			s.renewalMu.Unlock()
			return
		}
		if s.recoveryJobPending {
			if s.recoveryJobAttempts >= distributedRecoveryJobAttemptLimit {
				s.renewalMu.Unlock()
				return
			}
			recipeID, reason := s.recoveryRecipeID, s.recoveryReason
			s.recoveryJobAttempts++
			s.renewalMu.Unlock()
			s.retryDistributedRecovery(ctx, recipeID, reason, true)
			return
		}
		s.resetDistributedRecoveryLocked()
		s.renewalMu.Unlock()
		return
	}
	if s.renewalFirstFailed.IsZero() {
		s.renewalFirstFailed = observed
	}
	s.renewalFailures++
	due := !s.renewalDegrading &&
		s.renewalFailures >= distributedRenewalFailureLimit &&
		observed.Sub(s.renewalFirstFailed) >= fleet.HeartbeatFreshness
	if due {
		s.renewalDegrading = true
		s.recoveryRecipeID = active.ID
		s.recoveryReason = redact.String(err.Error())
		s.recoveryJobAttempts = 1
	}
	s.renewalMu.Unlock()
	if !due {
		return
	}
	// Recover synchronously so this loop cannot continue renewing a worker
	// while the engine stops the same group and records its one durable recovery
	// job. The engine records failed before either stop, which closes inference
	// admission even when the worker is unreachable.
	s.retryDistributedRecovery(ctx, active.ID, redact.String(err.Error()), false)
}

// proveDistributedHeadHealth uses the same local executor capability as the
// readiness step. For text runtimes that is an exact loopback /health request;
// the deadline makes a wedged scheduler evidence rather than a wedged manager.
func (s *Server) proveDistributedHeadHealth(ctx context.Context, active recipe.Recipe) error {
	healthCtx, cancel := context.WithTimeout(ctx, distributedHeadHealthTimeout)
	defer cancel()
	execution := operations.Execution{Kind: "health", Placement: operations.Placement{
		Role: operations.RoleHead, NodeCount: active.Topology.SparkCount,
	}}
	if s.localExecutor().Completed(healthCtx, execution, recipe.Operation{Type: "wait_http"}, active, nil) {
		return nil
	}
	return errors.New("the active model did not answer its /health check")
}

func (s *Server) retryDistributedRecovery(ctx context.Context, recipeID, reason string, retryRecoveryJob bool) {
	err := s.recoverDistributed(ctx, recipeID, reason, retryRecoveryJob)
	s.renewalMu.Lock()
	defer s.renewalMu.Unlock()
	if err == nil {
		s.resetDistributedRecoveryLocked()
		return
	}
	if errors.Is(err, engine.ErrDistributedRecoveryJob) {
		s.recoveryJobPending = true
		return
	}
	// A failed stop has no safe automatic retry. The engine logs the error and
	// leaves the model failed, rather than risking a second group beside it.
	s.recoveryJobPending = false
}

func (s *Server) resetDistributedRecoveryLocked() {
	s.renewalFailures = 0
	s.renewalFirstFailed = time.Time{}
	s.renewalDegrading = false
	s.recoveryJobPending = false
	s.recoveryRecipeID = ""
	s.recoveryReason = ""
	s.recoveryJobAttempts = 0
}

// trustedWorkerRecipe resolves what this node will actually run. The caller
// holds an ordinary API key, which already grants it installs of THIS
// Spark's own catalogue and nothing more; a delegated step must not widen
// that into running an attacker-chosen image with host networking, RDMA
// devices and the GPU. So a proposal is accepted only when this Spark
// already holds that exact recipe id and version, byte for byte, and the
// local copy is what executes.
func (s *Server) trustedWorkerRecipe(proposed recipe.Recipe) (recipe.Recipe, error) {
	local, ok := recipe.FindVersion(s.allRecipes(), proposed.ID, proposed.Version)
	if !ok {
		return recipe.Recipe{}, fmt.Errorf("this Spark does not have recipe %s version %d in its own catalogue", proposed.ID, proposed.Version)
	}
	localSum, err := recipeFingerprint(local)
	if err != nil {
		return recipe.Recipe{}, err
	}
	proposedSum, err := recipeFingerprint(proposed)
	if err != nil {
		return recipe.Recipe{}, err
	}
	if subtle.ConstantTimeCompare([]byte(localSum), []byte(proposedSum)) != 1 {
		return recipe.Recipe{}, fmt.Errorf("the proposed recipe %s does not match this Spark's own copy", proposed.ID)
	}
	if !local.Distributed() {
		return recipe.Recipe{}, errors.New("a single-Spark recipe is never run on behalf of another Spark")
	}
	return local, nil
}

func recipeFingerprint(r recipe.Recipe) (string, error) {
	encoded, err := json.Marshal(r)
	if err != nil {
		return "", errors.New("a recipe could not be canonicalized for comparison")
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// sharedArtifacts reports artifact keys and paths this node's OTHER
// installed models still depend on, so a remote removal can never delete
// weights another local model is serving from.
func (s *Server) sharedArtifacts(ctx context.Context, excludeRecipeID string) (map[string]bool, error) {
	models, err := s.store.Models(ctx)
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
		other, ok := s.pinnedOrEffective(model.RecipeID, model.RecipeVersion)
		if !ok {
			continue
		}
		for _, artifact := range other.Artifacts {
			shared[operations.ArtifactKey(artifact)] = true
		}
	}
	return shared, nil
}
