package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	dataDir   string
	auth      *auth.Manager
	store     *store.Store
	inventory inventory.Provider
	executor  operations.Executor
	engine    *engine.Engine
	recipes   []recipe.Recipe
	handler   http.Handler
	metrics   *http.Client

	updateMu      sync.Mutex
	updateResult  map[string]any
	updateFetched time.Time
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

func New(version, dataDir string, authManager *auth.Manager, s *store.Store, provider inventory.Provider, executor operations.Executor, e *engine.Engine, recipes []recipe.Recipe) *Server {
	server := &Server{version: version, dataDir: dataDir, auth: authManager, store: s, inventory: provider, executor: executor, engine: e, recipes: recipes, metrics: &http.Client{Timeout: 3 * time.Second}}
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
	mux.HandleFunc("/api/v1/keys", server.keys)
	mux.HandleFunc("/api/v1/keys/", server.keyAction)
	mux.HandleFunc("/api/v1/telemetry", server.withReadAuth(server.telemetry))
	mux.HandleFunc("/api/v1/storage", server.withReadAuth(server.storageBreakdown))
	mux.HandleFunc("/api/v1/update", server.withReadAuth(server.updateCheck))
	mux.HandleFunc("/v1/", server.proxyModel)
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
	type managedNode struct {
		Hostname    string `json:"hostname"`
		ProductName string `json:"product_name"`
		DGXSpark    bool   `json:"dgx_spark"`
		Local       bool   `json:"local"`
		Ready       bool   `json:"ready"`
	}
	type hardwareScope struct {
		Mode               string        `json:"mode"`
		DetectedSparkCount int           `json:"detected_spark_count"`
		ManagedNodes       []managedNode `json:"managed_nodes"`
	}
	detected := 0
	if system.DGXSpark {
		detected = 1
	}
	response := struct {
		inventory.System
		ManagerVersion  string                 `json:"manager_version"`
		InstalledModels []store.InstalledModel `json:"installed_models"`
		HardwareScope   hardwareScope          `json:"hardware_scope"`
	}{
		System:          system,
		ManagerVersion:  s.version,
		InstalledModels: models,
		HardwareScope: hardwareScope{
			Mode:               "local-manager",
			DetectedSparkCount: detected,
			ManagedNodes: []managedNode{{
				Hostname: system.Hostname, ProductName: system.ProductName, DGXSpark: system.DGXSpark,
				Local: true, Ready: system.DGXSpark && len(system.Blocking) == 0,
			}},
		},
	}
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
	checkedLiveMemory := false
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
		if op.Type == "verify_memory" {
			checkedLiveMemory = true
		}
		if err != nil {
			check.Error = err.Error()
			response.Ready = false
			if op.Type == "verify_disk" {
				check.Receipt = s.withReclaimCandidates(ctx, selected, receipt)
			}
		}
		response.Checks = append(response.Checks, check)
	}
	if !checkedLiveMemory {
		receipt, err := s.executor.Execute(ctx, operations.Execution{Kind: "preflight"}, recipe.Operation{Type: "verify_memory"}, selected, nil)
		if err != nil {
			if owner := s.managedPortOwner(ctx, selected); owner != "" {
				receipt = map[string]any{"occupied_by_managed_recipe": owner, "rechecked_after_switch_stop": true}
				err = nil
			}
		}
		check := preflightCheck{Operation: "verify_memory", OK: err == nil, Receipt: receipt}
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

// withReclaimCandidates turns a bare "insufficient disk" failure into an
// actionable one by listing installed models whose removal would free space.
func (s *Server) withReclaimCandidates(ctx context.Context, selected recipe.Recipe, receipt any) any {
	models, err := s.store.Models(ctx)
	if err != nil {
		return receipt
	}
	type candidate struct {
		RecipeID    string `json:"recipe_id"`
		DisplayName string `json:"display_name"`
		Bytes       int64  `json:"bytes"`
		Active      bool   `json:"active"`
		LastUsed    string `json:"last_used"`
	}
	candidates := []candidate{}
	for _, model := range models {
		if model.RecipeID == selected.ID {
			continue
		}
		item, ok := recipe.Find(s.recipes, model.RecipeID)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{RecipeID: item.ID, DisplayName: item.DisplayName, Bytes: item.TotalArtifactBytes(), Active: model.Active, LastUsed: model.UpdatedAt})
	}
	if len(candidates) == 0 {
		return receipt
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Bytes > candidates[j].Bytes })
	wrapped := map[string]any{"reclaim_candidates": candidates}
	if base, ok := receipt.(map[string]any); ok {
		for key, value := range base {
			wrapped[key] = value
		}
	}
	return wrapped
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
	case "start", "stop", "smoke-test", "benchmark":
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

// proxyModel is the stable OpenAI-compatible endpoint: it forwards /v1/* to
// the active model's loopback-only port. Clients authenticate with an API key
// (Authorization: Bearer rosk_...) or an existing console session, so the
// endpoint URL never changes when models are switched and the raw model port
// is never exposed to the network (ADR 0007).
func (s *Server) proxyModel(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeInference(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", "provide a valid RunOnSpark API key as 'Authorization: Bearer rosk_...'")
		return
	}
	active, ok := s.activeReadyRecipe(r.Context())
	if !ok {
		writeOpenAIError(w, http.StatusServiceUnavailable, "model_not_ready", "no model is active and ready; start one from the RunOnSpark console")
		return
	}
	target := fmt.Sprintf("127.0.0.1:%d", active.Service.DefaultHostPort)
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = target
			req.Host = target
			// Never forward manager credentials to the model runtime.
			req.Header.Del("Cookie")
			req.Header.Del("Authorization")
			req.Header.Del("X-CSRF-Token")
			req.Header.Del("Origin")
			req.Header.Del("Referer")
		},
		FlushInterval: -1, // stream token-by-token
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "the model runtime did not respond: "+err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) authorizeInference(r *http.Request) bool {
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		if s.store.VerifyAPIKey(r.Context(), strings.TrimPrefix(header, "Bearer ")) {
			return true
		}
	}
	_, ok := s.auth.Authenticate(r)
	return ok
}

func (s *Server) activeReadyRecipe(ctx context.Context) (recipe.Recipe, bool) {
	models, err := s.store.Models(ctx)
	if err != nil {
		return recipe.Recipe{}, false
	}
	for _, model := range models {
		if !model.Active || model.Status != "ready" {
			continue
		}
		if active, ok := recipe.Find(s.recipes, model.RecipeID); ok {
			return active, true
		}
	}
	return recipe.Recipe{}, false
}

func (s *Server) keys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.auth.Authenticate(r); !ok {
			writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		list, err := s.store.APIKeys(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		if err := s.auth.AuthorizeMutation(r); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
		var request struct {
			Name string `json:"name"`
		}
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		key, secret, err := s.store.CreateAPIKey(r.Context(), request.Name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"key": key, "secret": secret, "note": "store this secret now; it is not retrievable later"})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) keyAction(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(path.Clean(r.URL.Path), "/api/v1/keys/")
	if id == "" || id == "." || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if err := s.store.RevokeAPIKey(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, errors.New("key not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

func (s *Server) telemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	system, err := s.inventory.Inspect(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	response := map[string]any{
		"sampled_at":               time.Now().UTC().Format(time.RFC3339Nano),
		"memory_total":             system.MemoryTotal,
		"memory_available":         system.MemoryAvailable,
		"gpu_memory_total":         system.GPUMemoryTotal,
		"gpu_memory_free":          system.GPUMemoryFree,
		"storage_total":            system.StorageTotal,
		"storage_available":        system.StorageAvailable,
		"docker_storage_available": system.DockerStorageAvailable,
	}
	if active, ok := s.activeReadyRecipe(r.Context()); ok {
		model := map[string]any{"recipe_id": active.ID, "served_model_id": active.Service.ServedModelID}
		if metrics := s.vllmMetrics(r.Context(), active); metrics != nil {
			model["vllm"] = metrics
		}
		response["active_model"] = model
	}
	writeJSON(w, http.StatusOK, response)
}

// vllmMetrics samples the runtime's Prometheus endpoint for the handful of
// series the console renders; absence of any series is not an error.
func (s *Server) vllmMetrics(ctx context.Context, r recipe.Recipe) map[string]float64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", r.Service.DefaultHostPort), nil)
	if err != nil {
		return nil
	}
	resp, err := s.metrics.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil
	}
	wanted := map[string]string{
		"vllm:num_requests_running":    "requests_running",
		"vllm:num_requests_waiting":    "requests_waiting",
		"vllm:gpu_cache_usage_perc":    "kv_cache_usage",
		"vllm:prompt_tokens_total":     "prompt_tokens_total",
		"vllm:generation_tokens_total": "generation_tokens_total",
	}
	result := map[string]float64{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if brace := strings.IndexByte(name, '{'); brace >= 0 {
			name = name[:brace]
		} else if space := strings.IndexByte(name, ' '); space >= 0 {
			name = name[:space]
		}
		key, ok := wanted[name]
		if !ok {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		result[key] += value
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (s *Server) storageBreakdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	system, _ := s.inventory.Inspect(r.Context())
	models, _ := s.store.Models(r.Context())
	installed := map[string]store.InstalledModel{}
	for _, model := range models {
		installed[model.RecipeID] = model
	}
	type artifactUse struct {
		Repository string   `json:"repository"`
		Revision   string   `json:"revision"`
		Bytes      int64    `json:"bytes"`
		RecipeIDs  []string `json:"recipe_ids"`
	}
	artifacts := []artifactUse{}
	artifactRoot := filepath.Join(s.dataDir, "artifacts")
	if repoDirs, err := os.ReadDir(artifactRoot); err == nil {
		for _, repoDir := range repoDirs {
			if !repoDir.IsDir() {
				continue
			}
			repository := strings.ReplaceAll(repoDir.Name(), "--", "/")
			revisions, err := os.ReadDir(filepath.Join(artifactRoot, repoDir.Name()))
			if err != nil {
				continue
			}
			for _, revision := range revisions {
				if !revision.IsDir() {
					continue
				}
				use := artifactUse{Repository: repository, Revision: revision.Name(), Bytes: dirBytes(filepath.Join(artifactRoot, repoDir.Name(), revision.Name())), RecipeIDs: []string{}}
				for _, item := range s.recipes {
					for _, artifact := range item.Artifacts {
						if artifact.Repository == repository && artifact.Revision == revision.Name() {
							use.RecipeIDs = append(use.RecipeIDs, item.ID)
						}
					}
				}
				artifacts = append(artifacts, use)
			}
		}
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Bytes > artifacts[j].Bytes })
	type cacheUse struct {
		RecipeID string `json:"recipe_id"`
		Bytes    int64  `json:"bytes"`
	}
	caches := []cacheUse{}
	cacheRoot := filepath.Join(s.dataDir, "caches")
	if cacheDirs, err := os.ReadDir(cacheRoot); err == nil {
		for _, cacheDir := range cacheDirs {
			if cacheDir.IsDir() {
				caches = append(caches, cacheUse{RecipeID: cacheDir.Name(), Bytes: dirBytes(filepath.Join(cacheRoot, cacheDir.Name()))})
			}
		}
	}
	databaseBytes := int64(0)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(filepath.Join(s.dataDir, "manager.db"+suffix)); err == nil {
			databaseBytes += info.Size()
		}
	}
	total := databaseBytes + dirBytes(filepath.Join(s.dataDir, "configs"))
	for _, artifact := range artifacts {
		total += artifact.Bytes
	}
	for _, cache := range caches {
		total += cache.Bytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data_dir":            s.dataDir,
		"storage_total":       system.StorageTotal,
		"storage_available":   system.StorageAvailable,
		"total_managed_bytes": total,
		"database_bytes":      databaseBytes,
		"artifacts":           artifacts,
		"caches":              caches,
	})
}

func dirBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := entry.Info(); err == nil && entry.Type().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

const updateRepository = "punkjazz-labs/runonspark-manager"

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if s.updateResult != nil && time.Since(s.updateFetched) < 6*time.Hour {
		writeJSON(w, http.StatusOK, s.updateResult)
		return
	}
	result := map[string]any{"current_version": s.version, "checked": false, "update_available": false}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+updateRepository+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var release struct {
				TagName string `json:"tag_name"`
				HTMLURL string `json:"html_url"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release) == nil && release.TagName != "" {
				latest := strings.TrimPrefix(release.TagName, "v")
				result["checked"] = true
				result["latest_version"] = latest
				result["release_url"] = release.HTMLURL
				result["update_available"] = latest != strings.TrimPrefix(s.version, "v") && s.version != "dev"
			}
		} else if resp.StatusCode == http.StatusNotFound {
			result["checked"] = true
			result["note"] = "no published releases yet"
		}
	}
	result["checked_at"] = time.Now().UTC().Format(time.RFC3339)
	s.updateResult = result
	s.updateFetched = time.Now()
	writeJSON(w, http.StatusOK, result)
}

func writeOpenAIError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": kind, "message": message}})
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
