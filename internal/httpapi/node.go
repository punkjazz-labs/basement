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

// releasingOperations end this node's part in a delegated deployment, so the
// single-flight lease is handed back once one of them runs.
var releasingOperations = map[string]bool{"stop_container": true, "remove_container": true, "remove_artifact_if_unshared": true}

// workerLeaseTTL bounds how long a head Spark may hold this node without
// saying anything. A head that dies mid-job must not wedge this Spark
// forever, and a real two-node step (a weight download) can be long.
const workerLeaseTTL = 45 * time.Minute

// workerLease is a coarse single-flight guard: one head Spark drives this
// node at a time. Delegated steps run outside this manager's own engine, so
// they take neither its runtime lock nor its disk reservation; without this,
// two heads (or a head and a local install) could both pass preflight and
// then fight over the same GPU. Proper engine integration is deferred.
type workerLease struct {
	mu       sync.Mutex
	jobID    string
	recipeID string
	expires  time.Time
}

func (l *workerLease) acquire(jobID, recipeID string, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.jobID != "" && l.jobID != jobID && now.Before(l.expires) {
		return fmt.Errorf("this Spark is already working as the second node for %s, so wait for that to finish", l.recipeID)
	}
	l.jobID, l.recipeID, l.expires = jobID, recipeID, now.Add(workerLeaseTTL)
	return nil
}

func (l *workerLease) release(jobID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.jobID == jobID {
		l.jobID, l.recipeID, l.expires = "", "", time.Time{}
	}
}

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
	if err := s.nodeLease.acquire(request.JobID, trusted.ID, time.Now()); err != nil {
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
	writeJSON(w, http.StatusOK, s.runPreflightSkipping(r.Context(), trusted, skip))
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
	if err := s.nodeLease.acquire(request.JobID, trusted.ID, time.Now()); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if releasingOperations[request.Operation] {
		defer s.nodeLease.release(request.JobID)
	}
	execution := operations.Execution{Kind: "worker", RemoveArtifacts: request.RemoveArtifacts, Placement: request.Placement}
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
	writeJSON(w, http.StatusOK, map[string]any{"operation": operation, "running": running, "receipt": receipt})
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
