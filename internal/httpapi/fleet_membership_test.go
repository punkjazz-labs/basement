package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

func TestFleetMembershipRoutesRequireOwnerSession(t *testing.T) {
	server, manager, authManager := membershipTestServer(t)
	unauthorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/fleet/join-code", nil)
	request.Header.Set("Authorization", "Bearer public-key")
	server.Handler().ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusForbidden {
		t.Fatalf("public key reached join-code route: status=%d", unauthorized.Code)
	}

	cookie, csrf := pairMembershipConsole(t, server, authManager)
	joined := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/fleet/join-code", nil)
	request.AddCookie(cookie)
	request.Header.Set("Origin", "http://console.test")
	request.Header.Set("X-CSRF-Token", csrf)
	server.Handler().ServeHTTP(joined, request)
	if joined.Code != http.StatusCreated {
		t.Fatalf("owner session could not create join code: status=%d body=%s", joined.Code, joined.Body.String())
	}
	var code fleet.JoinCode
	if err := json.NewDecoder(joined.Body).Decode(&code); err != nil || code.Code == "" {
		t.Fatalf("join code response=%+v err=%v", code, err)
	}

	summary := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "http://console.test/api/v1/fleet", nil)
	request.AddCookie(cookie)
	server.Handler().ServeHTTP(summary, request)
	if summary.Code != http.StatusOK || !strings.Contains(summary.Body.String(), manager.Identity().NodeID) {
		t.Fatalf("fleet summary status=%d body=%s", summary.Code, summary.Body.String())
	}
}

// Joining a fleet is an owner action on the browser session. A published API
// key must never reach it, and a body this manager does not understand must be
// refused before any node is contacted.
func TestFleetJoinRouteRefusesKeysAndUnreadableRequests(t *testing.T) {
	server, _, authManager := membershipTestServer(t)
	_, key, err := server.store.CreateAPIKey(context.Background(), "public client")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"display_name":"spark-worker","console_url":"http://192.168.99.20:7070","node_url":"https://192.168.99.20:7071","join_code":"v1.not-a-code"}`
	keyed := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/fleet/join", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	server.Handler().ServeHTTP(keyed, request)
	if keyed.Code != http.StatusForbidden {
		t.Fatalf("an API key reached the join route: status=%d body=%s", keyed.Code, keyed.Body.String())
	}

	cookie, csrf := pairMembershipConsole(t, server, authManager)
	ownerRequest := func(payload string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/fleet/join", strings.NewReader(payload))
		request.AddCookie(cookie)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://console.test")
		request.Header.Set("X-CSRF-Token", csrf)
		server.Handler().ServeHTTP(response, request)
		return response
	}

	malformed := ownerRequest(body)
	if malformed.Code != http.StatusConflict {
		t.Fatalf("malformed join code status=%d body=%s", malformed.Code, malformed.Body.String())
	}
	var failure struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(malformed.Body).Decode(&failure); err != nil || !strings.Contains(failure.Error, "join code") {
		t.Fatalf("join failure did not name the join code: %+v err=%v", failure, err)
	}

	unknown := ownerRequest(`{"display_name":"spark-worker","console_url":"http://192.168.99.20:7070","node_url":"https://192.168.99.20:7071","join_code":"v1.a.b","fleet_id":"fleet_other"}`)
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("an unknown request field was accepted: status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}

// Answering an invitation is an owner action on this Spark's own console: it
// is what mints a join code for the machine that asked. A published API key
// must not reach the list or either answer.
func TestFleetInvitationRoutesNeedTheOwnerSession(t *testing.T) {
	server, _, authManager := membershipTestServer(t)
	_, key, err := server.store.CreateAPIKey(context.Background(), "public client")
	if err != nil {
		t.Fatal(err)
	}
	listed := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://console.test/api/v1/fleet/invitations", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	server.Handler().ServeHTTP(listed, request)
	if listed.Code != http.StatusUnauthorized {
		t.Fatalf("an API key listed the invitations: status=%d body=%s", listed.Code, listed.Body.String())
	}
	for _, action := range []string{"approve", "deny"} {
		answered := httptest.NewRecorder()
		request = httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/fleet/invitations/inv_abc/"+action, nil)
		request.Header.Set("Authorization", "Bearer "+key)
		server.Handler().ServeHTTP(answered, request)
		if answered.Code != http.StatusForbidden {
			t.Fatalf("an API key reached %s: status=%d body=%s", action, answered.Code, answered.Body.String())
		}
	}

	cookie, csrf := pairMembershipConsole(t, server, authManager)
	owner := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "http://console.test/api/v1/fleet/invitations", nil)
	request.AddCookie(cookie)
	server.Handler().ServeHTTP(owner, request)
	if owner.Code != http.StatusOK || !strings.Contains(owner.Body.String(), `"invitations":[]`) {
		t.Fatalf("owner invitations status=%d body=%s", owner.Code, owner.Body.String())
	}
	missing := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/fleet/invitations/inv_abc/approve", nil)
	request.AddCookie(cookie)
	request.Header.Set("Origin", "http://console.test")
	request.Header.Set("X-CSRF-Token", csrf)
	server.Handler().ServeHTTP(missing, request)
	if missing.Code != http.StatusConflict {
		t.Fatalf("approving an invitation nobody sent status=%d body=%s", missing.Code, missing.Body.String())
	}
}

// Adding a Spark spends the owner's authority over their own machines, and the
// address it is spent on has to be one this manager can actually address.
func TestControllerInviteRoutesNeedTheOwnerSessionAndAnAddressableConsole(t *testing.T) {
	server, manager, authManager := membershipTestServer(t)
	_, key, err := server.store.CreateAPIKey(context.Background(), "public client")
	if err != nil {
		t.Fatal(err)
	}
	keyed := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/fleet/invite", strings.NewReader(`{"console_url":"http://192.168.99.20:7070"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	server.Handler().ServeHTTP(keyed, request)
	if keyed.Code != http.StatusForbidden {
		t.Fatalf("an API key asked another Spark to join: status=%d body=%s", keyed.Code, keyed.Body.String())
	}
	anonymous := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "http://console.test/api/v1/fleet/invite/status?console_url=http://192.168.99.20:7070", nil)
	server.Handler().ServeHTTP(anonymous, request)
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("an anonymous caller read an addition: status=%d body=%s", anonymous.Code, anonymous.Body.String())
	}

	cookie, csrf := pairMembershipConsole(t, server, authManager)
	invite := func(payload string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/fleet/invite", strings.NewReader(payload))
		request.AddCookie(cookie)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://console.test")
		request.Header.Set("X-CSRF-Token", csrf)
		server.Handler().ServeHTTP(response, request)
		return response
	}
	for _, payload := range []string{`{"console_url":"not a url"}`, `{"console_url":"http://192.168.99.20"}`, `{"console_url":""}`} {
		malformed := invite(payload)
		if malformed.Code != http.StatusBadRequest {
			t.Fatalf("%s was accepted as an address: status=%d body=%s", payload, malformed.Code, malformed.Body.String())
		}
	}
	if unknown := invite(`{"console_url":"http://192.168.99.20:7070","fleet_id":"fleet_other"}`); unknown.Code != http.StatusBadRequest {
		t.Fatalf("an unknown request field was accepted: status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	// This manager's own console is not another Spark, and refusing it costs
	// no network call.
	itself := invite(`{"console_url":"` + membershipConsoleURL + `"}`)
	if itself.Code != http.StatusConflict || !strings.Contains(itself.Body.String(), "the Spark you are using") {
		t.Fatalf("this console invited itself: status=%d body=%s", itself.Code, itself.Body.String())
	}

	missing := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "http://console.test/api/v1/fleet/invite/status", nil)
	request.AddCookie(cookie)
	server.Handler().ServeHTTP(missing, request)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("a status poll without an address: status=%d body=%s", missing.Code, missing.Body.String())
	}
	unwatched := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "http://console.test/api/v1/fleet/invite/status?console_url=http://192.168.99.20:7070", nil)
	request.AddCookie(cookie)
	server.Handler().ServeHTTP(unwatched, request)
	if unwatched.Code != http.StatusNotFound {
		t.Fatalf("a machine this Spark is not adding: status=%d body=%s", unwatched.Code, unwatched.Body.String())
	}
	if manager.Identity().NodeID == "" {
		t.Fatal("the fleet manager has no identity")
	}
}

func TestPublicMuxCannotReachFleetTransport(t *testing.T) {
	server, _, _ := membershipTestServer(t)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://console.test/internal/fleet/v1/heartbeat", nil)
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("public mux exposed fleet transport: status=%d", response.Code)
	}
}

// membershipConsoleURL is where the manager under test believes its own
// console lives, which is the one address it must refuse to add.
const membershipConsoleURL = "http://192.168.99.10:7070"

func membershipTestServer(t *testing.T) (*Server, *fleet.Manager, *auth.Manager) {
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
	server := New("test", dataDir, authManager, database, readyInventory{}, executor, engine.New(database, executor, recipes), recipes)
	t.Cleanup(server.Close)
	manager, err := fleet.NewManager(context.Background(), fleet.Options{
		DataDir: dataDir, Database: database, Inventory: readyInventory{}, Version: "test", BuildIdentity: "test-build",
		DisplayName: "spark-head", ConsoleURL: membershipConsoleURL, NodeURL: "https://192.168.99.10:7071", Recipes: recipes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.SetFleetManager(manager)
	return server, manager, authManager
}

func pairMembershipConsole(t *testing.T, server *Server, authManager *auth.Manager) (*http.Cookie, string) {
	t.Helper()
	payload, err := os.ReadFile(authManager.PairingTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": strings.TrimSpace(string(payload))})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/auth/pair", bytes.NewReader(body))
	request.Header.Set("Origin", "http://console.test")
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("pair status=%d body=%s", response.Code, response.Body.String())
	}
	var paired struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&paired); err != nil {
		t.Fatal(err)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || paired.CSRF == "" {
		t.Fatal("pairing did not issue owner session authority")
	}
	return cookies[0], paired.CSRF
}
