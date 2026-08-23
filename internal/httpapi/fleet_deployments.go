package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

func (s *Server) PreflightIndependent(ctx context.Context, selected recipe.Recipe, reservationID string) (json.RawMessage, bool, error) {
	if selected.Topology.SparkCount != 1 {
		return nil, false, errors.New("only a single-node recipe can use independent placement")
	}
	response := s.runPreflightSkippingReservation(ctx, selected, nil, reservationID)
	payload, err := json.Marshal(response)
	return payload, response.Ready, err
}

func (s *Server) CreateIndependentJob(ctx context.Context, selected recipe.Recipe, intent fleet.IndependentIntent, reservationID, deploymentID, idempotencyKey string) (store.Job, bool, error) {
	if selected.Topology.SparkCount != 1 {
		return store.Job{}, false, errors.New("only a single-node recipe can use independent placement")
	}
	if !intent.Confirmed {
		return store.Job{}, false, errors.New("explicit installation confirmation is required")
	}
	preflight := s.runPreflightSkippingReservation(ctx, selected, nil, reservationID)
	if !preflight.Ready {
		return store.Job{}, false, errors.New("preflight failed without mutating runtime state")
	}
	if selected.Requirements.RequiredLicenceAccept && !intent.AcceptLicence {
		return store.Job{}, false, errors.New("model licence acceptance is required on the target node")
	}
	if selected.RequiresTerritoryConfirmation() && !intent.ConfirmTerritoryEligibility {
		return store.Job{}, false, errors.New("territory eligibility confirmation is required on the target node")
	}
	if intent.AcceptLicence {
		if err := s.store.AcceptLicence(ctx, selected.ID, selected.Version); err != nil {
			return store.Job{}, false, err
		}
	}
	if intent.ConfirmTerritoryEligibility {
		if err := s.store.ConfirmTerritoryEligibility(ctx, selected.ID, selected.Version); err != nil {
			return store.Job{}, false, err
		}
	}
	job, created, err := s.store.CreateJob(ctx, "install", selected.ID, idempotencyKey, engine.InstallPayload{
		Activate: boolPointer(intent.Activate), ReservationID: reservationID, DeploymentID: deploymentID,
	})
	if err != nil {
		return store.Job{}, false, err
	}
	if err := s.engine.Reservations().AttachJob(ctx, reservationID, job.ID); err != nil {
		if created {
			_ = s.store.UpdateJobState(ctx, job.ID, "failed", "the deployment reservation could not be attached to its target job")
		}
		return store.Job{}, false, err
	}
	if created || job.State == "failed" || job.State == "interrupted" {
		s.engine.Start(job.ID)
	}
	return job, created, nil
}

// AdoptIndependentJob gives an already-installed model the deployment record
// it never had. The job is only a carrier: its payload holds the
// deployment_id, which is the one place independentDeploymentID reads it
// from. The engine never sees this job. It is terminal from the moment it is
// created, so the serving container is not touched.
func (s *Server) AdoptIndependentJob(ctx context.Context, selected recipe.Recipe, deploymentID, idempotencyKey string) (store.Job, bool, error) {
	if selected.Topology.SparkCount != 1 {
		return store.Job{}, false, errors.New("only a single-node recipe can use independent placement")
	}
	if strings.TrimSpace(deploymentID) == "" {
		return store.Job{}, false, errors.New("the deployment id is required")
	}
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 128 {
		return store.Job{}, false, errors.New("a valid idempotency key is required")
	}
	model, err := s.store.Model(ctx, selected.ID)
	if err != nil {
		return store.Job{}, false, errors.New("the model is not installed on that node")
	}
	if model.RecipeVersion != selected.Version {
		return store.Job{}, false, errors.New("the installed model is a different version, so update it before you adopt it")
	}
	job, created, err := s.store.CreateJob(ctx, "adopt", selected.ID, idempotencyKey, map[string]any{"deployment_id": deploymentID})
	if err != nil {
		return store.Job{}, false, err
	}
	// A retry must repair a carrier job that an earlier attempt left short of
	// terminal, the way the sibling install path restarts a failed or
	// interrupted job. This job has no work to redo, so it only needs its
	// state put right.
	if created || job.State != "ready" {
		if err := s.store.UpdateJobState(ctx, job.ID, "ready", ""); err != nil {
			return store.Job{}, false, err
		}
	}
	job, err = s.store.GetJob(ctx, job.ID)
	return job, created, err
}

func boolPointer(value bool) *bool { return &value }

func (s *Server) IndependentJob(ctx context.Context, jobID string) (store.Job, error) {
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return store.Job{}, err
	}
	if independentDeploymentID(job) == "" {
		return store.Job{}, errors.New("the job is not owned by an independent fleet deployment")
	}
	return job, nil
}

func (s *Server) IndependentAction(ctx context.Context, owner store.Job, action, idempotencyKey string, intent fleet.IndependentIntent) (store.Job, error) {
	allowed := map[string]bool{"start": true, "stop": true, "remove": true, "cancel": true, "smoke-test": true, "benchmark": true}
	if !allowed[action] {
		return store.Job{}, errors.New("the deployment action is not supported")
	}
	if action == "cancel" {
		if err := s.engine.Cancel(ctx, owner.ID); err != nil {
			return store.Job{}, err
		}
		return s.store.GetJob(ctx, owner.ID)
	}
	selected, ok := s.pinnedOrEffective(owner.RecipeID, installedVersion(s.store, ctx, owner.RecipeID))
	if !ok {
		return store.Job{}, errors.New("the deployment recipe is no longer available on its owner node")
	}
	if _, err := s.store.Model(ctx, selected.ID); err != nil {
		return store.Job{}, errors.New("the model is not installed on its owner node")
	}
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 128 {
		return store.Job{}, errors.New("a valid idempotency key is required")
	}
	deploymentID := independentDeploymentID(owner)
	if deploymentID == "" {
		return store.Job{}, errors.New("the owner job is not tied to an independent fleet deployment")
	}
	payload := map[string]any{"deployment_id": deploymentID}
	if action == "remove" {
		payload["remove_artifacts"] = intent.RemoveArtifacts
	}
	job, created, err := s.store.CreateJob(ctx, action, selected.ID, idempotencyKey, payload)
	if err != nil {
		return store.Job{}, err
	}
	if created || job.State == "failed" || job.State == "interrupted" {
		s.engine.Start(job.ID)
	}
	return job, nil
}

func independentDeploymentID(job store.Job) string {
	var payload struct {
		DeploymentID string `json:"deployment_id"`
	}
	if json.Unmarshal(job.Payload, &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.DeploymentID)
}

func installedVersion(database *store.Store, ctx context.Context, recipeID string) int {
	model, err := database.Model(ctx, recipeID)
	if err != nil {
		return 0
	}
	return model.RecipeVersion
}

func (s *Server) fleetPlacementPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if _, ok := s.auth.Authenticate(r); !ok {
		writeError(w, http.StatusUnauthorized, errors.New("console authentication is required"))
		return
	}
	if s.fleetManager == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("fleet placement is unavailable"))
		return
	}
	var request struct {
		RecipeID string `json:"recipe_id"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	plan, err := s.fleetManager.PlanIndependent(r.Context(), request.RecipeID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) fleetDeployments(w http.ResponseWriter, r *http.Request) {
	if s.fleetManager == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("fleet placement is unavailable"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.auth.Authenticate(r); !ok {
			writeError(w, http.StatusUnauthorized, errors.New("console authentication is required"))
			return
		}
		deployments, err := s.fleetManager.Deployments(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, deployments)
	case http.MethodPost:
		if err := s.auth.AuthorizeMutation(r); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
		var request struct {
			RecipeID                    string `json:"recipe_id"`
			NodeID                      string `json:"node_id"`
			Confirmed                   bool   `json:"confirmed"`
			AcceptLicence               bool   `json:"accept_licence"`
			ConfirmTerritoryEligibility bool   `json:"confirm_territory_eligibility"`
			Activate                    *bool  `json:"activate"`
		}
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		activate := true
		if request.Activate != nil {
			activate = *request.Activate
		}
		deployment, created, err := s.fleetManager.CreateIndependentDeployment(r.Context(), fleet.CreateDeploymentRequest{
			RecipeID: request.RecipeID, NodeID: request.NodeID, IdempotencyKey: r.Header.Get("Idempotency-Key"),
			Intent: fleet.IndependentIntent{Confirmed: request.Confirmed, AcceptLicence: request.AcceptLicence,
				ConfirmTerritoryEligibility: request.ConfirmTerritoryEligibility, Activate: activate},
		})
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		status := http.StatusAccepted
		if !created {
			status = http.StatusOK
		}
		writeJSON(w, status, map[string]any{"deployment": deployment, "created": created})
	default:
		methodNotAllowed(w)
	}
}

// fleetDeploymentAdopt records a model that a node already runs as a fleet
// deployment, so this controller's console can act on it. The deployment id
// is deterministic from the fleet, the node, and the recipe. It does NOT come
// from the Idempotency-Key: that header only guards the carrier job row on
// the owner node. Adoption starts, stops, and restarts nothing.
func (s *Server) fleetDeploymentAdopt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.fleetManager == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("fleet placement is unavailable"))
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var request struct {
		NodeID   string `json:"node_id"`
		RecipeID string `json:"recipe_id"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	deployment, created, err := s.fleetManager.AdoptIndependentDeployment(r.Context(), request.NodeID, request.RecipeID, r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	// 201, not 202: the record exists in full by the time this replies, and
	// no work was handed to anything. A repeat adoption returns 200.
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"deployment": deployment, "created": created})
}

func (s *Server) fleetDeploymentAction(w http.ResponseWriter, r *http.Request) {
	if s.fleetManager == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("fleet placement is unavailable"))
		return
	}
	trimmed := strings.TrimPrefix(path.Clean(r.URL.Path), "/api/v1/fleet/deployments/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 || parts[0] == "" || parts[0] == "." {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		if _, ok := s.auth.Authenticate(r); !ok {
			writeError(w, http.StatusUnauthorized, errors.New("console authentication is required"))
			return
		}
		deployment, err := s.fleetManager.Deployment(r.Context(), parts[0])
		if err != nil {
			writeError(w, http.StatusNotFound, errors.New("fleet deployment not found"))
			return
		}
		writeJSON(w, http.StatusOK, deployment)
		return
	}
	if len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet {
		if _, ok := s.auth.Authenticate(r); !ok {
			writeError(w, http.StatusUnauthorized, errors.New("console authentication is required"))
			return
		}
		s.fleetDeploymentEvents(w, r, parts[0])
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var intent fleet.IndependentIntent
	if err := decodeBody(r, &intent); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	job, err := s.fleetManager.ActionDeployment(r.Context(), parts[0], parts[1], r.Header.Get("Idempotency-Key"), intent)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) fleetDeploymentEvents(w http.ResponseWriter, r *http.Request, deploymentID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := ""
	for {
		deployment, err := s.fleetManager.Deployment(r.Context(), deploymentID)
		if err != nil {
			return
		}
		payload, _ := json.Marshal(deployment)
		if current := string(payload); current != last {
			_, _ = fmt.Fprintf(w, "event: deployment\ndata: %s\n\n", payload)
			flusher.Flush()
			last = current
		}
		if deployment.Job != nil && terminal(deployment.Job.State) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case <-ticker.C:
		}
	}
}

var _ fleet.IndependentRuntime = (*Server)(nil)
