package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/punkjazz-labs/runonspark-manager/internal/auth"
	"github.com/punkjazz-labs/runonspark-manager/internal/engine"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
	"github.com/punkjazz-labs/runonspark-manager/internal/store"
)

// twoSparkRecipe is a shipped single-Spark recipe given the interconnect a
// two-Spark recipe must carry. No two-Spark recipe ships yet.
func twoSparkRecipe(t *testing.T) recipe.Recipe {
	t.Helper()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("base recipe missing")
	}
	r.Topology = recipe.Topology{SparkCount: 2, Interconnect: &recipe.Interconnect{
		Kind:       "connectx7",
		MasterPort: 29501,
		SharedEnvironment: map[string]string{
			"NCCL_IB_HCA": "rocep1s0f0", "NCCL_IB_GID_INDEX": "3",
			"NCCL_SOCKET_IFNAME": "enp1s0f0np0", "GLOO_SOCKET_IFNAME": "enp1s0f0np0",
		},
	}}
	r.Service.VLLM.TensorParallelSize = 2
	if err := recipe.Validate(r); err != nil {
		t.Fatalf("fixture is not a valid two-Spark recipe: %v", err)
	}
	return r
}

func TestWorkerNodeEndpointsAreKeyOnlyAndAllowlisted(t *testing.T) {
	ctx := context.Background()
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
	_, secret, err := database.CreateAPIKey(ctx, "other-spark")
	if err != nil {
		t.Fatal(err)
	}
	distributed := twoSparkRecipe(t)

	post := func(t *testing.T, path, key string, body any) (int, map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		return response.StatusCode, decoded
	}

	if status, _ := post(t, "/api/v1/internal/node/step", "", map[string]any{}); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated worker step status=%d", status)
	}
	if status, _ := post(t, "/api/v1/internal/node/step", "not-a-key", map[string]any{}); status != http.StatusUnauthorized {
		t.Fatalf("bad-key worker step status=%d", status)
	}

	status, body := post(t, "/api/v1/internal/node/preflight", secret, map[string]any{"recipe": distributed})
	if status != http.StatusOK || body["ready"] != true {
		t.Fatalf("worker preflight status=%d body=%#v", status, body)
	}
	// A worker rank publishes no HTTP port, so that check must not be run.
	for _, check := range body["checks"].([]any) {
		if check.(map[string]any)["operation"] == "verify_port" {
			t.Fatal("worker preflight checked the head's host port")
		}
	}

	status, body = post(t, "/api/v1/internal/node/step", secret, map[string]any{
		"operation": "pull_image", "recipe": distributed,
		"placement": map[string]any{"role": "worker", "node": "spark-b", "node_count": 2},
	})
	if status != http.StatusOK {
		t.Fatalf("worker step status=%d body=%#v", status, body)
	}
	if receipt, ok := body["receipt"].(map[string]any); !ok || receipt["operation"] != "pull_image" {
		t.Fatalf("worker step receipt=%#v", body["receipt"])
	}

	// Policy steps stay local to each manager.
	status, body = post(t, "/api/v1/internal/node/step", secret, map[string]any{
		"operation": "verify_openai_inference", "recipe": distributed,
		"placement": map[string]any{"role": "worker", "node": "spark-b", "node_count": 2},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("off-allowlist worker step status=%d body=%#v", status, body)
	}

	// A head that forgets it is talking to a worker gets nothing.
	status, _ = post(t, "/api/v1/internal/node/step", secret, map[string]any{
		"operation": "pull_image", "recipe": distributed,
		"placement": map[string]any{"role": "head", "node": "spark-a", "node_count": 2},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("head-role worker step status=%d", status)
	}

	// Single-Spark work is never delegated, and an invalid recipe is refused
	// on this node's own rules rather than on the caller's word.
	single, _ := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	status, _ = post(t, "/api/v1/internal/node/step", secret, map[string]any{
		"operation": "pull_image", "recipe": single,
		"placement": map[string]any{"role": "worker", "node": "spark-b", "node_count": 2},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("single-Spark delegation status=%d", status)
	}
	tampered := distributed
	tampered.Runtime.Digest = "sha256:nope"
	status, _ = post(t, "/api/v1/internal/node/preflight", secret, map[string]any{"recipe": tampered})
	if status != http.StatusBadRequest {
		t.Fatalf("unpinned recipe accepted from a peer, status=%d", status)
	}
}
