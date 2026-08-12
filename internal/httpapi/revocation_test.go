package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/recipefeed"
	"github.com/punkjazz-labs/basement/internal/store"
)

// revocationHarness is newPairedTestServer with the database and the server
// object kept, because a revocation arrives through the store (that is what
// feed ingest writes) and feed health is reported by the server.
type revocationHarness struct {
	server   *httptest.Server
	api      *Server
	database *store.Store
	recipes  []recipe.Recipe
	cookies  []*http.Cookie
	csrf     string
}

func newRevocationHarness(t *testing.T) *revocationHarness {
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
	api := New("test-version", dataDir, authManager, database, readyInventory{}, executor, engine.New(database, executor, recipes), recipes)
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)

	tokenBytes, err := os.ReadFile(authManager.PairingTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	paired := doRequest(t, http.MethodPost, server.URL+"/api/v1/auth/pair",
		`{"token":"`+strings.TrimSpace(string(tokenBytes))+`"}`, nil, map[string]string{"Origin": server.URL})
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
	return &revocationHarness{server: server, api: api, database: database, recipes: recipes, cookies: cookies, csrf: pairResult.CSRF}
}

func (h *revocationHarness) post(t *testing.T, path, body, idempotencyKey string) (int, string) {
	t.Helper()
	response := doRequest(t, http.MethodPost, h.server.URL+path, body, h.cookies,
		map[string]string{"Origin": h.server.URL, "X-CSRF-Token": h.csrf, "Idempotency-Key": idempotencyKey})
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response.StatusCode, string(data)
}

func (h *revocationHarness) get(t *testing.T, path string, into any) {
	t.Helper()
	response := doRequest(t, http.MethodGet, h.server.URL+path, "", h.cookies, nil)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, response.StatusCode, data)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("GET %s returned %s: %v", path, data, err)
	}
}

const revocationReason = "the published weights were the wrong quantisation and answer nonsense"

func TestRevokedVersionRefusesANewInstallAndSaysWhy(t *testing.T) {
	h := newRevocationHarness(t)
	target := singleSpark(h.recipes)
	if err := h.database.RecordRevocation(t.Context(), target.ID, target.Version, revocationReason, time.Now()); err != nil {
		t.Fatal(err)
	}
	status, body := h.post(t, "/api/v1/models/"+target.ID+"/install", `{"confirmed":true,"accept_licence":true,"activate":false}`, "revoked-install")
	if status != http.StatusConflict {
		t.Fatalf("install of a revoked version status=%d body=%s", status, body)
	}
	if !strings.Contains(body, revocationReason) {
		t.Fatalf("the refusal did not carry the publisher's reason: %s", body)
	}
	// Refused before anything was written: no job, no licence acceptance, no
	// preflight side effects.
	jobs, err := h.database.ListJobs(t.Context(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused install still created work: %#v", jobs)
	}
}

func TestRevokingOneVersionLeavesAnotherInstallable(t *testing.T) {
	// Revocation names one version. A neighbouring version of the same recipe
	// is a different statement that nobody made.
	h := newRevocationHarness(t)
	target := singleSpark(h.recipes)
	if err := h.database.RecordRevocation(t.Context(), target.ID, target.Version+1, revocationReason, time.Now()); err != nil {
		t.Fatal(err)
	}
	status, body := h.post(t, "/api/v1/models/"+target.ID+"/install", `{"confirmed":true,"accept_licence":true,"activate":false}`, "unrevoked-install")
	if status != http.StatusAccepted {
		t.Fatalf("install of an unrevoked version status=%d body=%s", status, body)
	}
}

func TestCatalogTellsTheConsoleWhichRecipeWasRevokedAndWhy(t *testing.T) {
	h := newRevocationHarness(t)
	target := singleSpark(h.recipes)
	other := secondSingleSpark(h.recipes)
	if err := h.database.RecordRevocation(t.Context(), target.ID, target.Version, revocationReason, time.Now()); err != nil {
		t.Fatal(err)
	}
	var catalog []struct {
		ID            string `json:"id"`
		Version       int    `json:"version"`
		Revoked       bool   `json:"revoked"`
		RevokedReason string `json:"revoked_reason"`
	}
	h.get(t, "/api/v1/recipes", &catalog)
	seen := false
	for _, item := range catalog {
		switch item.ID {
		case target.ID:
			seen = true
			if !item.Revoked || item.RevokedReason != revocationReason {
				t.Fatalf("the revoked recipe was not marked with its reason: %#v", item)
			}
		case other.ID:
			if item.Revoked || item.RevokedReason != "" {
				t.Fatalf("an unrelated recipe was marked revoked: %#v", item)
			}
		}
	}
	if !seen {
		// A revoked recipe stays listed on purpose: removing it would leave
		// anyone already running it with no explanation anywhere.
		t.Fatal("the revoked recipe disappeared from the catalog")
	}
}

func TestRevocationNeverTouchesAModelThatIsAlreadyServing(t *testing.T) {
	h := newRevocationHarness(t)
	target := singleSpark(h.recipes)
	serving := store.InstalledModel{
		RecipeID: target.ID, RecipeVersion: target.Version, Status: "ready",
		ArtifactPath: "/managed/" + target.ID, ContainerID: "container-1", Active: true,
	}
	if err := h.database.ActivateExclusively(t.Context(), serving); err != nil {
		t.Fatal(err)
	}
	// Exactly what feed ingest does when an index revokes the running
	// version. Nothing else happens: there is no plan, no stop, no switch.
	if err := h.database.RecordRevocation(t.Context(), target.ID, target.Version, revocationReason, time.Now()); err != nil {
		t.Fatal(err)
	}

	after, err := h.database.Model(t.Context(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Active || after.Status != "ready" || after.ContainerID != "container-1" {
		t.Fatalf("the serving model was disturbed by a revocation: %#v", after)
	}
	jobs, err := h.database.ListJobs(t.Context(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("ingesting a revocation generated work against a running model: %#v", jobs)
	}

	// The console is told, which is the whole of the effect on a running
	// model: a notice carrying the reason, and the owner decides.
	var models []struct {
		RecipeID      string `json:"recipe_id"`
		Active        bool   `json:"active"`
		Status        string `json:"status"`
		Revoked       bool   `json:"revoked"`
		RevokedReason string `json:"revoked_reason"`
	}
	h.get(t, "/api/v1/models", &models)
	if len(models) != 1 {
		t.Fatalf("unexpected models: %#v", models)
	}
	if !models[0].Active || models[0].Status != "ready" {
		t.Fatalf("the console was told the model stopped: %#v", models[0])
	}
	if !models[0].Revoked || models[0].RevokedReason != revocationReason {
		t.Fatalf("the console was not told about the revocation: %#v", models[0])
	}

	// And the owner can still operate it, revoked or not.
	if status, body := h.post(t, "/api/v1/models/"+target.ID+"/stop", "{}", "owner-stop"); status != http.StatusAccepted {
		t.Fatalf("stopping a revoked model status=%d body=%s", status, body)
	}
}

func TestSystemReportsRecipeFeedHealth(t *testing.T) {
	h := newRevocationHarness(t)
	type feedHealth struct {
		State               string     `json:"state"`
		AcceptedGeneratedAt *time.Time `json:"accepted_generated_at"`
		FetchedAt           *time.Time `json:"fetched_at"`
		Stale               bool       `json:"stale"`
	}
	var system struct {
		RecipeFeed feedHealth `json:"recipe_feed"`
	}

	// No feed wired is exactly the never_fetched case, reported rather than
	// omitted: a console that shows nothing cannot warn about anything.
	h.get(t, "/api/v1/system", &system)
	if system.RecipeFeed.State != recipefeed.StateNeverFetched {
		t.Fatalf("state=%q, want %q", system.RecipeFeed.State, recipefeed.StateNeverFetched)
	}
	if system.RecipeFeed.AcceptedGeneratedAt != nil || system.RecipeFeed.FetchedAt != nil || system.RecipeFeed.Stale {
		t.Fatalf("a feed that was never fetched reported more than it knows: %#v", system.RecipeFeed)
	}

	accepted := time.Now().UTC().Add(-31 * 24 * time.Hour).Truncate(time.Second)
	fetched := time.Now().UTC().Truncate(time.Second)
	h.api.SetRecipeFeedHealth(func() recipefeed.Health {
		return recipefeed.Health{State: recipefeed.StateUnreachable, AcceptedGeneratedAt: &accepted, FetchedAt: &fetched, Stale: true}
	})
	h.get(t, "/api/v1/system", &system)
	if system.RecipeFeed.State != recipefeed.StateUnreachable || !system.RecipeFeed.Stale {
		t.Fatalf("feed health was not passed through: %#v", system.RecipeFeed)
	}
	if system.RecipeFeed.AcceptedGeneratedAt == nil || !system.RecipeFeed.AcceptedGeneratedAt.Equal(accepted) {
		t.Fatalf("accepted_generated_at was not passed through: %#v", system.RecipeFeed)
	}
	if system.RecipeFeed.FetchedAt == nil || !system.RecipeFeed.FetchedAt.Equal(fetched) {
		t.Fatalf("fetched_at was not passed through: %#v", system.RecipeFeed)
	}
}
