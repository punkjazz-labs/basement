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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/inventory"
	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
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
	// started and hold let a test look at a step while it is still running:
	// a download closes started once it has reported progress, then waits
	// for the test to close hold. Both nil means no step ever blocks.
	started chan struct{}
	hold    chan struct{}
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
	if op.Type == "download_artifact" {
		if progress != nil {
			_ = progress(map[string]any{"bytes_complete": 100, "bytes_total": 100, "percent": 100})
		}
		if a.hold != nil {
			close(a.started)
			<-a.hold
		}
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
	if !bytes.Contains(body, []byte("basement")) {
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
	denied := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+singleSpark(recipes).ID+"/install", `{"confirmed":true,"accept_licence":true}`, cookies, map[string]string{"Origin": server.URL, "Idempotency-Key": "install-one"})
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", denied.StatusCode)
	}
	denied.Body.Close()
	headers := map[string]string{"Origin": server.URL, "Idempotency-Key": "install-one", "X-CSRF-Token": pairResult.CSRF}
	installed := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+singleSpark(recipes).ID+"/install", `{"confirmed":true,"accept_licence":true}`, cookies, headers)
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
	duplicate := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+singleSpark(recipes).ID+"/install", `{"confirmed":true,"accept_licence":true}`, cookies, headers)
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
	managedPort := doRequest(t, http.MethodGet, server.URL+"/api/v1/preflight?recipe_id="+secondSingleSpark(recipes).ID, "", cookies, nil)
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

	leakJob, _, err := database.CreateJob(context.Background(), "smoke-test", singleSpark(recipes).ID, "diagnostic-redaction", map[string]any{})
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
	if !bytes.Contains(diagnosticBody, []byte(`"format": "basement-diagnostics-v1"`)) || !bytes.Contains(diagnosticBody, []byte("[REDACTED]")) || bytes.Contains(diagnosticBody, []byte(secret)) {
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

// newAPIKey pairs a fresh manager and mints one API key on it, returning the
// server, a console session and the key secret a peer manager would hold.
func newAPIKey(t *testing.T) (server *httptest.Server, cookies []*http.Cookie, csrf, secret string) {
	t.Helper()
	server, cookies, csrf = newPairedTestServer(t)
	created := doRequest(t, http.MethodPost, server.URL+"/api/v1/keys", `{"name":"fleet-peer"}`, cookies,
		map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf})
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
	return server, cookies, csrf, createdKey.Secret
}

// TestPeerSideKeyAuthIsScopedToReadOnlyFleetEndpoints proves the extension
// described in the spec: an API key now authenticates GET requests to
// system, models, telemetry and preflight (so a peer manager can read them
// and can ask whether this machine could take an install), but every other
// /api/v1 route still requires a console session, exactly as before.
func TestPeerSideKeyAuthIsScopedToReadOnlyFleetEndpoints(t *testing.T) {
	server, _, _, secret := newAPIKey(t)
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	bearer := map[string]string{"Authorization": "Bearer " + secret}

	readable := []string{"/api/v1/system", "/api/v1/models", "/api/v1/telemetry", "/api/v1/preflight?recipe_id=" + singleSpark(recipes).ID}
	for _, path := range readable {
		response := doRequest(t, http.MethodGet, server.URL+path, "", nil, bearer)
		if response.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(response.Body)
			t.Errorf("%s with API key status=%d body=%s", path, response.StatusCode, data)
		}
		response.Body.Close()
	}

	for _, path := range []string{"/api/v1/recipes", "/api/v1/jobs", "/api/v1/storage", "/api/v1/diagnostics", "/api/v1/update", "/api/v1/keys", "/api/v1/peers"} {
		response := doRequest(t, http.MethodGet, server.URL+path, "", nil, bearer)
		if response.StatusCode != http.StatusUnauthorized {
			data, _ := io.ReadAll(response.Body)
			t.Errorf("%s with API key status=%d, want 401 (key auth must not extend past the fleet read surface): body=%s", path, response.StatusCode, data)
		}
		response.Body.Close()
	}

	// The same key must never authorize a mutation outside install.
	mutation := doRequest(t, http.MethodPost, server.URL+"/api/v1/peers", `{"name":"x","base_url":"http://x","api_key":"y"}`, nil, bearer)
	if mutation.StatusCode == http.StatusCreated {
		t.Fatal("an inference API key must not be able to create a peer")
	}
	mutation.Body.Close()
}

// TestBearerKeyOnlyReachesInstallOnModelRoutes is the least-privilege pin for
// delegated placement (ADR 0013). A head Spark holding this machine's API key
// may ask it to install a model, because that is what its owner asked for on
// the other console. It may not start, stop, benchmark, smoke-test or remove
// anything, so a key that leaks never becomes control of what this Spark is
// serving right now, and it may not touch keys or peers at all.
func TestBearerKeyOnlyReachesInstallOnModelRoutes(t *testing.T) {
	server, cookies, csrf, secret := newAPIKey(t)
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	target := singleSpark(recipes).ID
	bearer := map[string]string{"Authorization": "Bearer " + secret, "Idempotency-Key": "delegated-install"}

	// Install the model with a console session first, so the lifecycle
	// subactions below fail on authentication rather than on "not installed".
	installed := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+target+"/install", `{"confirmed":true,"accept_licence":true}`,
		cookies, map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf, "Idempotency-Key": "console-install"})
	if installed.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(installed.Body)
		t.Fatalf("console install status=%d body=%s", installed.StatusCode, data)
	}
	var createdJob struct {
		Job store.Job `json:"job"`
	}
	if err := json.NewDecoder(installed.Body).Decode(&createdJob); err != nil {
		t.Fatal(err)
	}
	installed.Body.Close()
	waitAPIJob(t, server.URL, createdJob.Job.ID, cookies, "ready")

	// The one thing a key may do. No CSRF token and no Origin header are
	// sent: a bearer header is never ambient, so CSRF does not apply.
	delegated := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+target+"/install", `{"confirmed":true,"accept_licence":true}`, nil, bearer)
	body, _ := io.ReadAll(delegated.Body)
	delegated.Body.Close()
	if delegated.StatusCode != http.StatusAccepted && delegated.StatusCode != http.StatusOK {
		t.Fatalf("delegated install status=%d body=%s", delegated.StatusCode, body)
	}

	// The model subactions must be refused by withModelAuth itself, before
	// modelAction is ever entered, which is what 401 (rather than 403) pins
	// here: the wrapper is the lock, not a check further down the handler.
	// The keys and peers routes were already console-only and answer with
	// their own existing status, so they are allowed either.
	refused := []struct {
		name   string
		method string
		path   string
		body   string
		want   []int
	}{
		{"start", http.MethodPost, "/api/v1/models/" + target + "/start", "{}", []int{http.StatusUnauthorized}},
		{"stop", http.MethodPost, "/api/v1/models/" + target + "/stop", "{}", []int{http.StatusUnauthorized}},
		{"smoke-test", http.MethodPost, "/api/v1/models/" + target + "/smoke-test", "{}", []int{http.StatusUnauthorized}},
		{"benchmark", http.MethodPost, "/api/v1/models/" + target + "/benchmark", "{}", []int{http.StatusUnauthorized}},
		{"remove", http.MethodDelete, "/api/v1/models/" + target, `{"remove_artifacts":false,"expected_reclaim_bytes":0}`, []int{http.StatusUnauthorized}},
		{"install with a trailing segment", http.MethodPost, "/api/v1/models/" + target + "/install/start", "{}", []int{http.StatusUnauthorized}},
		{"create key", http.MethodPost, "/api/v1/keys", `{"name":"escalate"}`, []int{http.StatusUnauthorized, http.StatusForbidden}},
		{"list keys", http.MethodGet, "/api/v1/keys", "", []int{http.StatusUnauthorized, http.StatusForbidden}},
		{"create peer", http.MethodPost, "/api/v1/peers", `{"name":"x","base_url":"http://127.0.0.1:1","api_key":"y"}`, []int{http.StatusUnauthorized, http.StatusForbidden}},
		{"list peers", http.MethodGet, "/api/v1/peers", "", []int{http.StatusUnauthorized, http.StatusForbidden}},
		{"delete peer", http.MethodDelete, "/api/v1/peers/anything", "", []int{http.StatusUnauthorized, http.StatusForbidden}},
	}
	for _, testCase := range refused {
		t.Run(testCase.name, func(t *testing.T) {
			response := doRequest(t, testCase.method, server.URL+testCase.path, testCase.body, nil, bearer)
			data, _ := io.ReadAll(response.Body)
			response.Body.Close()
			accepted := false
			for _, status := range testCase.want {
				accepted = accepted || response.StatusCode == status
			}
			if !accepted {
				t.Fatalf("%s with API key status=%d, want one of %v: body=%s", testCase.name, response.StatusCode, testCase.want, data)
			}
		})
	}

	// A console session with no CSRF token is still refused on install: the
	// delegated path must not have become a way around CSRF for cookies.
	noCSRF := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+target+"/install", `{"confirmed":true,"accept_licence":true}`,
		cookies, map[string]string{"Origin": server.URL, "Idempotency-Key": "no-csrf"})
	noCSRF.Body.Close()
	if noCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie install without CSRF status=%d, want 403", noCSRF.StatusCode)
	}

	// A console session still drives everything, so the least-privilege rule
	// narrowed the key and not the console.
	console := map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf, "Idempotency-Key": "console-stop"}
	stopped := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+target+"/stop", "{}", cookies, console)
	stoppedBody, _ := io.ReadAll(stopped.Body)
	stopped.Body.Close()
	if stopped.StatusCode != http.StatusAccepted {
		t.Fatalf("console stop status=%d body=%s", stopped.StatusCode, stoppedBody)
	}
}

// TestDelegatedInstallActivatesOnlyWhenAskedExplicitly pins the rest of the
// least-privilege rule (ADR 0013). A bearer key may put a model on this
// Spark, but an install that says nothing about activation must not switch
// what this machine is serving, because that is exactly the authority start
// and stop deny the same key. A head console states the intent, so placing a
// model and serving it right away still works, and a console install on this
// machine keeps installing and serving in one step as it always did.
func TestDelegatedInstallActivatesOnlyWhenAskedExplicitly(t *testing.T) {
	server, cookies, csrf, secret := newAPIKey(t)
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	target := singleSpark(recipes).ID
	bearer := func(key string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + secret, "Idempotency-Key": key}
	}
	cases := []struct {
		name    string
		body    string
		cookies []*http.Cookie
		headers map[string]string
		want    bool
	}{
		{"delegated install that says nothing about activation", `{"confirmed":true,"accept_licence":true}`, nil, bearer("delegated-default"), false},
		{"delegated install that asks to serve it now", `{"confirmed":true,"accept_licence":true,"activate":true}`, nil, bearer("delegated-explicit"), true},
		{"console install that says nothing about activation", `{"confirmed":true,"accept_licence":true}`, cookies,
			map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf, "Idempotency-Key": "console-default"}, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+target+"/install", testCase.body, testCase.cookies, testCase.headers)
			data, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("install status=%d body=%s", response.StatusCode, data)
			}
			var created struct {
				Job struct {
					Payload struct {
						Confirmed bool `json:"confirmed"`
						Activate  bool `json:"activate"`
					} `json:"payload"`
				} `json:"job"`
			}
			if err := json.Unmarshal(data, &created); err != nil {
				t.Fatal(err)
			}
			if !created.Job.Payload.Confirmed {
				t.Fatalf("install job was not created from the confirmed request: %s", data)
			}
			if created.Job.Payload.Activate != testCase.want {
				t.Fatalf("job payload activate=%v want=%v: %s", created.Job.Payload.Activate, testCase.want, data)
			}
		})
	}
}

// TestDelegatedInstallRefusesDistributedRecipe pins the authoritative half of
// the single-Spark rule. The head refuses to delegate a two-Spark recipe too,
// but it decides that against its own catalogue; if the two managers are at
// different catalogue versions, the machine that would actually run the
// recipe has to refuse it itself, or skew becomes a way to smuggle a
// distributed deployment onto one Spark.
func TestDelegatedInstallRefusesDistributedRecipe(t *testing.T) {
	server, cookies, _, secret := newAPIKey(t)
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	target := distributedRecipe(recipes).ID
	if target == "" {
		t.Fatal("the builtin catalogue carries no two-Spark recipe")
	}
	response := doRequest(t, http.MethodPost, server.URL+"/api/v1/models/"+target+"/install", `{"confirmed":true,"accept_licence":true,"activate":true}`,
		nil, map[string]string{"Authorization": "Bearer " + secret, "Idempotency-Key": "delegated-two-spark"})
	var refusal struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&refusal); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("delegated two-Spark install status=%d, want 400: %q", response.StatusCode, refusal.Error)
	}
	if refusal.Error != "a two-Spark model cannot be placed from another Spark, so install it from this Spark's own console" {
		t.Fatalf("unhelpful refusal: %q", refusal.Error)
	}

	// The refusal must land before anything is written, so no job exists for
	// that recipe at all.
	jobs := doRequest(t, http.MethodGet, server.URL+"/api/v1/jobs", "", cookies, nil)
	jobsBody, _ := io.ReadAll(jobs.Body)
	jobs.Body.Close()
	if bytes.Contains(jobsBody, []byte(target)) {
		t.Fatalf("a refused delegated install still created a job: %s", jobsBody)
	}
}

// TestDelegatableRecipeReadsTheEffectiveCatalogOnly pins the head's side of
// the same rule. Delegation always starts a fresh install, which the peer
// resolves against its own effective catalogue, and an older version of an id
// can carry a different topology, so answering from this manager's version
// history would be a guess about another machine. An id this manager cannot
// install itself is refused outright instead.
func TestDelegatableRecipeReadsTheEffectiveCatalogOnly(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	current := singleSpark(recipes)
	retired := current
	retired.ID = "retired-single-spark"
	s := &Server{}
	s.SetRecipes(append(append([]recipe.Recipe{}, recipes...), retired), recipes)

	if _, err := s.delegatableRecipe(current.ID); err != nil {
		t.Fatalf("a catalogue recipe was refused: %v", err)
	}
	_, err = s.delegatableRecipe(retired.ID)
	if err == nil || !strings.Contains(err.Error(), "does not have that model in its catalogue") {
		t.Fatalf("history-only id error=%v, want the unknown-recipe refusal", err)
	}
	_, err = s.delegatableRecipe(distributedRecipe(recipes).ID)
	if err == nil || !strings.Contains(err.Error(), "runs across 2 Sparks") {
		t.Fatalf("two-Spark id error=%v, want the topology refusal", err)
	}
}

// TestModelInstallRequestClassifier pins the predicate both locks in the
// least-privilege rule are built on: withModelAuth uses it to decide whether
// a bearer key is admitted at all, and modelAction uses it again before
// acting on a request that arrived marked as delegated. Anything it
// misclassifies as an install becomes reachable with a key alone.
func TestModelInstallRequestClassifier(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/api/v1/models/qwen36-27b-nvfp4-1s/install", true},
		{http.MethodPost, "/api/v1/models/qwen36-27b-nvfp4-1s/start", false},
		{http.MethodPost, "/api/v1/models/qwen36-27b-nvfp4-1s/stop", false},
		{http.MethodPost, "/api/v1/models/qwen36-27b-nvfp4-1s/smoke-test", false},
		{http.MethodPost, "/api/v1/models/qwen36-27b-nvfp4-1s/benchmark", false},
		{http.MethodDelete, "/api/v1/models/qwen36-27b-nvfp4-1s", false},
		{http.MethodDelete, "/api/v1/models/qwen36-27b-nvfp4-1s/install", false},
		{http.MethodGet, "/api/v1/models/qwen36-27b-nvfp4-1s/install", false},
		{http.MethodPost, "/api/v1/models/qwen36-27b-nvfp4-1s/install/start", false},
		{http.MethodPost, "/api/v1/models/qwen36-27b-nvfp4-1s/install/../start", false},
		{http.MethodPost, "/api/v1/models//install", false},
		{http.MethodPost, "/api/v1/models/install", false},
		{http.MethodPost, "/api/v1/models/", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.method+" "+testCase.path, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader("{}"))
			if got := isModelInstallRequest(request); got != testCase.want {
				t.Fatalf("isModelInstallRequest=%v want=%v", got, testCase.want)
			}
		})
	}
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

// delegatePeer stands in for the manager on the receiving end of a placement.
// It answers the two endpoints a head delegates to and records exactly what
// arrived, so the tests can assert the head forwarded the caller's intent
// rather than inventing one of its own.
type delegatePeer struct {
	*httptest.Server
	mu             sync.Mutex
	authorization  string
	idempotencyKey string
	installBody    string
	installPath    string
	preflightQuery string
	installStatus  int
}

func newDelegatePeer(t *testing.T, wantKey string) *delegatePeer {
	t.Helper()
	peer := &delegatePeer{installStatus: http.StatusAccepted}
	mux := http.NewServeMux()
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			peer.mu.Lock()
			peer.authorization = r.Header.Get("Authorization")
			peer.mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer "+wantKey {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/api/v1/system", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"hostname": "peer-host"})
	}))
	mux.HandleFunc("/api/v1/preflight", authed(func(w http.ResponseWriter, r *http.Request) {
		peer.mu.Lock()
		peer.preflightQuery = r.URL.RawQuery
		peer.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"recipe_id": r.URL.Query().Get("recipe_id"), "ready": true, "licence_accepted": false})
	}))
	mux.HandleFunc("/api/v1/models/", authed(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		peer.mu.Lock()
		peer.installPath, peer.installBody = r.URL.Path, string(data)
		peer.idempotencyKey = r.Header.Get("Idempotency-Key")
		status := peer.installStatus
		peer.mu.Unlock()
		if status != http.StatusAccepted {
			writeJSON(w, status, map[string]any{"error": "preflight failed without mutating runtime state"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"job": map[string]any{"id": "peer-job-1", "state": "pending"}, "created": true})
	}))
	peer.Server = httptest.NewServer(mux)
	t.Cleanup(peer.Close)
	return peer
}

// registerPeer saves a peer on the head, which also proves it is reachable.
func registerPeer(t *testing.T, server *httptest.Server, cookies []*http.Cookie, csrf, baseURL, key string) string {
	t.Helper()
	created := doRequest(t, http.MethodPost, server.URL+"/api/v1/peers", `{"name":"edgexpert-alpha","base_url":"`+baseURL+`","api_key":"`+key+`"}`,
		cookies, map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf})
	data, _ := io.ReadAll(created.Body)
	created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("peer create status=%d body=%s", created.StatusCode, data)
	}
	var peerView struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &peerView); err != nil {
		t.Fatal(err)
	}
	return peerView.ID
}

// TestDelegatedPlacementProxy covers the head's side of ADR 0013: it is a
// remote control, so it forwards the question and relays the peer's own
// answer, status code included, without deciding anything about the peer's
// machine itself.
func TestDelegatedPlacementProxy(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	target := singleSpark(recipes).ID
	peer := newDelegatePeer(t, "rosk_peerkey")
	peerID := registerPeer(t, server, cookies, csrf, peer.URL, "rosk_peerkey")
	mutating := map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf, "Idempotency-Key": "place-on-peer"}

	// Preflight is answered by the peer about the peer.
	preflight := doRequest(t, http.MethodGet, server.URL+"/api/v1/peers/"+peerID+"/preflight?recipe_id="+target, "", cookies, nil)
	preflightBody, _ := io.ReadAll(preflight.Body)
	preflight.Body.Close()
	if preflight.StatusCode != http.StatusOK {
		t.Fatalf("peer preflight status=%d body=%s", preflight.StatusCode, preflightBody)
	}
	var preflightResult struct {
		RecipeID string `json:"recipe_id"`
		Ready    bool   `json:"ready"`
	}
	if err := json.Unmarshal(preflightBody, &preflightResult); err != nil {
		t.Fatal(err)
	}
	if preflightResult.RecipeID != target || !preflightResult.Ready {
		t.Fatalf("peer preflight was not relayed: %s", preflightBody)
	}
	peer.mu.Lock()
	gotQuery, gotAuth := peer.preflightQuery, peer.authorization
	peer.mu.Unlock()
	if gotQuery != "recipe_id="+target {
		t.Fatalf("peer saw preflight query %q", gotQuery)
	}
	if gotAuth != "Bearer rosk_peerkey" {
		t.Fatalf("peer saw authorization %q, want the stored key", gotAuth)
	}

	// Install forwards the body and the caller's idempotency key, and the
	// peer's job comes back untouched.
	installed := doRequest(t, http.MethodPost, server.URL+"/api/v1/peers/"+peerID+"/models/"+target+"/install",
		`{"confirmed":true,"accept_licence":true,"activate":false}`, cookies, mutating)
	installedBody, _ := io.ReadAll(installed.Body)
	installed.Body.Close()
	if installed.StatusCode != http.StatusAccepted {
		t.Fatalf("delegated install status=%d body=%s", installed.StatusCode, installedBody)
	}
	if !bytes.Contains(installedBody, []byte("peer-job-1")) {
		t.Fatalf("the peer's job was not relayed: %s", installedBody)
	}
	peer.mu.Lock()
	gotPath, gotBody, gotKey := peer.installPath, peer.installBody, peer.idempotencyKey
	peer.mu.Unlock()
	if gotPath != "/api/v1/models/"+target+"/install" {
		t.Fatalf("peer saw install path %q", gotPath)
	}
	if gotKey != "place-on-peer" {
		t.Fatalf("peer saw Idempotency-Key %q, want the caller's", gotKey)
	}
	var forwarded struct {
		Confirmed     bool  `json:"confirmed"`
		AcceptLicence bool  `json:"accept_licence"`
		Activate      *bool `json:"activate"`
	}
	if err := json.Unmarshal([]byte(gotBody), &forwarded); err != nil {
		t.Fatalf("peer saw an unreadable body %q: %v", gotBody, err)
	}
	if !forwarded.Confirmed || !forwarded.AcceptLicence || forwarded.Activate == nil || *forwarded.Activate {
		t.Fatalf("install intent was not forwarded faithfully: %s", gotBody)
	}

	// A refusal from the peer is the peer's to make, and reaches the console
	// with the peer's own status code.
	peer.mu.Lock()
	peer.installStatus = http.StatusConflict
	peer.mu.Unlock()
	refused := doRequest(t, http.MethodPost, server.URL+"/api/v1/peers/"+peerID+"/models/"+target+"/install",
		`{"confirmed":true,"accept_licence":true}`, cookies, mutating)
	refused.Body.Close()
	if refused.StatusCode != http.StatusConflict {
		t.Fatalf("peer refusal status=%d, want the peer's own 409", refused.StatusCode)
	}

	// Every other guard on the head side.
	distributed := distributedRecipe(recipes).ID
	unknownPeer := "peer-that-does-not-exist"
	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		headers map[string]string
		want    int
	}{
		{"two-Spark install refused", http.MethodPost, "/api/v1/peers/" + peerID + "/models/" + distributed + "/install", `{"confirmed":true}`, mutating, http.StatusBadRequest},
		{"two-Spark preflight refused", http.MethodGet, "/api/v1/peers/" + peerID + "/preflight?recipe_id=" + distributed, "", nil, http.StatusBadRequest},
		{"unknown recipe refused", http.MethodPost, "/api/v1/peers/" + peerID + "/models/not-a-recipe/install", `{"confirmed":true}`, mutating, http.StatusBadRequest},
		{"install without CSRF refused", http.MethodPost, "/api/v1/peers/" + peerID + "/models/" + target + "/install", `{"confirmed":true}`, map[string]string{"Origin": server.URL, "Idempotency-Key": "no-csrf"}, http.StatusForbidden},
		{"unknown peer", http.MethodGet, "/api/v1/peers/" + unknownPeer + "/preflight?recipe_id=" + target, "", nil, http.StatusNotFound},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := doRequest(t, testCase.method, server.URL+testCase.path, testCase.body, cookies, testCase.headers)
			data, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != testCase.want {
				t.Fatalf("status=%d want=%d body=%s", response.StatusCode, testCase.want, data)
			}
			if !bytes.Contains(data, []byte(`"error"`)) {
				t.Fatalf("refusal carried no error message: %s", data)
			}
		})
	}

	// Unauthenticated callers never reach the peer at all.
	for _, path := range []string{"/api/v1/peers/" + peerID + "/preflight?recipe_id=" + target, "/api/v1/peers/" + peerID + "/models/" + target + "/install"} {
		response := doRequest(t, http.MethodPost, server.URL+path, `{"confirmed":true}`, nil, map[string]string{"Origin": server.URL})
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s status=%d, want 401", path, response.StatusCode)
		}
	}
}

// TestDelegatedPlacementReportsUnreachablePeer pins the one answer the head
// speaks for itself: the network between the two machines.
func TestDelegatedPlacementReportsUnreachablePeer(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	target := singleSpark(recipes).ID
	peer := newDelegatePeer(t, "rosk_peerkey")
	peerID := registerPeer(t, server, cookies, csrf, peer.URL, "rosk_peerkey")
	peer.Close()

	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		headers map[string]string
	}{
		{"preflight", http.MethodGet, "/api/v1/peers/" + peerID + "/preflight?recipe_id=" + target, "", nil},
		{"install", http.MethodPost, "/api/v1/peers/" + peerID + "/models/" + target + "/install", `{"confirmed":true,"accept_licence":true}`,
			map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf, "Idempotency-Key": "unreachable"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := doRequest(t, testCase.method, server.URL+testCase.path, testCase.body, cookies, testCase.headers)
			var failure struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusBadGateway {
				t.Fatalf("status=%d, want 502", response.StatusCode)
			}
			if !strings.Contains(failure.Error, "could not reach that Spark") {
				t.Fatalf("unhelpful error for a down peer: %q", failure.Error)
			}
		})
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

func TestFetchPeerJSONRefusesRedirects(t *testing.T) {
	// A malicious "peer" must not be able to bounce this manager's
	// authenticated GET to a host the user never approved: a redirect is
	// treated as the peer's final (failing) answer, never followed.
	elsewhere := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		writeJSON(w, http.StatusOK, map[string]any{"secret": true})
	}))
	defer target.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/v1/system", http.StatusFound)
	}))
	defer peer.Close()

	s := &Server{peerClient: &http.Client{Timeout: 3 * time.Second, CheckRedirect: refusePeerRedirect}}
	_, err := s.fetchPeerJSON(context.Background(), peer.URL, "irrelevant", "/api/v1/system")
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirecting peer accepted: %v", err)
	}
	if elsewhere != 0 {
		t.Fatal("the redirect target was contacted")
	}
}

// The sampler reads each runtime's own series names. Neither SGLang nor
// llama-server publishes an equivalent of vLLM's KV cache usage gauge, so
// that field stays absent for them rather than defaulting to zero, and label
// sets must not defeat the match.
func TestRuntimeMetricsSamplerIsKindAware(t *testing.T) {
	exposition := map[string]string{
		"vllm": "# HELP vllm:num_requests_running Running.\n" +
			"vllm:num_requests_running{model_name=\"m\"} 3\n" +
			"vllm:num_requests_waiting{model_name=\"m\"} 1\n" +
			"vllm:gpu_cache_usage_perc{model_name=\"m\"} 0.5\n" +
			"vllm:generation_tokens_total{model_name=\"m\"} 120\n",
		"sglang": "# HELP sglang:num_running_reqs Running.\n" +
			"sglang:num_running_reqs{model_name=\"m\",tp_rank=\"0\"} 3\n" +
			"sglang:num_queue_reqs{model_name=\"m\",tp_rank=\"0\"} 1\n" +
			"sglang:cache_hit_rate{model_name=\"m\",tp_rank=\"0\"} 0.9\n" +
			"sglang:prompt_tokens_total{model_name=\"m\",tp_rank=\"0\"} 40\n" +
			"sglang:generation_tokens_total{model_name=\"m\",tp_rank=\"0\"} 120\n",
		// llama-server names the same quantities differently: a request in
		// flight is "processing", a queued one is "deferred", and generated
		// tokens are "predicted".
		"llamacpp": "# HELP llamacpp:requests_processing Number of requests processing.\n" +
			"llamacpp:requests_processing 3\n" +
			"llamacpp:requests_deferred 1\n" +
			"llamacpp:prompt_tokens_total 40\n" +
			"llamacpp:tokens_predicted_total 120\n",
	}
	for kind, body := range exposition {
		t.Run(kind, func(t *testing.T) {
			metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer metrics.Close()
			port, err := strconv.Atoi(metrics.URL[strings.LastIndex(metrics.URL, ":")+1:])
			if err != nil {
				t.Fatal(err)
			}
			server := &Server{metrics: metrics.Client()}
			r := recipe.Recipe{Runtime: recipe.Runtime{Kind: kind}, Service: recipe.Service{DefaultHostPort: port}}
			sample := server.runtimeMetrics(context.Background(), r)
			if sample["requests_running"] != 3 || sample["requests_waiting"] != 1 || sample["generation_tokens_total"] != 120 {
				t.Fatalf("%s sample=%#v", kind, sample)
			}
			if _, hasKV := sample["kv_cache_usage"]; hasKV != (kind == "vllm") {
				t.Fatalf("%s kv_cache_usage present=%v", kind, hasKV)
			}
		})
	}
	// A kind with no series mapping is not scraped at all. This must be a
	// kind the manager genuinely has no mapping for; using a mapped one here
	// would pass only because the fixture recipe has no port, and would go on
	// passing after the mapping was added.
	server := &Server{metrics: &http.Client{}}
	if names := runtimeMetricNames("tensorrt"); names != nil {
		t.Fatalf("this test needs an unmapped kind; tensorrt now maps to %#v", names)
	}
	if sample := server.runtimeMetrics(context.Background(), recipe.Recipe{Runtime: recipe.Runtime{Kind: "tensorrt"}}); sample != nil {
		t.Fatalf("unmapped kind sampled=%#v", sample)
	}
}

// Token usage is accumulated from readings of a counter that restarts with
// the serving container, so a restarted runtime must add its new counter
// rather than reset the model's total.
func TestTokenUsageAccumulatesAcrossARuntimeRestart(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	counters := "vllm:prompt_tokens_total{model_name=\"m\"} 500\nvllm:generation_tokens_total{model_name=\"m\"} 200\n"
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, counters)
	}))
	defer metrics.Close()
	port, err := strconv.Atoi(metrics.URL[strings.LastIndex(metrics.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{metrics: metrics.Client(), store: database}
	r := recipe.Recipe{ID: "qwen", Runtime: recipe.Runtime{Kind: "vllm"}, Service: recipe.Service{DefaultHostPort: port}}
	server.CaptureTokenUsage(ctx, r)
	counters = "vllm:prompt_tokens_total{model_name=\"m\"} 60\nvllm:generation_tokens_total{model_name=\"m\"} 20\n"
	server.CaptureTokenUsage(ctx, r)

	usage, err := database.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].PromptTokens != 560 || usage[0].GenerationTokens != 220 {
		t.Fatalf("usage=%+v", usage)
	}

	// A runtime the manager has no series mapping for leaves the model with
	// no row, rather than one claiming it served nothing. The kind here must
	// really be unmapped, or the assertion would hold for the wrong reason.
	if names := runtimeMetricNames("tensorrt"); names != nil {
		t.Fatalf("this test needs an unmapped kind; tensorrt now maps to %#v", names)
	}
	quiet := &Server{metrics: &http.Client{}, store: database}
	quiet.CaptureTokenUsage(ctx, recipe.Recipe{ID: "unmapped-model", Runtime: recipe.Runtime{Kind: "tensorrt"}})
	usage, err = database.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 {
		t.Fatalf("an unmapped runtime was counted: %+v", usage)
	}
}

// llama-server publishes its token counters under its own names, and they
// restart with the container the same way vLLM's do. This drives the whole
// token-usage path against a fake llama-server so the mapping is exercised
// rather than merely declared.
func TestTokenUsageAccumulatesFromLlamaServerCounters(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// llama-server exposes these without a model label, unlike the other two.
	counters := "# HELP llamacpp:prompt_tokens_total Number of prompt tokens processed.\n" +
		"# TYPE llamacpp:prompt_tokens_total counter\n" +
		"llamacpp:prompt_tokens_total 500\n" +
		"llamacpp:tokens_predicted_total 200\n"
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, counters)
	}))
	defer metrics.Close()
	port, err := strconv.Atoi(metrics.URL[strings.LastIndex(metrics.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{metrics: metrics.Client(), store: database}
	r := recipe.Recipe{
		ID:      "deepseek-v4-flash-0731-ud-iq3-xxs-1s",
		Runtime: recipe.Runtime{Kind: "llamacpp"},
		Service: recipe.Service{DefaultHostPort: port, ServedModelID: "unsloth/DeepSeek-V4-Flash-0731-GGUF"},
	}
	server.CaptureTokenUsage(ctx, r)
	usage, err := database.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].PromptTokens != 500 || usage[0].GenerationTokens != 200 {
		t.Fatalf("first llama-server reading=%+v", usage)
	}
	// The container restarts and the counters begin again from zero; the
	// model's running total must grow by the new reading, not reset to it.
	counters = "llamacpp:prompt_tokens_total 60\nllamacpp:tokens_predicted_total 20\n"
	server.CaptureTokenUsage(ctx, r)
	usage, err = database.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].PromptTokens != 560 || usage[0].GenerationTokens != 220 {
		t.Fatalf("usage after a llama-server restart=%+v", usage)
	}
	// And the same scrape feeds the console's live tiles, where the counters
	// llama-server does not publish stay absent instead of reading as zero.
	sample := server.runtimeMetrics(ctx, r)
	if sample["prompt_tokens_total"] != float64(60) || sample["generation_tokens_total"] != float64(20) {
		t.Fatalf("llama-server sample=%#v", sample)
	}
	if _, hasKV := sample["kv_cache_usage"]; hasKV {
		t.Fatalf("llama-server published a KV cache figure it does not have: %#v", sample)
	}
}

// The 45s ticker and the engine's pre-stop sample can both call
// CaptureTokenUsage for the same model at once. Before tokenMu, a slower
// scrape's response could be stored after a faster, later scrape's,
// which tokenDelta then reads as the runtime having restarted and
// re-adds in full. This proves the two are now serialized: the second
// scrape cannot even reach the runtime until the first's whole
// read-then-record has finished, so accumulation stays ordered and
// deterministic.
func TestCaptureTokenUsageSerializesConcurrentScrapes(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	firstArrived := make(chan struct{})
	release := make(chan struct{})
	var served int32
	var secondArrivedEarly bool

	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&served, 1) == 1 {
			close(firstArrived)
			<-release
			_, _ = io.WriteString(w, "vllm:prompt_tokens_total{model_name=\"m\"} 110\nvllm:generation_tokens_total{model_name=\"m\"} 40\n")
			return
		}
		select {
		case <-release:
		default:
			secondArrivedEarly = true
		}
		_, _ = io.WriteString(w, "vllm:prompt_tokens_total{model_name=\"m\"} 120\nvllm:generation_tokens_total{model_name=\"m\"} 45\n")
	}))
	defer metrics.Close()
	port, err := strconv.Atoi(metrics.URL[strings.LastIndex(metrics.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{metrics: metrics.Client(), store: database}
	r := recipe.Recipe{ID: "qwen", Runtime: recipe.Runtime{Kind: "vllm"}, Service: recipe.Service{DefaultHostPort: port}}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); server.CaptureTokenUsage(ctx, r) }() // the slow, "older" scrape
	<-firstArrived
	go func() { defer wg.Done(); server.CaptureTokenUsage(ctx, r) }() // the fast, "newer" scrape
	// Give the second call every chance to reach the runtime before the
	// first is released; under the fix it cannot, because CaptureTokenUsage
	// holds tokenMu across the whole read-then-record, including the HTTP
	// round trip.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if secondArrivedEarly {
		t.Fatal("the second scrape reached the runtime before the first scrape's read-then-record finished; they are not serialized")
	}
	usage, err := database.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Both readings are stored in the order they were taken (110 then 120),
	// so the total is the higher, later reading rather than 110 (a stale
	// write clobbering a newer one) or 230 (a spurious restart double-count).
	if len(usage) != 1 || usage[0].PromptTokens != 120 || usage[0].GenerationTokens != 45 {
		t.Fatalf("usage=%+v", usage)
	}
}

// CaptureFinalTokenUsage is what the engine's pre-stop hook is bound to: it
// samples exactly like CaptureTokenUsage and then, in the same locked call,
// resets the stored last-seen counters so the next container's first
// reading counts in full instead of comparing against a container that can
// never publish again.
func TestCaptureFinalTokenUsageSamplesThenResetsCounters(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	counters := "vllm:prompt_tokens_total{model_name=\"m\"} 500\nvllm:generation_tokens_total{model_name=\"m\"} 200\n"
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, counters)
	}))
	defer metrics.Close()
	port, err := strconv.Atoi(metrics.URL[strings.LastIndex(metrics.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{metrics: metrics.Client(), store: database}
	r := recipe.Recipe{ID: "qwen", Runtime: recipe.Runtime{Kind: "vllm"}, Service: recipe.Service{DefaultHostPort: port}}
	server.CaptureFinalTokenUsage(ctx, r)

	usage, err := database.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].PromptTokens != 500 || usage[0].GenerationTokens != 200 {
		t.Fatalf("the final sample was not recorded: %+v", usage)
	}

	// A fresh container for the same model starts publishing above zero
	// immediately. If the reset above had not run, this would be read as
	// only the rise past the dead container's counters (a large amount would
	// be lost); with the reset, the whole reading counts.
	counters = "vllm:prompt_tokens_total{model_name=\"m\"} 12\nvllm:generation_tokens_total{model_name=\"m\"} 5\n"
	server.CaptureTokenUsage(ctx, r)
	usage, err = database.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].PromptTokens != 512 || usage[0].GenerationTokens != 205 {
		t.Fatalf("counters were not reset before the next container's reading: %+v", usage)
	}
}

// singleSpark returns the first shipped single-Spark recipe. The pack now
// also carries distributed recipes, and these fixtures run one node.
func singleSpark(recipes []recipe.Recipe) recipe.Recipe {
	for _, r := range recipes {
		if !r.Distributed() {
			return r
		}
	}
	return recipe.Recipe{}
}

// distributedRecipe returns the first shipped two-Spark recipe, which is
// never a candidate for delegation to one machine.
func distributedRecipe(recipes []recipe.Recipe) recipe.Recipe {
	for _, r := range recipes {
		if r.Distributed() {
			return r
		}
	}
	return recipe.Recipe{}
}

// secondSingleSpark returns a shipped single-Spark recipe distinct from
// singleSpark(recipes), for switch and port-conflict fixtures.
func secondSingleSpark(recipes []recipe.Recipe) recipe.Recipe {
	count := 0
	for _, r := range recipes {
		if !r.Distributed() {
			count++
			if count == 2 {
				return r
			}
		}
	}
	return recipe.Recipe{}
}
