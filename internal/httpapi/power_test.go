package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/power"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
	_ "modernc.org/sqlite"
)

type powerHarness struct {
	server   *httptest.Server
	api      *Server
	manager  *fleet.Manager
	database *store.Store
	path     string
	cookies  []*http.Cookie
	csrf     string
	mu       sync.Mutex
	calls    [][]string
}

func newPowerHarness(t *testing.T) *powerHarness {
	t.Helper()
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "manager.db")
	database, err := store.Open(path)
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
	api := New("test-version", dataDir, authManager, database, readyInventory{}, executor, engine.New(database, executor, recipes), recipes)
	t.Cleanup(api.Close)
	harness := &powerHarness{api: api, database: database, path: path}
	controller := power.NewController(database, harness.run)
	api.SetPowerController(controller)
	// Opening a fleet manager is also what writes this machine's own fleet
	// row, which the member join below moves into a fleet.
	manager, err := fleet.NewManager(context.Background(), fleet.Options{
		DataDir: dataDir, Database: database, Inventory: readyInventory{}, Version: "test-version", BuildIdentity: "test-build",
		DisplayName: "node-power", ConsoleURL: "http://192.168.99.20:7070", NodeURL: "https://192.168.99.20:7071",
		Recipes: recipes, EffectiveRecipes: recipes,
	})
	if err != nil {
		t.Fatal(err)
	}
	api.SetFleetManager(manager)
	manager.SetPowerRuntime(controller)
	harness.manager = manager
	harness.server = httptest.NewServer(api.Handler())
	t.Cleanup(harness.server.Close)

	token, err := os.ReadFile(authManager.PairingTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	paired := doRequest(t, http.MethodPost, harness.server.URL+"/api/v1/auth/pair",
		`{"token":"`+strings.TrimSpace(string(token))+`"}`, nil, map[string]string{"Origin": harness.server.URL})
	if paired.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(paired.Body)
		t.Fatalf("pair status=%d body=%s", paired.StatusCode, body)
	}
	var result struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(paired.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	harness.cookies = paired.Cookies()
	paired.Body.Close()
	harness.csrf = result.CSRF
	return harness
}

func (h *powerHarness) run(_ context.Context, args ...string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, append([]string(nil), args...))
	return nil
}

// coolCommand is the command line the applier builds for the cap on the
// machine these tests run on. It comes from the applier rather than being
// written out here: whether the command goes through sudo depends on who this
// process is, and that question belongs to internal/power, not to these doors.
func coolCommand(t *testing.T) string {
	t.Helper()
	line, err := power.CommandLine(store.PowerModeCool)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(line, " ")
}

func (h *powerHarness) commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]string, 0, len(h.calls))
	for _, call := range h.calls {
		result = append(result, strings.Join(call, " "))
	}
	return result
}

func (h *powerHarness) post(t *testing.T, path, body string, headers map[string]string) (int, string) {
	t.Helper()
	full := map[string]string{"Origin": h.server.URL, "X-CSRF-Token": h.csrf, "Idempotency-Key": "power-test"}
	for key, value := range headers {
		if value == "" {
			delete(full, key)
			continue
		}
		full[key] = value
	}
	response := doRequest(t, http.MethodPost, h.server.URL+path, body, h.cookies, full)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response.StatusCode, string(data)
}

// The whole local door: nobody unauthenticated may read or move the switch,
// and a console session that moves it gets the new state and a GPU that was
// actually asked.
func TestPowerModeAPIReadsAndSetsTheModeForTheConsoleOnly(t *testing.T) {
	h := newPowerHarness(t)

	anonymous := doRequest(t, http.MethodGet, h.server.URL+"/api/v1/system/power-mode", "", nil, nil)
	anonymous.Body.Close()
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated read status=%d, want 401", anonymous.StatusCode)
	}
	noSession := doRequest(t, http.MethodPost, h.server.URL+"/api/v1/system/power-mode", `{"mode":"cool"}`, nil,
		map[string]string{"Origin": h.server.URL, "Idempotency-Key": "power-anonymous"})
	noSession.Body.Close()
	if noSession.StatusCode != http.StatusForbidden {
		t.Fatalf("an unauthenticated write status=%d, want 403", noSession.StatusCode)
	}
	if commands := h.commands(); len(commands) != 0 {
		t.Fatalf("a refused request reached the GPU: %v", commands)
	}

	var initial store.PowerMode
	read := doRequest(t, http.MethodGet, h.server.URL+"/api/v1/system/power-mode", "", h.cookies, nil)
	if err := json.NewDecoder(read.Body).Decode(&initial); err != nil {
		t.Fatal(err)
	}
	read.Body.Close()
	if initial.Mode != store.PowerModeFull || initial.Failure != "" {
		t.Fatalf("a fresh Spark reads %+v", initial)
	}

	status, body := h.post(t, "/api/v1/system/power-mode", `{"mode":"cool"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("setting cool status=%d body=%s", status, body)
	}
	var applied store.PowerMode
	if err := json.Unmarshal([]byte(body), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Mode != store.PowerModeCool || applied.Failure != "" {
		t.Fatalf("the answer reads %+v", applied)
	}
	if commands := h.commands(); len(commands) != 1 || commands[0] != coolCommand(t) {
		t.Fatalf("the GPU was asked for %v", commands)
	}

	stored, err := h.database.PowerMode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stored.Mode != store.PowerModeCool {
		t.Fatalf("the store holds %+v after the change", stored)
	}

	// A word this product does not know is refused as input, not as a machine
	// failure, and nothing runs.
	if status, body := h.post(t, "/api/v1/system/power-mode", `{"mode":"silent"}`, nil); status != http.StatusBadRequest {
		t.Fatalf("an unknown mode status=%d body=%s, want 400", status, body)
	}
	// Every mutation on this console carries an idempotency key, and this one
	// is held to the same rule as the rest.
	if status, body := h.post(t, "/api/v1/system/power-mode", `{"mode":"full"}`, map[string]string{"Idempotency-Key": ""}); status != http.StatusBadRequest {
		t.Fatalf("a write with no idempotency key status=%d body=%s, want 400", status, body)
	}
	if commands := h.commands(); len(commands) != 1 {
		t.Fatalf("a refused request reached the GPU: %v", commands)
	}
}

// A Spark that answers to a controller answers to it for this too, in exactly
// the words it uses for a model change.
func TestPowerModeIsRefusedOnAManagedMember(t *testing.T) {
	h := newPowerHarness(t)
	joinFleetAsMember(t, h.database, "http://192.168.99.10:7070")

	status, body := h.post(t, "/api/v1/system/power-mode", `{"mode":"cool"}`, nil)
	if status != http.StatusConflict {
		t.Fatalf("a member set its own power mode: status=%d body=%s", status, body)
	}
	var refusal struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Error != "this node is managed by the fleet controller at http://192.168.99.10:7070; use that dashboard for model changes" {
		t.Fatalf("the refusal says something else: %q", refusal.Error)
	}
	if commands := h.commands(); len(commands) != 0 {
		t.Fatalf("a refused member request reached the GPU: %v", commands)
	}
	// The reading side is untouched: a member console still shows what its
	// own machine is set to.
	read := doRequest(t, http.MethodGet, h.server.URL+"/api/v1/system/power-mode", "", h.cookies, nil)
	read.Body.Close()
	if read.StatusCode != http.StatusOK {
		t.Fatalf("a member could not read its own power mode: status=%d", read.StatusCode)
	}
}

// The same door as every other mutation, failing in the same direction. A
// fleet role this manager cannot read is not evidence that the machine is free
// to change, so nothing runs and the answer claims no controller.
func TestPowerModeRefusalFailsClosedOnAStoreError(t *testing.T) {
	h := newPowerHarness(t)
	ctx := context.Background()

	raw, err := sql.Open("sqlite", h.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DROP TABLE fleet_config`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.database.FleetConfig(ctx); err == nil {
		t.Fatal("the fleet role is still readable, so this test proves nothing")
	}

	status, body := h.post(t, "/api/v1/system/power-mode", `{"mode":"cool"}`, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("an unreadable fleet role status=%d body=%s, want 500", status, body)
	}
	var refusal struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Error != "this node cannot read its own fleet role now, so it changed nothing" {
		t.Fatalf("the refusal says something else: %q", refusal.Error)
	}
	if strings.Contains(refusal.Error, "managed by") {
		t.Fatalf("the refusal claims a controller nobody read: %q", refusal.Error)
	}
	if commands := h.commands(); len(commands) != 0 {
		t.Fatalf("a request refused by a broken store reached the GPU: %v", commands)
	}
}

// The fleet door answers for the node the console named. On this Spark's own
// node id it is the local runtime, which is what a standalone console and a
// controller acting on itself both use.
func TestFleetPowerModeSetsTheNamedSpark(t *testing.T) {
	h := newPowerHarness(t)
	self := h.manager.Identity().NodeID

	status, body := h.post(t, "/api/v1/fleet/power-mode", `{"node_id":"`+self+`","mode":"cool"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("setting the local node status=%d body=%s", status, body)
	}
	var answer struct {
		NodeID  string `json:"node_id"`
		Mode    string `json:"mode"`
		Failure string `json:"failure"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.NodeID != self || answer.Mode != store.PowerModeCool || answer.Failure != "" {
		t.Fatalf("the fleet answer reads %+v", answer)
	}
	if commands := h.commands(); len(commands) != 1 || commands[0] != coolCommand(t) {
		t.Fatalf("the GPU was asked for %v", commands)
	}

	// A Spark this fleet does not hold is named as such, with no command run.
	if status, body := h.post(t, "/api/v1/fleet/power-mode", `{"node_id":"node_absent","mode":"cool"}`, nil); status != http.StatusConflict {
		t.Fatalf("an unknown node status=%d body=%s, want 409", status, body)
	}
	if status, body := h.post(t, "/api/v1/fleet/power-mode", `{"node_id":"`+self+`","mode":"loud"}`, nil); status != http.StatusBadRequest {
		t.Fatalf("an unknown mode status=%d body=%s, want 400", status, body)
	}
	if commands := h.commands(); len(commands) != 1 {
		t.Fatalf("a refused fleet request reached the GPU: %v", commands)
	}
}

// A manager that was never given a power boundary says so, rather than
// answering as though it had changed something.
func TestPowerModeSaysSoWhenThisManagerHasNoController(t *testing.T) {
	h := newPowerHarness(t)
	h.api.SetPowerController(nil)
	read := doRequest(t, http.MethodGet, h.server.URL+"/api/v1/system/power-mode", "", h.cookies, nil)
	read.Body.Close()
	if read.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a manager with no power controller answered %d, want 503", read.StatusCode)
	}
}
