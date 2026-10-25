package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/punkjazz-labs/basement/internal/attached"
	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

// newAttachedTestServer is newPairedTestServer with the Server and its data
// directory still in hand, so a test can give it attached-service
// declarations before it asks the API about them.
func newAttachedTestServer(t *testing.T) (*httptest.Server, *Server, string, []*http.Cookie, string) {
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
	token, err := os.ReadFile(authManager.PairingTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	paired := doRequest(t, http.MethodPost, server.URL+"/api/v1/auth/pair", `{"token":"`+strings.TrimSpace(string(token))+`"}`, nil, map[string]string{"Origin": server.URL})
	defer paired.Body.Close()
	var result struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(paired.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return server, api, dataDir, paired.Cookies(), result.CSRF
}

type attachedListing struct {
	Services []struct {
		File        string             `json:"file"`
		Declaration attached.Service   `json:"declaration"`
		Health      attached.Health    `json:"health"`
		Nodes       []attachedNodeView `json:"nodes"`
	} `json:"services"`
	Errors []attached.Problem `json:"errors"`
}

func readAttached(t *testing.T, url string, cookies []*http.Cookie) (int, attachedListing) {
	t.Helper()
	response := doRequest(t, http.MethodGet, url+"/api/v1/attached-services", "", cookies, nil)
	defer response.Body.Close()
	var listing attachedListing
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode, listing
}

func TestAttachedServicesNeedAConsoleSession(t *testing.T) {
	server, _, _, _, _ := newAttachedTestServer(t)
	if status, _ := readAttached(t, server.URL, nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous read status = %d", status)
	}
	response := doRequest(t, http.MethodGet, server.URL+"/api/v1/attached-services", "", nil, map[string]string{"Authorization": "Bearer rosk_not_a_session"})
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a bearer key must not read attached services, status = %d", response.StatusCode)
	}
}

func TestAttachedServicesListIsEmptyWithoutDeclarations(t *testing.T) {
	server, api, _, cookies, _ := newAttachedTestServer(t)
	status, listing := readAttached(t, server.URL, cookies)
	if status != http.StatusOK || listing.Services == nil || listing.Errors == nil || len(listing.Services)+len(listing.Errors) != 0 {
		t.Fatalf("status=%d listing=%+v", status, listing)
	}
	api.SetAttachedServices(attached.NewRegistry(t.TempDir(), nil))
	if status, listing := readAttached(t, server.URL, cookies); status != http.StatusOK || len(listing.Services)+len(listing.Errors) != 0 {
		t.Fatalf("a missing directory lists nothing: status=%d listing=%+v", status, listing)
	}
	post := doRequest(t, http.MethodPost, server.URL+"/api/v1/attached-services", "{}", cookies, nil)
	post.Body.Close()
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("the list is read-only, POST status = %d", post.StatusCode)
	}
}

func TestAttachedServicesReportDeclarationHealthAndProtection(t *testing.T) {
	var healthCalls, otherCalls atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			healthCalls.Add(1)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		otherCalls.Add(1)
	}))
	defer service.Close()

	server, api, dataDir, cookies, csrf := newAttachedTestServer(t)
	peer := newDelegatePeer(t, "rosk_peerkey")
	peerID := registerPeer(t, server, cookies, csrf, peer.URL, "rosk_peerkey")

	registry := attached.NewRegistry(dataDir, nil)
	if err := os.Mkdir(registry.Directory(), 0o750); err != nil {
		t.Fatal(err)
	}
	declaration := "schema_version: 1\nid: video-pair\ndisplay_name: Video pair\nkind: video-generation\n" +
		"managed_by: \"systemd: video.service\"\nendpoint: \"http://127.0.0.1:8790 (head)\"\nhealth_url: " + service.URL + "/health\n" +
		"topology:\n  spark_count: 2\n  parallelism: TP1 x SP2 pair\n  nodes:\n" +
		"    - node_name: EDGEXPERT-ALPHA.local\n      role: head/rank0\n    - node_name: spark-ghost\n      role: worker/rank1\n" +
		"licence: Example Licence\nlicence_territory_exclusions:\n  - Somewhere\n"
	if err := os.WriteFile(filepath.Join(registry.Directory(), "video-pair.yaml"), []byte(declaration), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry.Directory(), "broken.yaml"), []byte("id: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry.Reload()
	api.SetAttachedServices(registry)

	status, listing := readAttached(t, server.URL, cookies)
	if status != http.StatusOK || len(listing.Services) != 1 || len(listing.Errors) != 1 {
		t.Fatalf("status=%d listing=%+v", status, listing)
	}
	if listing.Errors[0].File != "broken.yaml" || !strings.Contains(listing.Errors[0].Error, "not a valid declaration") {
		t.Fatalf("errors = %+v", listing.Errors)
	}
	entry := listing.Services[0]
	if entry.File != "video-pair.yaml" || entry.Declaration.DisplayName != "Video pair" || entry.Declaration.ManagedBy != "systemd: video.service" {
		t.Fatalf("declaration = %+v", entry)
	}
	if entry.Health.Status != attached.StatusUnknown {
		t.Fatalf("nothing has been checked yet, health = %+v", entry.Health)
	}
	// The paired Spark is one this manager places models on; the ghost is a
	// name it cannot match, and the console has to be able to say so.
	if len(entry.Nodes) != 2 || !entry.Nodes[0].Protected || entry.Nodes[0].ThisSpark || entry.Nodes[0].Role != "head/rank0" ||
		entry.Nodes[1].Protected || entry.Nodes[1].Name != "spark-ghost" {
		t.Fatalf("nodes = %+v", entry.Nodes)
	}

	registry.CheckNow(context.Background())
	_, listing = readAttached(t, server.URL, cookies)
	if health := listing.Services[0].Health; health.Status != attached.StatusHealthy || health.LastCheckedAt == "" {
		t.Fatalf("health after a check = %+v", health)
	}
	readAttached(t, server.URL, cookies)
	if healthCalls.Load() != 1 || otherCalls.Load() != 0 {
		t.Fatalf("reading the list must not call the service: health=%d other=%d", healthCalls.Load(), otherCalls.Load())
	}

	// A delegated install onto the held peer is refused here, before the peer
	// is asked anything.
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	install := doRequest(t, http.MethodPost, server.URL+"/api/v1/peers/"+peerID+"/models/"+singleSpark(recipes).ID+"/install",
		`{"confirmed":true,"activate":true}`, cookies, map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf, "Idempotency-Key": "onto-held-peer"})
	body, _ := io.ReadAll(install.Body)
	install.Body.Close()
	if install.StatusCode != http.StatusConflict || !strings.Contains(string(body), "the attached service Video pair has edgexpert-alpha reserved outside Basement") {
		t.Fatalf("delegated install onto a held peer: status=%d body=%s", install.StatusCode, body)
	}
	peer.mu.Lock()
	installed := peer.installPath
	peer.mu.Unlock()
	if installed != "" {
		t.Fatalf("the held peer was asked to install: %s", installed)
	}
}
