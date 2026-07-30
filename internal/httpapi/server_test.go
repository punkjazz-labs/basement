package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
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
	mu      sync.Mutex
	done    map[string]bool
	running bool
}

func (a *apiExecutor) ArtifactPath(r recipe.Recipe) string { return "/managed/" + r.ID }
func (a *apiExecutor) Execute(_ context.Context, _ operations.Execution, op recipe.Operation, _ recipe.Recipe, progress operations.Progress) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
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
	server := httptest.NewServer(New("test-version", authManager, database, readyInventory{}, executor, runner, recipes).Handler())
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
