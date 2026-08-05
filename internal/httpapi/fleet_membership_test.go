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

func TestPublicMuxCannotReachFleetTransport(t *testing.T) {
	server, _, _ := membershipTestServer(t)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://console.test/internal/fleet/v1/heartbeat", nil)
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("public mux exposed fleet transport: status=%d", response.Code)
	}
}

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
	manager, err := fleet.NewManager(context.Background(), fleet.Options{
		DataDir: dataDir, Database: database, Inventory: readyInventory{}, Version: "test", BuildIdentity: "test-build",
		DisplayName: "spark-head", ConsoleURL: "http://192.168.99.10:7070", NodeURL: "https://192.168.99.10:7071", Recipes: recipes,
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
