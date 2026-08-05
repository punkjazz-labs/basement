package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

func TestPublicAPIKeyCannotPlanOrCreateFleetDeployment(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authManager, err := auth.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	executor := &apiExecutor{done: map[string]bool{}}
	runner := engine.New(database, executor, recipes)
	server := New("test", directory, authManager, database, readyInventory{}, executor, runner, recipes)
	manager, err := fleet.NewManager(ctx, fleet.Options{
		DataDir: directory, Database: database, Inventory: readyInventory{}, Version: "test", BuildIdentity: "test-build",
		DisplayName: "node-local", ConsoleURL: "http://192.168.99.10:7070", NodeURL: "https://192.168.99.10:7071",
		Recipes: recipes, EffectiveRecipes: recipes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.SetFleetManager(manager)
	_, key, err := database.CreateAPIKey(ctx, "public client")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		body string
		want int
	}{
		{path: "/api/v1/fleet/placements/plan", body: `{"recipe_id":"anything"}`, want: http.StatusUnauthorized},
		{path: "/api/v1/fleet/deployments", body: `{"recipe_id":"anything","node_id":"anything","confirmed":true}`, want: http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodPost, "http://manager.test"+test.path, bytes.NewBufferString(test.body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+key)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("%s status=%d body=%s", test.path, response.Code, response.Body.String())
		}
	}
}
