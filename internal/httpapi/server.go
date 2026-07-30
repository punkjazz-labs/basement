package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/punkjazz-labs/runonspark-manager/internal/auth"
	"github.com/punkjazz-labs/runonspark-manager/internal/engine"
	"github.com/punkjazz-labs/runonspark-manager/internal/inventory"
	"github.com/punkjazz-labs/runonspark-manager/internal/operations"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
	"github.com/punkjazz-labs/runonspark-manager/internal/redact"
	"github.com/punkjazz-labs/runonspark-manager/internal/store"
	"github.com/punkjazz-labs/runonspark-manager/internal/webui"
)

type Server struct {
	version   string
	auth      *auth.Manager
	store     *store.Store
	inventory inventory.Provider
	executor  operations.Executor
	engine    *engine.Engine
	recipes   []recipe.Recipe
	handler   http.Handler
}

type preflightCheck struct {
	Operation string `json:"operation"`
	OK        bool   `json:"ok"`
	Receipt   any    `json:"receipt,omitempty"`
	Error     string `json:"error,omitempty"`
}
type preflightResponse struct {
	RecipeID        string           `json:"recipe_id"`
	Ready           bool             `json:"ready"`
	Checks          []preflightCheck `json:"checks"`
	LicenceAccepted bool             `json:"licence_accepted"`
	Secrets         map[string]bool  `json:"secrets"`
}

func New(version string, authManager *auth.Manager, s *store.Store, provider inventory.Provider, executor operations.Executor, e *engine.Engine, recipes []recipe.Recipe) *Server {
	server := &Server{version: version, auth: authManager, store: s, inventory: provider, executor: executor, engine: e, recipes: recipes}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.health)
	mux.HandleFunc("/api/v1/auth/pair", server.pair)
	mux.HandleFunc("/api/v1/auth/status", server.authStatus)
	mux.HandleFunc("/api/v1/system", server.withReadAuth(server.system))
	mux.HandleFunc("/api/v1/preflight", server.withReadAuth(server.preflight))
	mux.HandleFunc("/api/v1/recipes", server.withReadAuth(server.listRecipes))
	mux.HandleFunc("/api/v1/models", server.withReadAuth(server.listModels))
	mux.HandleFunc("/api/v1/models/", server.withReadAuth(server.modelAction))
	mux.HandleFunc("/api/v1/jobs", server.withReadAuth(server.listJobs))
	mux.HandleFunc("/api/v1/jobs/", server.withReadAuth(server.jobAction))
	mux.HandleFunc("/api/v1/diagnostics", server.withReadAuth(server.diagnostics))
	assets, _ := fs.Sub(webui.Assets, "assets")
	fileServer := http.FileServer(http.FS(assets))
	mux.Handle("/assets/", http.StripPrefix("/assets/", fileServer))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, _ := fs.ReadFile(assets, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})
	server.handler = securityHeaders(mux)
	return server
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.version})
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := auth.ValidateOrigin(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var request struct {
		Token string `json:"token"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	csrf, err := s.auth.Pair(w, request.Token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": csrf})
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	csrf, ok := s.auth.Authenticate(r)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": ok, "csrf_token": csrf})
}

func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	system, err := s.inventory.Inspect(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	models, _ := s.store.Models(r.Context())
	response := struct {
		inventory.System
		ManagerVersion  string                 `json:"manager_version"`
		InstalledModels []store.InstalledModel `json:"installed_models"`
	}{System: system, ManagerVersion: s.version, InstalledModels: models}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) preflight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	recipeID := r.URL.Query().Get("recipe_id")
	selected, ok := recipe.Find(s.recipes, recipeID)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("recipe not found"))
		return
	}
	writeJSON(w, http.StatusOK, s.runPreflight(r.Context(), selected))
}

func (s *Server) runPreflight(ctx context.Context, selected recipe.Recipe) preflightResponse {
	response := preflightResponse{RecipeID: selected.ID, Ready: true, Secrets: map[string]bool{}}
	for _, op := range selected.Operations {
		if !strings.HasPrefix(op.Type, "verify_") {
			break
		}
		receipt, err := s.executor.Execute(ctx, operations.Execution{Kind: "preflight"}, op, selected, nil)
		if err != nil && op.Type == "verify_port" {
			if owner := s.managedPortOwner(ctx, selected); owner != "" {
				receipt = map[string]any{"host_port": selected.Service.DefaultHostPort, "occupied_by_managed_recipe": owner, "available_after_switch": true}
				err = nil
			}
		}
		check := preflightCheck{Operation: op.Type, OK: err == nil, Receipt: receipt}
		if err != nil {
			check.Error = err.Error()
			response.Ready = false
		}
		response.Checks = append(response.Checks, check)
	}
	accepted, _ := s.store.LicenceAccepted(ctx, selected.ID, selected.Version)
	response.LicenceAccepted = accepted
	for _, name := range selected.Requirements.Secrets {
		_, present := os.LookupEnv(name)
		response.Secrets[name] = present
		if !present {
			response.Ready = false
		}
	}
	return response
}

func (s *Server) managedPortOwner(ctx context.Context, selected recipe.Recipe) string {
	models, err := s.store.Models(ctx)
	if err != nil {
		return ""
	}
	for _, model := range models {
		if !model.Active || model.RecipeID == selected.ID {
			continue
		}
		if active, ok := recipe.Find(s.recipes, model.RecipeID); ok && active.Service.DefaultHostPort == selected.Service.DefaultHostPort {
			return active.ID
		}
	}
	return ""
}

func (s *Server) listRecipes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	type view struct {
		recipe.Recipe
		ArtifactBytes int64 `json:"artifact_bytes"`
		RequiredBytes int64 `json:"required_bytes"`
	}
	result := make([]view, 0, len(s.recipes))
	for _, item := range s.recipes {
		result = append(result, view{Recipe: item, ArtifactBytes: item.TotalArtifactBytes(), RequiredBytes: item.RequiredBytes()})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	models, err := s.store.Models(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, models)
}
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	jobs, err := s.store.ListJobs(r.Context(), 50)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, jobs)
}

func (s *Server) modelAction(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(path.Clean(r.URL.Path), "/api/v1/models/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 1 || parts[0] == "." {
		http.NotFound(w, r)
		return
	}
	selected, ok := recipe.Find(s.recipes, parts[0])
	if !ok {
		writeError(w, 404, errors.New("recipe not found"))
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 {
		if _, err := s.store.Model(r.Context(), selected.ID); err != nil {
			writeError(w, http.StatusConflict, errors.New("model is not installed"))
			return
		}
		s.remove(w, r, selected)
		return
	}
	if r.Method != http.MethodPost || len(parts) != 2 {
		methodNotAllowed(w)
		return
	}
	switch parts[1] {
	case "install":
		s.install(w, r, selected)
	case "start", "stop", "smoke-test":
		if _, err := s.store.Model(r.Context(), selected.ID); err != nil {
			writeError(w, http.StatusConflict, errors.New("model is not installed"))
			return
		}
		s.createJob(w, r, parts[1], selected, map[string]any{})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) install(w http.ResponseWriter, r *http.Request, selected recipe.Recipe) {
	var request struct {
		Confirmed     bool `json:"confirmed"`
		AcceptLicence bool `json:"accept_licence"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, 400, err)
		return
	}
	if !request.Confirmed {
		writeError(w, 400, errors.New("explicit installation confirmation is required"))
		return
	}
	preflight := s.runPreflight(r.Context(), selected)
	if !preflight.Ready {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "preflight failed without mutating runtime state", "preflight": preflight})
		return
	}
	if selected.Requirements.RequiredLicenceAccept && !request.AcceptLicence {
		writeError(w, 400, errors.New("model licence acceptance is required"))
		return
	}
	if request.AcceptLicence {
		if err := s.store.AcceptLicence(r.Context(), selected.ID, selected.Version); err != nil {
			writeError(w, 500, err)
			return
		}
	}
	s.createJob(w, r, "install", selected, map[string]any{"confirmed": true})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request, selected recipe.Recipe) {
	var request struct {
		RemoveArtifacts      bool  `json:"remove_artifacts"`
		ExpectedReclaimBytes int64 `json:"expected_reclaim_bytes"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, 400, err)
		return
	}
	expected := int64(0)
	if request.RemoveArtifacts {
		expected = selected.TotalArtifactBytes()
	}
	if request.ExpectedReclaimBytes != expected {
		writeError(w, 400, fmt.Errorf("reclaim confirmation mismatch: expected %d bytes", expected))
		return
	}
	s.createJob(w, r, "remove", selected, engine.RemovePayload{RemoveArtifacts: request.RemoveArtifacts})
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request, kind string, selected recipe.Recipe, payload any) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, 400, errors.New("a valid Idempotency-Key header is required"))
		return
	}
	job, created, err := s.store.CreateJob(r.Context(), kind, selected.ID, key, payload)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if created || job.State == "failed" || job.State == "interrupted" {
		s.engine.Start(job.ID)
	}
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"job": job, "created": created})
}

func (s *Server) jobAction(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(path.Clean(r.URL.Path), "/api/v1/jobs/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 1 || parts[0] == "." {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		job, err := s.store.GetJob(r.Context(), parts[0])
		if err != nil {
			writeError(w, 404, errors.New("job not found"))
			return
		}
		writeJSON(w, 200, job)
		return
	}
	if len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet {
		s.events(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		if err := s.auth.AuthorizeMutation(r); err != nil {
			writeError(w, 403, err)
			return
		}
		if err := s.engine.Cancel(r.Context(), parts[0]); err != nil {
			writeError(w, 409, err)
			return
		}
		writeJSON(w, 200, map[string]any{"cancelled": true})
		return
	}
	methodNotAllowed(w)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, jobID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, errors.New("streaming is unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := ""
	for {
		job, err := s.store.GetJob(r.Context(), jobID)
		if err != nil {
			return
		}
		body, _ := json.Marshal(job)
		current := string(body)
		if current != last {
			_, _ = fmt.Fprintf(w, "event: job\ndata: %s\n\n", body)
			flusher.Flush()
			last = current
		}
		if terminal(job.State) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	system, _ := s.inventory.Inspect(r.Context())
	jobs, _ := s.store.ListJobs(r.Context(), 20)
	models, _ := s.store.Models(r.Context())
	recentLogs := make([]string, 0)
	for _, job := range jobs {
		if job.Error != "" {
			recentLogs = append(recentLogs, fmt.Sprintf("job %s %s: %s", job.ID, job.State, job.Error))
		}
		for _, step := range job.Steps {
			if step.Error != "" {
				recentLogs = append(recentLogs, fmt.Sprintf("job %s step %d %s: %s", job.ID, step.Index, step.Operation, step.Error))
			}
		}
		if len(recentLogs) >= 100 {
			break
		}
	}
	bundle := map[string]any{
		"format": "runonspark-diagnostics-v1", "generated_at": time.Now().UTC().Format(time.RFC3339Nano),
		"manager_version": s.version, "system": system, "recipes": s.recipes, "models": models,
		"jobs": jobs, "recent_logs": recentLogs, "redacted": true,
	}
	body, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	body = []byte(redact.String(string(body)))
	if redact.ContainsLikelySecret(string(body)) {
		writeError(w, http.StatusInternalServerError, errors.New("diagnostic export failed secret scan"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="runonspark-diagnostics-%s.json"`, time.Now().UTC().Format("20060102T150405Z")))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

func (s *Server) withReadAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.auth.Authenticate(r); !ok {
			writeError(w, 401, errors.New("authentication required"))
			return
		}
		next(w, r)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
func decodeBody(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid request: exactly one JSON object is required")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
}
func terminal(value string) bool {
	switch value {
	case "ready", "failed", "cancelled", "stopped", "removed":
		return true
	}
	return false
}
