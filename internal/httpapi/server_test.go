package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/punkjazz-labs/runonspark-manager/internal/auth"
	"github.com/punkjazz-labs/runonspark-manager/internal/engine"
	"github.com/punkjazz-labs/runonspark-manager/internal/inventory"
	"github.com/punkjazz-labs/runonspark-manager/internal/operations"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
	"github.com/punkjazz-labs/runonspark-manager/internal/store"
)

type readyInventory struct{}

func (readyInventory) Inspect(context.Context) (inventory.System, error) {
	return inventory.System{Hostname: "spark-test", Architecture: "aarch64", OS: "DGX OS", ProductName: "NVIDIA DGX Spark", DGXSpark: true, DockerReady: true, NvidiaRuntimeReady: true, GPUVisible: true, DataDirectoryWritable: true, StorageAvailable: 100_000_000_000, StorageTotal: 200_000_000_000, Ready: true}, nil
}

type apiExecutor struct {
	mu       sync.Mutex
	done     map[string]bool
	running  bool
	failPort bool
}

func (a *apiExecutor) ArtifactPath(r recipe.Recipe) string { return "/managed/" + r.ID }
func (a *apiExecutor) RuntimeImageBytes(_ context.Context, r recipe.Recipe) (int64, bool) {
	return r.Runtime.ImageDiskBytes, true
}
func (a *apiExecutor) Execute(_ context.Context, _ operations.Execution, op recipe.Operation, _ recipe.Recipe, progress operations.Progress) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if op.Type == "verify_port" && a.failPort {
		return nil, errors.New("port 8000 is occupied")
	}
	a.done[op.Type] = true
	if op.Type == "start_container" {
		a.running = true
	}
	if op.Type == "stop_container" {
		a.running = false
	}
	if progress != nil && op.Type == "download_artifact" {
		_ = progress(map[string]any{"bytes_complete": 100, "bytes_total": 100, "percent": 100})
	}
	return map[string]any{"operation": op.Type, "response_non_empty": op.Type == "verify_openai_inference"}, nil
}
func (a *apiExecutor) Completed(_ context.Context, _ operations.Execution, op recipe.Operation, _ recipe.Recipe, _ json.RawMessage) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if op.Type == "start_container" || op.Type == "wait_http" || op.Type == "verify_openai_inference" {
		return a.running
	}
	return a.done[op.Type]
}

func TestAuthenticatedQwenInstallAPI(t *testing.T) {
	dataDir := t.TempDir()
	database, err := store.Open(filepath.Join(dataDir, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authManager, err := auth.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	executor := &apiExecutor{done: map[string]bool{}}
	runner := engine.New(database, executor, recipes)
	server := httptest.NewServer(New("test-version", dataDir, authManager, database, readyInventory{}, executor, runner, recipes).Handler())
	defer server.Close()
	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !bytes.Contains(body, []byte("RunOnSpark Manager")) {
		t.Fatal("embedded UI did not load")
	}
	if response.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("security headers missing")
	}
	response, err = http.Get(server.URL + "/api/v1/system")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated system status=%d", response.StatusCode)
	}
	response.Body.Close()
	tokenBytes, err := os.ReadFile(authManager.PairingTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	bad := doRequest(t, http.MethodPost, server.URL+"/api/v1/auth/pair", `{"token":"`+token+`"}`, nil, map[string]string{"Origin": "https://evil.example"})
	if bad.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin pairing status=%d", bad.StatusCode)
	}
	bad.Body.Close()
	paired := doRequest(t, http.MethodPost, server.URL+"/api/v1/auth/pair", `{"token":"`+token+`"}`, nil, map[string]string{"Origin": server.URL})
	if paired.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(paired.Body)
		t.Fatalf("pair status=%d body=%s", paired.StatusCode, data)
	}
	var pairResult struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(paired.Body).Decode(&pairResult); err != nil {
		t.Fatal(err)
	}
	cookies := paired.Cookies()
	paired.Body.Close()
	if len(cookies) != 1 || pairResult.CSRF == "" {
		t.Fatal("pairing did not issue session and CSRF tokens")
	}
	systemResponse := doRequest(t, http.MethodGet, server.URL+"/api/v1/system", "", cookies, nil)
	var systemResult struct {
		HardwareScope struct {
			Mode               string `json:"mode"`
			DetectedSparkCount int    `json:"detected_spark_count"`
			ManagedNodes       []struct {
				Hostname string `json:"hostname"`
				Local    bool   `json:"local"`
				Ready    bool   `json:"ready"`
			} `json:"managed_nodes"`
		} `json:"hardware_scope"`
	}
	if err := json.NewDecoder(systemResponse.Body).Decode(&systemResult); err != nil {
		t.Fatal(err)
	}
	systemResponse.Body.Close()
	if systemResult.HardwareScope.Mode != "local-manager" || systemResult.HardwareScope.DetectedSparkCount != 1 || len(systemResult.HardwareScope.ManagedNodes) != 1 {
		t.Fatalf("unexpected detected hardware scope: %#v", systemResult.HardwareScope)
	}
	if node := systemResult.HardwareScope.ManagedNodes[0]; node.Hostname != "spark-test" || !node.Local || !node.Ready {
		t.Fatalf("unexpected managed node: %#v", node)
	}
	denied := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+recipes[0].ID+"/install", `{"confirmed":true,"accept_licence":true}`, cookies, map[string]string{"Origin": server.URL, "Idempotency-Key": "install-one"})
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", denied.StatusCode)
	}
	denied.Body.Close()
	headers := map[string]string{"Origin": server.URL, "Idempotency-Key": "install-one", "X-CSRF-Token": pairResult.CSRF}
	installed := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+recipes[0].ID+"/install", `{"confirmed":true,"accept_licence":true}`, cookies, headers)
	if installed.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(installed.Body)
		t.Fatalf("install status=%d body=%s", installed.StatusCode, data)
	}
	var created struct {
		Job     store.Job `json:"job"`
		Created bool      `json:"created"`
	}
	if err := json.NewDecoder(installed.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	installed.Body.Close()
	if !created.Created {
		t.Fatal("install job not created")
	}
	waitAPIJob(t, server.URL, created.Job.ID, cookies, "ready")
	duplicate := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+recipes[0].ID+"/install", `{"confirmed":true,"accept_licence":true}`, cookies, headers)
	var duplicateResult struct {
		Job     store.Job `json:"job"`
		Created bool      `json:"created"`
	}
	if err := json.NewDecoder(duplicate.Body).Decode(&duplicateResult); err != nil {
		t.Fatal(err)
	}
	duplicate.Body.Close()
	if duplicateResult.Created || duplicateResult.Job.ID != created.Job.ID {
		t.Fatalf("duplicate install was not idempotent: %#v", duplicateResult)
	}
	events := doRequest(t, http.MethodGet, server.URL+"/api/v1/jobs/"+created.Job.ID+"/events", "", cookies, nil)
	eventBody, _ := io.ReadAll(events.Body)
	events.Body.Close()
	if !bytes.Contains(eventBody, []byte("event: job")) || !bytes.Contains(eventBody, []byte(`"state":"ready"`)) {
		t.Fatalf("unexpected SSE payload: %s", eventBody)
	}

	executor.mu.Lock()
	executor.failPort = true
	executor.mu.Unlock()
	managedPort := doRequest(t, http.MethodGet, server.URL+"/api/v1/preflight?recipe_id="+recipes[1].ID, "", cookies, nil)
	var switchPreflight preflightResponse
	if err := json.NewDecoder(managedPort.Body).Decode(&switchPreflight); err != nil {
		t.Fatal(err)
	}
	managedPort.Body.Close()
	if !switchPreflight.Ready {
		t.Fatalf("managed active port should be switchable: %#v", switchPreflight)
	}
	executor.mu.Lock()
	executor.failPort = false
	executor.mu.Unlock()

	leakJob, _, err := database.CreateJob(context.Background(), "smoke-test", recipes[0].ID, "diagnostic-redaction", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	secret := "hf_SUPERSECRET1234567890"
	if err := database.UpdateJobState(context.Background(), leakJob.ID, "failed", "Authorization: Bearer "+secret); err != nil {
		t.Fatal(err)
	}
	unauthenticatedDiagnostics, err := http.Get(server.URL + "/api/v1/diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	if unauthenticatedDiagnostics.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated diagnostics status=%d", unauthenticatedDiagnostics.StatusCode)
	}
	unauthenticatedDiagnostics.Body.Close()
	diagnostics := doRequest(t, http.MethodGet, server.URL+"/api/v1/diagnostics", "", cookies, nil)
	diagnosticBody, _ := io.ReadAll(diagnostics.Body)
	diagnostics.Body.Close()
	if diagnostics.StatusCode != http.StatusOK || !strings.HasPrefix(diagnostics.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("diagnostics status=%d disposition=%q", diagnostics.StatusCode, diagnostics.Header.Get("Content-Disposition"))
	}
	if !bytes.Contains(diagnosticBody, []byte(`"format": "runonspark-diagnostics-v1"`)) || !bytes.Contains(diagnosticBody, []byte("[REDACTED]")) || bytes.Contains(diagnosticBody, []byte(secret)) {
		t.Fatalf("diagnostic export was incomplete or leaked a secret: %s", diagnosticBody)
	}

	// Storage accounting must attribute runtime images with their recipes and
	// count them toward the managed total.
	storage := doRequest(t, http.MethodGet, server.URL+"/api/v1/storage", "", cookies, nil)
	var storageInfo struct {
		TotalManaged int64 `json:"total_managed_bytes"`
		Images       []struct {
			Reference string   `json:"reference"`
			Bytes     int64    `json:"bytes"`
			RecipeIDs []string `json:"recipe_ids"`
		} `json:"images"`
	}
	if err := json.NewDecoder(storage.Body).Decode(&storageInfo); err != nil {
		t.Fatal(err)
	}
	storage.Body.Close()
	if len(storageInfo.Images) == 0 {
		t.Fatal("storage breakdown reported no runtime images")
	}
	var imageTotal int64
	attributed := map[string]bool{}
	for _, image := range storageInfo.Images {
		if image.Reference == "" || image.Bytes <= 0 || len(image.RecipeIDs) == 0 {
			t.Fatalf("incomplete image attribution: %#v", image)
		}
		imageTotal += image.Bytes
		for _, id := range image.RecipeIDs {
			attributed[id] = true
		}
	}
	for _, item := range recipes {
		if !attributed[item.ID] {
			t.Errorf("recipe %s is not attributed to any runtime image", item.ID)
		}
	}
	if storageInfo.TotalManaged < imageTotal {
		t.Fatalf("managed total %d does not include image bytes %d", storageInfo.TotalManaged, imageTotal)
	}
}

// newPairedTestServer boots a fresh manager and completes pairing, returning
// a session cookie jar and CSRF token every subsequent mutating call needs.
func newPairedTestServer(t *testing.T) (server *httptest.Server, cookies []*http.Cookie, csrf string) {
	t.Helper()
	dataDir := t.TempDir()
	database, err := store.Open(filepath.Join(dataDir, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	authManager, err := auth.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	executor := &apiExecutor{done: map[string]bool{}}
	runner := engine.New(database, executor, recipes)
	server = httptest.NewServer(New("test-version", dataDir, authManager, database, readyInventory{}, executor, runner, recipes).Handler())
	t.Cleanup(server.Close)
	tokenBytes, err := os.ReadFile(authManager.PairingTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	paired := doRequest(t, http.MethodPost, server.URL+"/api/v1/auth/pair", `{"token":"`+token+`"}`, nil, map[string]string{"Origin": server.URL})
	if paired.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(paired.Body)
		t.Fatalf("pair status=%d body=%s", paired.StatusCode, data)
	}
	var pairResult struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(paired.Body).Decode(&pairResult); err != nil {
		t.Fatal(err)
	}
	cookies = paired.Cookies()
	paired.Body.Close()
	return server, cookies, pairResult.CSRF
}

// fakePeer stands in for another Spark's manager: it serves exactly the
// three read-only endpoints a real peer would, gated behind the same Bearer
// API key scheme this manager itself uses.
func fakePeer(t *testing.T, wantKey string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+wantKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/api/v1/system", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"hostname": "peer-host", "manager_version": "9.9.9", "memory_available_bytes": 42_000_000_000,
			"installed_models": []map[string]any{{"recipe_id": "demo", "active": true, "status": "ready"}},
		})
	}))
	mux.HandleFunc("/api/v1/models", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{{"recipe_id": "demo", "active": true, "status": "ready"}})
	}))
	mux.HandleFunc("/api/v1/telemetry", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"memory_available": 42_000_000_000})
	}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestPeerLifecycleOverAPI(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	headers := map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf}

	peer := fakePeer(t, "rosk_realkey")

	// A wrong key must be caught before anything is persisted: the whole
	// point of the pre-save check is that a broken entry is never saved.
	badKey := doRequest(t, http.MethodPost, server.URL+"/api/v1/peers",
		`{"name":"edgexpert-beta","base_url":"`+peer.URL+`","api_key":"rosk_wrongkey"}`, cookies, headers)
	if badKey.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad key status=%d", badKey.StatusCode)
	}
	var badKeyBody struct {
		Error string `json:"error"`
	}
	json.NewDecoder(badKey.Body).Decode(&badKeyBody)
	badKey.Body.Close()
	if badKeyBody.Error != "could not reach that Spark with this key, so check the URL and key" {
		t.Fatalf("unexpected error message: %q", badKeyBody.Error)
	}

	// Missing CSRF must be rejected the same way every other mutation is.
	noCSRF := doRequest(t, http.MethodPost, server.URL+"/api/v1/peers",
		`{"name":"edgexpert-beta","base_url":"`+peer.URL+`","api_key":"rosk_realkey"}`, cookies, map[string]string{"Origin": server.URL})
	if noCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", noCSRF.StatusCode)
	}
	noCSRF.Body.Close()

	// A URL with a path is rejected before any network call is made.
	badURL := doRequest(t, http.MethodPost, server.URL+"/api/v1/peers",
		`{"name":"edgexpert-beta","base_url":"`+peer.URL+`/some/path","api_key":"rosk_realkey"}`, cookies, headers)
	if badURL.StatusCode != http.StatusBadRequest {
		t.Fatalf("path-bearing URL status=%d", badURL.StatusCode)
	}
	badURL.Body.Close()

	created := doRequest(t, http.MethodPost, server.URL+"/api/v1/peers",
		`{"name":"edgexpert-beta","base_url":"`+peer.URL+`","api_key":"rosk_realkey"}`, cookies, headers)
	createdBody, _ := io.ReadAll(created.Body)
	created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.StatusCode, createdBody)
	}
	if bytes.Contains(createdBody, []byte("rosk_realkey")) {
		t.Fatalf("create response leaked the API key: %s", createdBody)
	}
	var createdPeer struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
	}
	if err := json.Unmarshal(createdBody, &createdPeer); err != nil {
		t.Fatal(err)
	}
	if createdPeer.ID == "" || createdPeer.Name != "edgexpert-beta" {
		t.Fatalf("unexpected created peer: %#v", createdPeer)
	}

	list := doRequest(t, http.MethodGet, server.URL+"/api/v1/peers", "", cookies, nil)
	listBody, _ := io.ReadAll(list.Body)
	list.Body.Close()
	if bytes.Contains(listBody, []byte("rosk_realkey")) {
		t.Fatalf("peer list leaked the API key: %s", listBody)
	}
	var peerList []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
	}
	if err := json.Unmarshal(listBody, &peerList); err != nil {
		t.Fatal(err)
	}
	if len(peerList) != 1 || peerList[0].ID != createdPeer.ID {
		t.Fatalf("unexpected peer list: %s", listBody)
	}

	summary := doRequest(t, http.MethodGet, server.URL+"/api/v1/peers/"+createdPeer.ID+"/summary", "", cookies, nil)
	var summaryBody struct {
		Reachable bool           `json:"reachable"`
		System    map[string]any `json:"system"`
		Models    []any          `json:"models"`
		Telemetry map[string]any `json:"telemetry"`
	}
	if err := json.NewDecoder(summary.Body).Decode(&summaryBody); err != nil {
		t.Fatal(err)
	}
	summary.Body.Close()
	if !summaryBody.Reachable || summaryBody.System["hostname"] != "peer-host" || len(summaryBody.Models) != 1 || summaryBody.Telemetry == nil {
		t.Fatalf("unexpected reachable summary: %#v", summaryBody)
	}

	// The summary must mark the peer unreachable, not error the whole
	// response, once it stops answering.
	peer.Close()
	downSummary := doRequest(t, http.MethodGet, server.URL+"/api/v1/peers/"+createdPeer.ID+"/summary", "", cookies, nil)
	var downBody struct {
		Reachable bool `json:"reachable"`
	}
	if downSummary.StatusCode != http.StatusOK {
		t.Fatalf("down peer summary status=%d, want 200", downSummary.StatusCode)
	}
	if err := json.NewDecoder(downSummary.Body).Decode(&downBody); err != nil {
		t.Fatal(err)
	}
	downSummary.Body.Close()
	if downBody.Reachable {
		t.Fatal("summary reported a closed peer as reachable")
	}

	noCSRFDelete := doRequest(t, http.MethodDelete, server.URL+"/api/v1/peers/"+createdPeer.ID, "", cookies, map[string]string{"Origin": server.URL})
	if noCSRFDelete.StatusCode != http.StatusForbidden {
		t.Fatalf("delete without CSRF status=%d", noCSRFDelete.StatusCode)
	}
	noCSRFDelete.Body.Close()

	deleted := doRequest(t, http.MethodDelete, server.URL+"/api/v1/peers/"+createdPeer.ID, "", cookies, headers)
	if deleted.StatusCode != http.StatusOK {
		t.Fatalf("delete status=%d", deleted.StatusCode)
	}
	deleted.Body.Close()

	deletedAgain := doRequest(t, http.MethodDelete, server.URL+"/api/v1/peers/"+createdPeer.ID, "", cookies, headers)
	if deletedAgain.StatusCode != http.StatusNotFound {
		t.Fatalf("delete of already-removed peer status=%d", deletedAgain.StatusCode)
	}
	deletedAgain.Body.Close()

	afterDelete := doRequest(t, http.MethodGet, server.URL+"/api/v1/peers", "", cookies, nil)
	afterDeleteBody, _ := io.ReadAll(afterDelete.Body)
	afterDelete.Body.Close()
	if !bytes.Equal(bytes.TrimSpace(afterDeleteBody), []byte("[]")) {
		t.Fatalf("peer list after delete = %s, want []", afterDeleteBody)
	}
}

// TestPeerSideKeyAuthIsScopedToReadOnlyFleetEndpoints proves the extension
// described in the spec: an API key now authenticates GET requests to
// system, models and telemetry (so a peer manager can read them), but every
// other /api/v1 route still requires a console session, exactly as before.
func TestPeerSideKeyAuthIsScopedToReadOnlyFleetEndpoints(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	headers := map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf, "Idempotency-Key": "make-a-key"}
	created := doRequest(t, http.MethodPost, server.URL+"/api/v1/keys", `{"name":"fleet-peer"}`, cookies, headers)
	var createdKey struct {
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(created.Body).Decode(&createdKey); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	if createdKey.Secret == "" {
		t.Fatal("key creation did not return a secret")
	}
	bearer := map[string]string{"Authorization": "Bearer " + createdKey.Secret}

	for _, path := range []string{"/api/v1/system", "/api/v1/models", "/api/v1/telemetry"} {
		response := doRequest(t, http.MethodGet, server.URL+path, "", nil, bearer)
		if response.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(response.Body)
			t.Errorf("%s with API key status=%d body=%s", path, response.StatusCode, data)
		}
		response.Body.Close()
	}

	for _, path := range []string{"/api/v1/recipes", "/api/v1/jobs", "/api/v1/storage", "/api/v1/diagnostics", "/api/v1/preflight", "/api/v1/update"} {
		response := doRequest(t, http.MethodGet, server.URL+path, "", nil, bearer)
		if response.StatusCode != http.StatusUnauthorized {
			data, _ := io.ReadAll(response.Body)
			t.Errorf("%s with API key status=%d, want 401 (key auth must not extend past system/models/telemetry): body=%s", path, response.StatusCode, data)
		}
		response.Body.Close()
	}

	// The same key must never authorize a mutation anywhere.
	mutation := doRequest(t, http.MethodPost, server.URL+"/api/v1/peers", `{"name":"x","base_url":"http://x","api_key":"y"}`, nil, bearer)
	if mutation.StatusCode == http.StatusCreated {
		t.Fatal("an inference API key must not be able to create a peer")
	}
	mutation.Body.Close()
}

// TestFetchPeerJSONRejectsNonAllowlistedPath is the acceptance test for the
// proxy's allowlist: fetchPeerJSON is the only place that ever calls out to
// a peer, and it must refuse anything outside the three named endpoints
// before a request is even sent.
func TestFetchPeerJSONRejectsNonAllowlistedPath(t *testing.T) {
	called := false
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	peer := httptest.NewServer(mux)
	defer peer.Close()

	s := &Server{peerClient: &http.Client{Timeout: 3 * time.Second}}
	for _, path := range []string{"/api/v1/keys", "/api/v1/keys/abc", "/v1/chat/completions", "/api/v1/models/qwen/install", "../etc/passwd", ""} {
		_, err := s.fetchPeerJSON(context.Background(), peer.URL, "irrelevant", path)
		if err == nil {
			t.Fatalf("path %q was not rejected by the allowlist", path)
		}
		if !strings.Contains(err.Error(), "not allowlisted") {
			t.Fatalf("path %q rejected for the wrong reason: %v", path, err)
		}
	}
	if called {
		t.Fatal("a disallowed path reached the peer over the network")
	}

	// Sanity: an allowlisted path on the same client does reach the peer.
	if _, err := s.fetchPeerJSON(context.Background(), peer.URL, "irrelevant", "/api/v1/system"); err != nil {
		t.Fatalf("allowlisted path was rejected: %v", err)
	}
	if !called {
		t.Fatal("allowlisted path never reached the peer")
	}
}

func doRequest(t *testing.T, method, url, body string, cookies []*http.Cookie, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func waitAPIJob(t *testing.T, base, id string, cookies []*http.Cookie, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := doRequest(t, http.MethodGet, base+"/api/v1/jobs/"+id, "", cookies, nil)
		var job store.Job
		if err := json.NewDecoder(response.Body).Decode(&job); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if job.State == want {
			return
		}
		if job.State == "failed" {
			t.Fatalf("job failed: %s", job.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job did not reach %s", want)
}
