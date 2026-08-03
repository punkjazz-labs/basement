package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/punkjazz-labs/runonspark-manager/internal/operations"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
	"github.com/punkjazz-labs/runonspark-manager/internal/redact"
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

// nodePreflight runs this node's own guardrails for a recipe another Spark
// proposes to run here. Each node evaluates itself (ADR 0004); this never
// aggregates capacity with the caller's.
func (s *Server) nodePreflight(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Recipe recipe.Recipe `json:"recipe"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := acceptedWorkerRecipe(request.Recipe); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// A worker rank serves no HTTP, so the head's host port is not its concern.
	writeJSON(w, http.StatusOK, s.runPreflightSkipping(r.Context(), request.Recipe, map[string]bool{"verify_port": true}))
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
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := acceptedWorkerRecipe(request.Recipe); err != nil {
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
	execution := operations.Execution{Kind: "worker", RemoveArtifacts: request.RemoveArtifacts, Placement: request.Placement}
	if request.RemoveArtifacts {
		shared, err := s.sharedArtifacts(r.Context(), request.Recipe.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		execution.SharedArtifacts = shared
	}
	receipt, err := s.executor.Execute(r.Context(), execution, recipe.Operation{Type: request.Operation}, request.Recipe, nil)
	response := map[string]any{"receipt": receipt}
	if err != nil {
		response["error"] = redact.String(err.Error())
	}
	writeJSON(w, http.StatusOK, response)
}

// acceptedWorkerRecipe re-validates the recipe the head sent. The head is
// authenticated, not trusted to have validated anything: this node executes
// against its own schema rules or not at all.
func acceptedWorkerRecipe(r recipe.Recipe) error {
	if err := recipe.Validate(r); err != nil {
		return errors.New("the proposed recipe is not valid on this Spark: " + err.Error())
	}
	if !r.Distributed() {
		return errors.New("a single-Spark recipe is never run on behalf of another Spark")
	}
	return nil
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
