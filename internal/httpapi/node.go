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
	if err := s.engine.Reservations().Activate(r.Context(), reservationID, ""); err != nil {
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
	reservationID, err := s.prepareLegacyRankReservation(r.Context(), request.JobID, trusted)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.engine.Reservations().Activate(r.Context(), reservationID, ""); err != nil {
		writeError(w, http.StatusConflict, fmt.Errorf("this node's runtime is already reserved by another deployment: %w", err))
		return
	}
	if _, err := s.renewLegacyRankReservation(r.Context(), reservationID); err != nil {
		writeError(w, http.StatusConflict, fmt.Errorf("renew this node's delegated runtime reservation: %w", err))
		return
	}
	if releasingOperations[request.Operation] {
		defer s.engine.Reservations().Release(context.Background(), reservationID)
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
		if s.engine != nil {
			_ = s.engine.RenewDistributedReservations(ctx)
		}
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
