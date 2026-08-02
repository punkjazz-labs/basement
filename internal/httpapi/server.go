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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// effective is the current merged catalog (one recipe per ID, the
	// newest version known) — what listing the catalog and starting a new
	// install or update target. all is every distinct (id, version) ever
	// verified and only ever grows; anything acting on an ALREADY-INSTALLED
	// model (proxying inference, checking what an artifact deletion would
	// affect, reclaim byte counts) must resolve through it via
	// pinnedOrEffective, or a background recipe-index update could silently
	// change which port, config, or files an existing install means. Both
	// are swapped together by SetRecipes; readers never take a lock.
	effective atomic.Pointer[[]recipe.Recipe]
	all       atomic.Pointer[[]recipe.Recipe]
	handler   http.Handler
	metrics   *http.Client
	// peerClient is a short-timeout client dedicated to server-to-server
	// fleet calls; kept separate from metrics so its purpose (reaching
	// another Spark, not this one's own vLLM runtime) is unambiguous at
	// call sites.
	peerClient *http.Client
	// closing releases long-lived SSE streams the moment shutdown starts, so
	// a service restart never waits out the graceful-drain timeout on
	// progress streams that would otherwise stay open forever.
	closing   chan struct{}
	closeOnce sync.Once

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
	server := &Server{version: version, dataDir: dataDir, auth: authManager, store: s, inventory: provider, executor: executor, engine: e, metrics: &http.Client{Timeout: 3 * time.Second}, peerClient: &http.Client{Timeout: 3 * time.Second, CheckRedirect: refusePeerRedirect}, closing: make(chan struct{})}
	server.SetRecipes(recipes, recipes)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.health)
	mux.HandleFunc("/api/v1/auth/pair", server.pair)
	mux.HandleFunc("/api/v1/auth/status", server.authStatus)
	// system, models and telemetry are also the three endpoints a peer
	// manager reads through /summary, so they accept API-key auth in
	// addition to a console session (ADR-scale change: see withPeerReadAuth).
	mux.HandleFunc("/api/v1/system", server.withPeerReadAuth(server.system))
	mux.HandleFunc("/api/v1/preflight", server.withReadAuth(server.preflight))
	mux.HandleFunc("/api/v1/recipes", server.withReadAuth(server.listRecipes))
	mux.HandleFunc("/api/v1/models", server.withPeerReadAuth(server.listModels))
	mux.HandleFunc("/api/v1/models/", server.withReadAuth(server.modelAction))
	mux.HandleFunc("/api/v1/jobs", server.withReadAuth(server.listJobs))
	mux.HandleFunc("/api/v1/jobs/", server.withReadAuth(server.jobAction))
	mux.HandleFunc("/api/v1/diagnostics", server.withReadAuth(server.diagnostics))
	mux.HandleFunc("/api/v1/keys", server.keys)
	mux.HandleFunc("/api/v1/keys/", server.keyAction)
	mux.HandleFunc("/api/v1/telemetry", server.withPeerReadAuth(server.telemetry))
	mux.HandleFunc("/api/v1/storage", server.withReadAuth(server.storageBreakdown))
	mux.HandleFunc("/api/v1/storage/artifacts", server.withReadAuth(server.deleteArtifact))
	mux.HandleFunc("/api/v1/update", server.withReadAuth(server.updateCheck))
	mux.HandleFunc("/api/v1/peers", server.peersCollection)
	mux.HandleFunc("/api/v1/peers/", server.withReadAuth(server.peerAction))
	mux.HandleFunc("/v1/", server.proxyModel)
	assets, _ := fs.Sub(webui.Assets, "assets")
	fileServer := http.FileServer(http.FS(assets))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			// The entry document must revalidate on every load — a cached
			// index keeps referencing old hashed bundles after an upgrade.
			data, _ := fs.ReadFile(assets, "index.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(data)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if _, err := fs.Stat(assets, name); err != nil {
			http.NotFound(w, r)
			return
		}
		// Hashed bundles are immutable by construction; logos and other
		// stable files can revalidate cheaply.
		if strings.HasPrefix(name, "static/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
	server.handler = securityHeaders(mux)
	return server
}

func (s *Server) Handler() http.Handler { return s.handler }

// SetRecipes replaces the catalog and its version history in one call — see
// the field comments on Server. Safe to call from any goroutine at any
// time, including while requests are in flight.
func (s *Server) SetRecipes(all, effective []recipe.Recipe) {
	s.all.Store(&all)
	s.effective.Store(&effective)
}

func (s *Server) effectiveRecipes() []recipe.Recipe {
	if p := s.effective.Load(); p != nil {
		return *p
	}
	return nil
}

func (s *Server) allRecipes() []recipe.Recipe {
	if p := s.all.Load(); p != nil {
		return *p
	}
	return nil
}

// pinnedOrEffective resolves the recipe an already-installed model was
// actually built from — the exact (id, version) it was recorded with —
// falling back to whatever the effective catalog currently has for that ID
// on the rare chance history does not contain it (it should always grow,
// never shrink; see recipefeed.Fetcher).
func (s *Server) pinnedOrEffective(id string, version int) (recipe.Recipe, bool) {
	if pinned, ok := recipe.FindVersion(s.allRecipes(), id, version); ok {
		return pinned, true
	}
	return recipe.Find(s.effectiveRecipes(), id)
}

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
	// Preflight always evaluates against the current catalog entry: it
	// exists to decide whether a fresh install or an update can proceed,
	// and that always targets the newest known version.
	selected, ok := recipe.Find(s.effectiveRecipes(), recipeID)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("recipe not found"))
		return
	}
	writeJSON(w, http.StatusOK, s.runPreflight(r.Context(), selected))
}

func (s *Server) runPreflight(ctx context.Context, selected recipe.Recipe) preflightResponse {
	response := preflightResponse{RecipeID: selected.ID, Ready: true, Secrets: map[string]bool{}}
	// The advisory checks see other running installs' disk reservations,
	// exactly like the real verify_disk step will, so the dialog and the
	// job it starts cannot disagree while another download runs.
	execution := operations.Execution{Kind: "preflight", ReservedBytes: s.engine.ReservedDiskBytes()}
	checkedLiveMemory := false
	for _, op := range selected.Operations {
		if !strings.HasPrefix(op.Type, "verify_") {
			break
		}
		receipt, err := s.executor.Execute(ctx, execution, op, selected, nil)
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
		receipt, err := s.executor.Execute(ctx, execution, recipe.Operation{Type: "verify_memory"}, selected, nil)
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
		// The exact installed version: what removing this model would
		// actually reclaim, which can differ from the catalog's current
		// (possibly newer) entry for the same ID.
		item, ok := s.pinnedOrEffective(model.RecipeID, model.RecipeVersion)
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
		// The port an active model actually has bound is whatever its
		// installed version declared, not the catalog's current entry.
		if active, ok := s.pinnedOrEffective(model.RecipeID, model.RecipeVersion); ok && active.Service.DefaultHostPort == selected.Service.DefaultHostPort {
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
	effective := s.effectiveRecipes()
	result := make([]view, 0, len(effective))
	for _, item := range effective {
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
	// Every action here either targets a fresh install/update (the effective,
	// newest catalog entry) or an already-installed model, whose handler
	// (remove, and createJob's callers) re-resolves the exact installed
	// version itself where a byte count or other version-sensitive field
	// matters. This lookup only needs to know the ID is real; it falls back
	// to the full version history so a model whose recipe has since been
	// delisted upstream (no tombstone propagation exists yet — see the spec
	// 04 report) stays reachable for its own start/stop/remove, even though
	// it can no longer be freshly installed.
	selected, ok := recipe.Find(s.effectiveRecipes(), parts[0])
	if !ok {
		selected, ok = recipe.Find(s.allRecipes(), parts[0])
	}
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
		Confirmed     bool  `json:"confirmed"`
		AcceptLicence bool  `json:"accept_licence"`
		Activate      *bool `json:"activate"`
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
	// Activate defaults to true, preserving the historical install-and-serve
	// behaviour for callers that omit it.
	activate := request.Activate == nil || *request.Activate
	s.createJob(w, r, "install", selected, map[string]any{"confirmed": true, "activate": activate})
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
		// What removal actually reclaims is governed by the exact installed
		// version's artifacts, not the catalog's current (possibly newer)
		// entry for this ID.
		target := selected
		if model, err := s.store.Model(r.Context(), selected.ID); err == nil {
			if pinned, ok := s.pinnedOrEffective(selected.ID, model.RecipeVersion); ok {
				target = pinned
			}
		}
		expected = target.TotalArtifactBytes()
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
		case <-s.closing:
			return
		case <-ticker.C:
		}
	}
}

// Close releases every long-lived stream so graceful shutdown completes
// immediately. Registered with the HTTP server's shutdown hooks.
func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.closing) })
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
		"format": "basement-diagnostics-v1", "generated_at": time.Now().UTC().Format(time.RFC3339Nano),
		"manager_version": s.version, "system": system, "recipes": s.effectiveRecipes(), "models": models,
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
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="basement-diagnostics-%s.json"`, time.Now().UTC().Format("20060102T150405Z")))
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
		writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", "provide a valid basement API key as 'Authorization: Bearer rosk_...'")
		return
	}
	active, ok := s.activeReadyRecipe(r.Context())
	if !ok {
		writeOpenAIError(w, http.StatusServiceUnavailable, "model_not_ready", "no model is active and ready; start one from the basement console")
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

// activeReadyRecipe resolves the exact recipe version the active model was
// installed and started with — never the catalog's current entry for that
// ID. This is what proxyModel dials and what telemetry reports on; using a
// newer, not-yet-installed recipe's port or served_model_id here would
// misroute live inference traffic the moment a background recipe update
// lands, without anything about the actually-running container changing.
func (s *Server) activeReadyRecipe(ctx context.Context) (recipe.Recipe, bool) {
	models, err := s.store.Models(ctx)
	if err != nil {
		return recipe.Recipe{}, false
	}
	for _, model := range models {
		if !model.Active || model.Status != "ready" {
			continue
		}
		if active, ok := s.pinnedOrEffective(model.RecipeID, model.RecipeVersion); ok {
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

// peerAllowedPaths is the entire surface a fleet peer will ever expose to
// this manager. fetchPeerJSON refuses anything outside it, so this stays an
// enforced allowlist rather than an assumption baked into call sites.
var peerAllowedPaths = map[string]bool{
	"/api/v1/system":    true,
	"/api/v1/models":    true,
	"/api/v1/telemetry": true,
}

type peerView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
}

func (s *Server) peersCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.auth.Authenticate(r); !ok {
			writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		s.listPeers(w, r)
	case http.MethodPost:
		if err := s.auth.AuthorizeMutation(r); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
		s.createPeer(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) listPeers(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.Peers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	result := make([]peerView, 0, len(list))
	for _, peer := range list {
		result = append(result, peerView{ID: peer.ID, Name: peer.Name, BaseURL: peer.BaseURL})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createPeer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	baseURL, err := normalizedPeerBaseURL(request.BaseURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(request.APIKey) == "" {
		writeError(w, http.StatusBadRequest, errors.New("an API key is required"))
		return
	}
	// Prove the URL and key actually reach a Spark before this is saved;
	// a peer entry that has never been confirmed reachable is worse than
	// no entry at all.
	if _, err := s.fetchPeerJSON(r.Context(), baseURL, request.APIKey, "/api/v1/system"); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("could not reach that Spark with this key, so check the URL and key"))
		return
	}
	peer, err := s.store.CreatePeer(r.Context(), request.Name, baseURL, request.APIKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, peerView{ID: peer.ID, Name: peer.Name, BaseURL: peer.BaseURL})
}

func (s *Server) peerAction(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(path.Clean(r.URL.Path), "/api/v1/peers/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 1 || parts[0] == "" || parts[0] == "." {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := s.auth.AuthorizeMutation(r); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
		if err := s.store.DeletePeer(r.Context(), id); err != nil {
			writeError(w, http.StatusNotFound, errors.New("that Spark is not in the fleet"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": true})
		return
	}
	if len(parts) == 2 && parts[1] == "summary" && r.Method == http.MethodGet {
		s.peerSummary(w, r, id)
		return
	}
	methodNotAllowed(w)
}

// peerSummary proxies exactly three read-only endpoints on the peer
// (system, models, telemetry) and merges their bodies into one response.
// A peer that cannot be reached, times out, or fails auth is reported as
// reachable: false rather than turning into an error for the whole call, so
// one down machine never breaks the rest of the fleet view.
func (s *Server) peerSummary(w http.ResponseWriter, r *http.Request, id string) {
	peer, apiKey, err := s.store.PeerCredentials(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("that Spark is not in the fleet"))
		return
	}
	system, systemErr := s.fetchPeerJSON(r.Context(), peer.BaseURL, apiKey, "/api/v1/system")
	models, modelsErr := s.fetchPeerJSON(r.Context(), peer.BaseURL, apiKey, "/api/v1/models")
	telemetry, telemetryErr := s.fetchPeerJSON(r.Context(), peer.BaseURL, apiKey, "/api/v1/telemetry")
	response := map[string]any{"reachable": systemErr == nil && modelsErr == nil && telemetryErr == nil}
	if systemErr == nil {
		response["system"] = system
	}
	if modelsErr == nil {
		response["models"] = models
	}
	if telemetryErr == nil {
		response["telemetry"] = telemetry
	}
	writeJSON(w, http.StatusOK, response)
}

// fetchPeerJSON calls exactly one allowlisted read-only path on a peer and
// decodes its JSON body. The peer's response is untrusted input: the call is
// time-boxed independently of the caller's own request context, the body is
// size-capped before it is ever parsed, and it is only ever interpreted as
// JSON, never as HTML.
// refusePeerRedirect makes the peer client treat any redirect as a final
// response. A real Spark manager never redirects its API; following one
// would let a malicious "peer" bounce this manager's authenticated GET to a
// host the user never approved.
func refusePeerRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func (s *Server) fetchPeerJSON(ctx context.Context, baseURL, apiKey, endpoint string) (any, error) {
	if !peerAllowedPaths[endpoint] {
		return nil, fmt.Errorf("peer endpoint %s is not allowlisted", endpoint)
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, baseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := s.peerClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer returned status %d", resp.StatusCode)
	}
	var payload any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("peer returned an unreadable response: %w", err)
	}
	return payload, nil
}

// normalizedPeerBaseURL accepts only a bare http(s) origin: LAN HTTP is
// acceptable at this phase (mTLS is a later spec), but a path, query or
// embedded credentials would let a malicious "peer" redirect this manager's
// authenticated calls somewhere the user never approved.
func normalizedPeerBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("enter a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("the URL must start with http:// or https://")
	}
	if u.Host == "" {
		return "", errors.New("the URL must include a host")
	}
	if u.User != nil {
		return "", errors.New("the URL must not include a username or password")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("the URL must not include a path")
	}
	if u.RawQuery != "" {
		return "", errors.New("the URL must not include a query string")
	}
	if u.Fragment != "" {
		return "", errors.New("the URL must not include a fragment")
	}
	return u.Scheme + "://" + u.Host, nil
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
		"gpu_power_draw_watts":     system.GPUPowerDrawWatts,
		"gpu_clock_mhz":            system.GPUClockMHz,
		"gpu_temperature_c":        system.GPUTemperatureC,
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
	// The full version history, not just the current catalog: an artifact
	// or image still on disk for an older-but-still-installed recipe
	// version must not read as orphaned just because the catalog has since
	// moved that ID on to a newer version.
	knownRecipes := s.allRecipes()
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
				seenIDs := map[string]bool{}
				for _, item := range knownRecipes {
					if seenIDs[item.ID] {
						continue // the same ID can appear at several versions
					}
					for _, artifact := range item.Artifacts {
						if artifact.Repository == repository && artifact.Revision == revision.Name() {
							use.RecipeIDs = append(use.RecipeIDs, item.ID)
							seenIDs[item.ID] = true
							break
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
	// Runtime images live in Docker's own store, but they are ours: pulled by
	// install jobs against pinned digests. Attribute them so the storage page
	// accounts for every byte the product is responsible for.
	type imageUse struct {
		Reference string   `json:"reference"`
		Bytes     int64    `json:"bytes"`
		RecipeIDs []string `json:"recipe_ids"`
	}
	images := []imageUse{}
	seenImages := map[string]int{}
	for _, item := range knownRecipes {
		reference := item.Runtime.Reference()
		if index, ok := seenImages[reference]; ok {
			ids := images[index].RecipeIDs
			alreadyListed := false
			for _, id := range ids {
				if id == item.ID {
					alreadyListed = true
					break
				}
			}
			if !alreadyListed {
				images[index].RecipeIDs = append(ids, item.ID)
			}
			continue
		}
		size, present := s.executor.RuntimeImageBytes(r.Context(), item)
		if !present {
			continue
		}
		seenImages[reference] = len(images)
		images = append(images, imageUse{Reference: reference, Bytes: size, RecipeIDs: []string{item.ID}})
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Bytes > images[j].Bytes })
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
	for _, image := range images {
		total += image.Bytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data_dir":            s.dataDir,
		"storage_total":       system.StorageTotal,
		"storage_available":   system.StorageAvailable,
		"total_managed_bytes": total,
		"database_bytes":      databaseBytes,
		"artifacts":           artifacts,
		"caches":              caches,
		"images":              images,
	})
}

// deleteArtifact removes one downloaded weights directory. Installed models
// keep their files (they leave through uninstall, which also clears runtime
// and configuration), and a running job is never pulled out from under.
func (s *Server) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var request struct {
		Repository string `json:"repository"`
		Revision   string `json:"revision"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, 400, err)
		return
	}
	// Resolve against the real on-disk layout so nothing from the request body
	// ever reaches the filesystem as a path.
	target := ""
	artifactRoot := filepath.Join(s.dataDir, "artifacts")
	if repoDirs, err := os.ReadDir(artifactRoot); err == nil {
		for _, repoDir := range repoDirs {
			if !repoDir.IsDir() || strings.ReplaceAll(repoDir.Name(), "--", "/") != request.Repository {
				continue
			}
			revisions, err := os.ReadDir(filepath.Join(artifactRoot, repoDir.Name()))
			if err != nil {
				continue
			}
			for _, revision := range revisions {
				if revision.IsDir() && revision.Name() == request.Revision {
					target = filepath.Join(artifactRoot, repoDir.Name(), revision.Name())
				}
			}
		}
	}
	if target == "" {
		writeError(w, 404, errors.New("no downloaded files match that model"))
		return
	}
	// A safe (conservative) check by design: it asks whether ANY version of
	// a recipe ID this manager has ever known — not just the catalog's
	// current entry — references the artifact, because the catalog can
	// have moved a recipe ID on to a newer version (with a different
	// pinned revision) while an older version is what's actually installed
	// and still using these exact files on disk. Understating "in use"
	// here is how a still-needed model's files get deleted out from under
	// it; overstating it just blocks a deletion that turns out to be safe.
	referencesArtifact := func(recipeID string) bool {
		for _, candidate := range s.allRecipes() {
			if candidate.ID != recipeID {
				continue
			}
			for _, artifact := range candidate.Artifacts {
				if artifact.Repository == request.Repository && artifact.Revision == request.Revision {
					return true
				}
			}
		}
		return false
	}
	models, err := s.store.Models(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	for _, model := range models {
		if referencesArtifact(model.RecipeID) {
			selected, ok := s.pinnedOrEffective(model.RecipeID, model.RecipeVersion)
			name := model.RecipeID
			if ok {
				name = selected.DisplayName
			}
			writeError(w, http.StatusConflict, fmt.Errorf("%s is installed and uses these files, so uninstall it instead", name))
			return
		}
	}
	if jobs, err := s.store.ListJobs(r.Context(), 200); err == nil {
		terminal := map[string]bool{"ready": true, "failed": true, "cancelled": true, "stopped": true, "removed": true}
		for _, job := range jobs {
			if !terminal[job.State] && referencesArtifact(job.RecipeID) {
				writeError(w, http.StatusConflict, errors.New("a job is using these files right now, so wait for it to finish"))
				return
			}
		}
	}
	reclaimed := dirBytes(target)
	if err := os.RemoveAll(target); err != nil {
		writeError(w, 500, err)
		return
	}
	_ = os.Remove(filepath.Dir(target)) // clears the repo folder when this was its last revision
	writeJSON(w, http.StatusOK, map[string]any{"reclaimed_bytes": reclaimed})
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

// withPeerReadAuth additionally accepts an API-key bearer token, so another
// Spark's manager can read this handler on our behalf when assembling a
// fleet summary. It must only ever wrap read-only, non-sensitive handlers:
// system, models and telemetry. Every other /api/v1 route keeps requiring a
// console session via withReadAuth.
func (s *Server) withPeerReadAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
			if s.store.VerifyAPIKey(r.Context(), strings.TrimPrefix(header, "Bearer ")) {
				next(w, r)
				return
			}
		}
		if _, ok := s.auth.Authenticate(r); ok {
			next(w, r)
			return
		}
		writeError(w, 401, errors.New("authentication required"))
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
