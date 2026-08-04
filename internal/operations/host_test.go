package operations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/inventory"
	"github.com/punkjazz-labs/basement/internal/recipe"
)

type resourceInventory struct{ system inventory.System }

func (r resourceInventory) Inspect(context.Context) (inventory.System, error) { return r.system, nil }

func TestHostMemoryGuardChecksCapacityAndLiveHeadroom(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r := recipes[0]
	provider := resourceInventory{system: inventory.System{
		Hostname: "spark-low", MemoryTotal: 128_000_000_000, MemoryAvailable: 100_000_000_000,
		GPUMemoryTotal: 128_000_000_000, GPUMemoryFree: 90_000_000_000,
	}}
	executor := &HostExecutor{inventory: provider}
	if _, err := executor.verifyMemory(context.Background(), r, false); err != nil {
		t.Fatalf("static capacity should pass: %v", err)
	}
	if _, err := executor.verifyMemory(context.Background(), r, true); err == nil || !strings.Contains(err.Error(), "KV cache") {
		t.Fatalf("live OOM risk passed: %v", err)
	}
}

// The guardrail reads mem_fraction_static for SGLang the way it reads
// gpu_memory_utilization for vLLM, and the receipt keeps the shared field
// names so a reader does not need to know the kind to read it.
func TestHostMemoryGuardIsKindAware(t *testing.T) {
	r := sglangRecipe()
	r.Topology.SparkCount = 1
	r.Requirements.MinimumMemoryBytes = 120_000_000_000
	r.Requirements.MemoryReserveBytes = 16_000_000_000
	provider := resourceInventory{system: inventory.System{
		Hostname: "spark-sglang", MemoryTotal: 128_000_000_000, MemoryAvailable: 120_000_000_000,
		GPUMemoryTotal: 128_000_000_000, GPUMemoryFree: 120_000_000_000,
	}}
	executor := &HostExecutor{inventory: provider}
	receipt, err := executor.verifyMemory(context.Background(), r, false)
	if err != nil {
		t.Fatalf("static capacity should pass: %v", err)
	}
	if receipt["runtime_kind"] != "sglang" || receipt["kv_cache_dtype"] != "fp8_e4m3" || receipt["max_model_len"] != 262144 || receipt["max_num_seqs"] != 8 {
		t.Fatalf("sglang memory receipt=%#v", receipt)
	}
	// 0.85 of a 128 GB machine leaves less than the 16 GB reserve free, so
	// the live check must refuse the start.
	if _, err := executor.verifyMemory(context.Background(), r, true); err == nil {
		t.Fatal("live headroom check passed on an overcommitted machine")
	}
	// A recipe whose kind has no service block is a build error, not a
	// silently zero memory plan.
	r.Service.SGLang = nil
	if _, err := executor.verifyMemory(context.Background(), r, false); err == nil || !strings.Contains(err.Error(), "without its service block") {
		t.Fatalf("verifyMemory()=%v, want a missing-block error", err)
	}
}

func TestStartTimeoutFallsBackToTwentyMinutes(t *testing.T) {
	cases := []struct {
		minutes int
		want    time.Duration
	}{
		{0, 20 * time.Minute},
		{45, 45 * time.Minute},
		{-5, 20 * time.Minute},
	}
	for _, c := range cases {
		r := recipe.Recipe{Runtime: recipe.Runtime{StartTimeoutMinutes: c.minutes}}
		if got := startTimeout(r); got != c.want {
			t.Errorf("startTimeout(%d) = %s, want %s", c.minutes, got, c.want)
		}
	}
}

func TestHostDiskGuardRejectsBeforeRequiredHeadroom(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r := recipes[0]
	executor := &HostExecutor{inventory: resourceInventory{system: inventory.System{Hostname: "spark-full", StorageAvailable: r.RequiredBytes() - 1, DockerStorageAvailable: r.RequiredBytes() - 1, DockerSharesDataDisk: true}}}
	if _, err := executor.verifyDisk(context.Background(), r, r.TotalArtifactBytes(), r.Runtime.ImageDiskBytes, 0); err == nil || !strings.Contains(err.Error(), "spark-full") {
		t.Fatalf("insufficient disk passed: %v", err)
	}
}

func TestHostDiskGuardSubtractsOtherJobsReservation(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r := recipes[0]
	// Exactly enough for this job alone; a same-size reservation held by
	// another job must push it over the edge.
	system := inventory.System{Hostname: "spark-shared", StorageAvailable: r.RequiredBytes(), DockerStorageAvailable: r.RequiredBytes(), DockerSharesDataDisk: true}
	executor := &HostExecutor{inventory: resourceInventory{system: system}}
	if _, err := executor.verifyDisk(context.Background(), r, r.TotalArtifactBytes(), r.Runtime.ImageDiskBytes, 0); err != nil {
		t.Fatalf("job alone should fit: %v", err)
	}
	_, err = executor.verifyDisk(context.Background(), r, r.TotalArtifactBytes(), r.Runtime.ImageDiskBytes, r.RequiredBytes())
	if err == nil {
		t.Fatal("another job's reservation was not subtracted from free space")
	}
	if err.Error() != "not enough free space while another install is running, so wait for it to finish or free up space" {
		t.Fatalf("error does not match the UI-facing sentence: %v", err)
	}
}

// fakeVLLM serves a canned chat-completions response on 127.0.0.1 and returns
// an executor plus a recipe whose service port points at it.
func fakeVLLM(t *testing.T, response string) (*HostExecutor, recipe.Recipe) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	port, err := strconv.Atoi(server.URL[strings.LastIndex(server.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r := recipes[0]
	r.Service.DefaultHostPort = port
	return &HostExecutor{http: server.Client()}, r
}

// A model installed before the basement rename (spec 10) still runs under
// its pre-rename container name; resolveContainerName must find it instead
// of assuming it is missing, and Completed must recognize it as already
// created from its pre-rename labels. Live containers are never renamed
// (docs/plans/10-rename-basement.md).
func TestResolveContainerNameFallsBackToPreRenameContainer(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r := recipes[0]
	newName := containerName(r)
	legacyName := legacyContainerName(r)
	docker := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Path, url.PathEscape(newName)):
			return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		case strings.Contains(request.URL.Path, url.PathEscape(legacyName)):
			body := `{"ID":"legacy-id","State":{"Running":true,"Status":"running"},"Config":{"Labels":{"ai.runonspark.managed":"true","ai.runonspark.recipe-id":"` +
				r.ID + `","ai.runonspark.recipe-version":"` + strconv.Itoa(r.Version) + `"}}}`
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		default:
			t.Fatalf("unexpected container request: %s", request.URL)
			return nil, nil
		}
	})}}
	executor := &HostExecutor{docker: docker}

	if got := executor.resolveContainerName(context.Background(), r); got != legacyName {
		t.Fatalf("resolveContainerName = %q, want the pre-rename name %q", got, legacyName)
	}
	if !executor.Completed(context.Background(), Execution{}, recipe.Operation{Type: "create_container"}, r, nil) {
		t.Error("create_container was not recognized as already completed for a pre-rename container")
	}
	if !executor.Completed(context.Background(), Execution{}, recipe.Operation{Type: "start_container"}, r, nil) {
		t.Error("start_container was not recognized as already running for a pre-rename container")
	}
}

func TestVerifyInferenceAcceptsReasoningOnlyResponse(t *testing.T) {
	// A thinking model can spend its whole budget inside the reasoning
	// channel; that still proves the endpoint performs real inference.
	executor, r := fakeVLLM(t, `{"model":"m","choices":[{"finish_reason":"length","message":{"content":"","reasoning_content":"The user wants the word ready."}}]}`)
	receipt, err := executor.verifyInference(context.Background(), r)
	if err != nil {
		t.Fatalf("reasoning-only response rejected: %v", err)
	}
	if receipt["reasoning_only"] != true || receipt["answered"] != false {
		t.Fatalf("receipt misclassified response: %v", receipt)
	}
}

func TestVerifyInferenceEmptyResponseErrorCarriesDiagnostics(t *testing.T) {
	executor, r := fakeVLLM(t, `{"model":"m","choices":[{"finish_reason":"length","message":{"content":"","reasoning_content":""}}]}`)
	_, err := executor.verifyInference(context.Background(), r)
	if err == nil {
		t.Fatal("empty response passed")
	}
	for _, want := range []string{"empty model response", `finish_reason "length"`, `"choices"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q: %v", want, err)
		}
	}
}

func TestVerifyInferenceRequestsGenerousTokenBudget(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"finish_reason":"stop","message":{"content":"ready"}}]}`))
	}))
	t.Cleanup(server.Close)
	port, err := strconv.Atoi(server.URL[strings.LastIndex(server.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r := recipes[0]
	r.Service.DefaultHostPort = port
	executor := &HostExecutor{http: server.Client()}
	if _, err := executor.verifyInference(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if tokens, ok := got["max_tokens"].(float64); !ok || tokens < 256 {
		t.Fatalf("max_tokens %v is too small for reasoning models", got["max_tokens"])
	}
}

func fakeVLLMStream(t *testing.T, lines []string) (*HostExecutor, recipe.Recipe) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range lines {
			_, _ = w.Write([]byte("data: " + line + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(server.Close)
	port, err := strconv.Atoi(server.URL[strings.LastIndex(server.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r := recipes[0]
	r.Service.DefaultHostPort = port
	return &HostExecutor{http: server.Client()}, r
}

func TestBenchmarkCountsReasoningDeltas(t *testing.T) {
	// A thinking model streams its output as reasoning deltas (under either
	// field name) long before any visible content appears.
	executor, r := fakeVLLMStream(t, []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
		`{"choices":[{"delta":{"reasoning":"more"}}]}`,
		`{"choices":[{"delta":{"content":"ready"}}]}`,
		`{"choices":[],"usage":{"completion_tokens":42}}`,
	})
	receipt, err := executor.measureThroughput(context.Background(), r)
	if err != nil {
		t.Fatalf("reasoning stream rejected: %v", err)
	}
	if receipt["completion_tokens"] != int64(42) {
		t.Fatalf("usage tokens not used: %v", receipt)
	}
}

func TestBenchmarkFallsBackToUsageWhenDeltasUnrecognized(t *testing.T) {
	// If a future server renames the delta text fields again, the usage frame
	// still proves generation happened; the benchmark must not fail.
	executor, r := fakeVLLMStream(t, []string{
		`{"choices":[{"delta":{"some_future_field":"ready"}}]}`,
		`{"choices":[],"usage":{"completion_tokens":17}}`,
	})
	receipt, err := executor.measureThroughput(context.Background(), r)
	if err != nil {
		t.Fatalf("usage fallback failed: %v", err)
	}
	if receipt["completion_tokens"] != int64(17) {
		t.Fatalf("unexpected receipt: %v", receipt)
	}
}

func TestBenchmarkEmptyStreamErrorCarriesSamples(t *testing.T) {
	executor, r := fakeVLLMStream(t, []string{`{"choices":[]}`})
	_, err := executor.measureThroughput(context.Background(), r)
	if err == nil {
		t.Fatal("empty stream passed")
	}
	if !strings.Contains(err.Error(), `{"choices":[]}`) {
		t.Fatalf("error lacks stream sample: %v", err)
	}
}

func TestRetryNetworkRetriesTransientFailuresAutomatically(t *testing.T) {
	// A resolver blip mid-install (the exact ghcr.io SERVFAIL seen on a
	// Spark) must be retried without asking the user anything.
	executor := &HostExecutor{retryDelays: []time.Duration{0, 0}}
	calls := 0
	receipt, err := executor.retryNetwork(context.Background(), "pulling the runtime image", nil, func() (map[string]any, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("dial tcp: lookup ghcr.io on 127.0.0.53:53: server misbehaving")
		}
		return map[string]any{"image": "ok"}, nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if receipt["image"] != "ok" {
		t.Fatalf("receipt=%v", receipt)
	}
}

func TestRetryNetworkGivesUpWithMachineSideGuidance(t *testing.T) {
	executor := &HostExecutor{retryDelays: []time.Duration{0}}
	calls := 0
	_, err := executor.retryNetwork(context.Background(), "downloading model weights", nil, func() (map[string]any, error) {
		calls++
		return nil, errors.New("read tcp: connection reset by peer")
	})
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	if err == nil || !strings.Contains(err.Error(), "check this machine's connection and DNS") {
		t.Fatalf("error lacks guidance: %v", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("error hides the underlying cause: %v", err)
	}
}

func TestRetryNetworkSurfacesRealErrorsImmediately(t *testing.T) {
	// Auth failures, missing images and the disk guard are decisions, not
	// blips; retrying them would only hide the real problem.
	executor := &HostExecutor{retryDelays: []time.Duration{0, 0}}
	for _, message := range []string{
		"docker returned 401 Unauthorized: authentication required",
		"the image pull paused to protect free disk space: free space would drop below the safety margin",
	} {
		calls := 0
		_, err := executor.retryNetwork(context.Background(), "pulling the runtime image", nil, func() (map[string]any, error) {
			calls++
			return nil, errors.New(message)
		})
		if calls != 1 {
			t.Fatalf("%q retried %d times", message, calls)
		}
		if err == nil || err.Error() != message {
			t.Fatalf("error rewritten: %v", err)
		}
	}
}

func TestRetryNetworkStopsWhenCancelled(t *testing.T) {
	executor := &HostExecutor{retryDelays: []time.Duration{time.Hour}}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	done := make(chan error, 1)
	go func() {
		_, err := executor.retryNetwork(ctx, "pulling the runtime image", nil, func() (map[string]any, error) {
			calls++
			return nil, errors.New("dial tcp: i/o timeout")
		})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation swallowed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retry ignored cancellation")
	}
}

// chatTemplateRecipe is a recipe whose generated command names a file inside
// the mounted weights, which is what makes the checks below meaningful.
func chatTemplateRecipe(t *testing.T) recipe.Recipe {
	t.Helper()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "qwen36-27b-nvfp4-1s")
	if !ok {
		t.Fatal("the Qwen 3.6 27B recipe is missing")
	}
	if r.Service.VLLM == nil || r.Service.VLLM.ChatTemplateFile == "" {
		t.Fatal("the recipe no longer pins a chat template file")
	}
	return r
}

// writeArtifactFixture puts a complete-looking download where the executor
// expects this recipe's weights.
func writeArtifactFixture(t *testing.T, executor *HostExecutor, r recipe.Recipe) {
	t.Helper()
	target := executor.artifactPath(r, 0)
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, r.Service.VLLM.ChatTemplateFile), []byte("{{ messages }}"), 0o640); err != nil {
		t.Fatal(err)
	}
}

func containerFixtureJSON(id string, running bool, labels, mounts map[string]string, command ...string) string {
	status := "exited"
	if running {
		status = "running"
	}
	entries := make([]map[string]string, 0, len(mounts))
	for destination, source := range mounts {
		entries = append(entries, map[string]string{"Type": "bind", "Source": source, "Destination": destination})
	}
	encoded, err := json.Marshal(map[string]any{
		"ID":     id,
		"State":  map[string]any{"Running": running, "Status": status},
		"Config": map[string]any{"Labels": labels, "Cmd": command},
		"Mounts": entries,
	})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func dockerFixtureResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

// containerFromPath reads the container name out of /containers/<name>/...
func containerFromPath(path string) string {
	name := strings.TrimPrefix(path, "/containers/")
	if cut := strings.Index(name, "/"); cut >= 0 {
		name = name[:cut]
	}
	unescaped, err := url.PathUnescape(name)
	if err != nil {
		return name
	}
	return unescaped
}

func legacyLabels(r recipe.Recipe) map[string]string {
	return map[string]string{
		legacyLabelManaged:       "true",
		legacyLabelRecipeID:      r.ID,
		legacyLabelRecipeVersion: strconv.Itoa(r.Version),
	}
}

// A container keeps the bind mounts it was created with for life. An install
// adopted across the rename (spec 10) has its files moved out from under a
// container still pointed at the pre-rename data directory, and Docker fills
// a bind source that no longer exists with an empty directory rather than
// refusing to start — so the container comes back up with an empty /model and
// the runtime rejects the chat template path before it loads a single weight.
// Such a container has to be rebuilt, not started.
func TestStartContainerRebuildsAContainerLeftAtAMovedPath(t *testing.T) {
	r := chatTemplateRecipe(t)
	executor := &HostExecutor{dataDir: t.TempDir()}
	writeArtifactFixture(t, executor, r)
	newName, legacyName := containerName(r), legacyContainerName(r)
	staleMountSources := map[string]string{
		"/model":       "/var/lib/runonspark-manager/artifacts/nvidia--Qwen3.6-27B-NVFP4/" + r.Artifacts[0].Revision,
		cacheMountPath: "/var/lib/runonspark-manager/caches/" + r.ID,
	}
	created, started := false, false
	var stopped, removed []string
	executor.docker = &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		path := request.URL.Path
		name := containerFromPath(path)
		switch {
		case request.Method == http.MethodPost && strings.HasPrefix(path, "/containers/create"):
			if got := request.URL.Query().Get("name"); got != newName {
				t.Fatalf("rebuilt container name = %q, want %q", got, newName)
			}
			created = true
			return dockerFixtureResponse(http.StatusCreated, `{"ID":"rebuilt-id"}`), nil
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/stop"):
			stopped = append(stopped, name)
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case request.Method == http.MethodDelete:
			removed = append(removed, name)
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
			if name != newName {
				t.Fatalf("started %q, want the rebuilt container %q", name, newName)
			}
			started = true
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case name == newName:
			if !created {
				return dockerFixtureResponse(http.StatusNotFound, ""), nil
			}
			labels := map[string]string{labelManaged: "true", labelRecipeID: r.ID, labelRecipeVersion: strconv.Itoa(r.Version)}
			return dockerFixtureResponse(http.StatusOK, containerFixtureJSON("rebuilt-id", started, labels, executor.expectedMounts(r))), nil
		case name == legacyName:
			for _, gone := range removed {
				if gone == legacyName {
					return dockerFixtureResponse(http.StatusNotFound, ""), nil
				}
			}
			return dockerFixtureResponse(http.StatusOK, containerFixtureJSON("adopted-id", false, legacyLabels(r), staleMountSources)), nil
		default:
			t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL)
			return nil, nil
		}
	})}}

	// The stale container must not read as already created, or nothing ever
	// rebuilds it.
	if executor.Completed(context.Background(), Execution{}, recipe.Operation{Type: "create_container"}, r, nil) {
		t.Error("a container pointed at a moved directory was accepted as already created")
	}
	receipt, err := executor.Execute(context.Background(), Execution{}, recipe.Operation{Type: "start_container"}, r, nil)
	if err != nil {
		t.Fatalf("start_container: %v", err)
	}
	if !created || !started {
		t.Fatalf("created=%v started=%v, want the container rebuilt and started", created, started)
	}
	if len(stopped) != 1 || stopped[0] != legacyName || len(removed) != 1 || removed[0] != legacyName {
		t.Fatalf("stopped=%v removed=%v, want only the stale container %q", stopped, removed, legacyName)
	}
	moved, ok := receipt["recreated_after_move"].([]map[string]any)
	if !ok || len(moved) != 2 {
		t.Fatalf("receipt does not report the move: %#v", receipt["recreated_after_move"])
	}
	if moved[0]["mount_point"] != "/model" || moved[0]["now"] != executor.artifactPath(r, 0) {
		t.Fatalf("receipt mount detail = %#v", moved[0])
	}
}

// A container removed outside this manager — docker rm by hand, say while a
// machine was being repaired — leaves the stored install still naming it, so
// the start job takes the short plan and never creates anything. Before this
// the job died on a raw Docker 404 from the start call. The manager holds the
// recipe and every input, so it builds the container again exactly as a create
// would and starts that one.
func TestStartContainerRebuildsAContainerRemovedOutsideTheManager(t *testing.T) {
	r := chatTemplateRecipe(t)
	executor := &HostExecutor{dataDir: t.TempDir()}
	writeArtifactFixture(t, executor, r)
	newName, legacyName := containerName(r), legacyContainerName(r)
	created, started := false, false
	var createBody map[string]any
	executor.docker = &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		path := request.URL.Path
		name := containerFromPath(path)
		switch {
		case request.Method == http.MethodPost && strings.HasPrefix(path, "/containers/create"):
			if got := request.URL.Query().Get("name"); got != newName {
				t.Fatalf("rebuilt container name = %q, want %q", got, newName)
			}
			if err := json.NewDecoder(request.Body).Decode(&createBody); err != nil {
				t.Fatal(err)
			}
			created = true
			return dockerFixtureResponse(http.StatusCreated, `{"ID":"rebuilt-id"}`), nil
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
			if !created {
				t.Fatal("the missing container was started instead of being built again")
			}
			if name != newName {
				t.Fatalf("started %q, want the rebuilt container %q", name, newName)
			}
			started = true
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case request.Method == http.MethodGet && name == legacyName:
			return dockerFixtureResponse(http.StatusNotFound, ""), nil
		case request.Method == http.MethodGet && name == newName:
			if !created {
				return dockerFixtureResponse(http.StatusNotFound, ""), nil
			}
			labels := map[string]string{labelManaged: "true", labelRecipeID: r.ID, labelRecipeVersion: strconv.Itoa(r.Version)}
			return dockerFixtureResponse(http.StatusOK, containerFixtureJSON("rebuilt-id", started, labels, executor.expectedMounts(r))), nil
		default:
			t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL)
			return nil, nil
		}
	})}}

	receipt, err := executor.Execute(context.Background(), Execution{}, recipe.Operation{Type: "start_container"}, r, nil)
	if err != nil {
		t.Fatalf("start_container: %v", err)
	}
	if !created || !started {
		t.Fatalf("created=%v started=%v, want the container built again and started", created, started)
	}
	if receipt["container_id"] != "rebuilt-id" || receipt["running"] != true {
		t.Fatalf("receipt=%#v", receipt)
	}
	// The log has to say which repair happened: this container was gone, not
	// pointed at the wrong place.
	if receipt["recreated_missing"] != newName {
		t.Fatalf("receipt does not name the container it rebuilt: %#v", receipt["recreated_missing"])
	}
	if _, moved := receipt["recreated_after_move"]; moved {
		t.Errorf("a missing container was reported as a moved one: %#v", receipt["recreated_after_move"])
	}

	// The rebuilt container is a normal create: same mounts, same labels.
	host, _ := createBody["HostConfig"].(map[string]any)
	if host == nil {
		t.Fatalf("create request carried no host configuration: %#v", createBody)
	}
	binds := toStrings(host["Binds"].([]any))
	want := []string{executor.artifactPath(r, 0) + ":/model:ro", executor.cachePath(r) + ":" + cacheMountPath + ":rw"}
	if len(binds) != len(want) {
		t.Fatalf("binds=%#v, want %#v", binds, want)
	}
	for index, bind := range binds {
		if bind != want[index] {
			t.Fatalf("binds=%#v, want %#v", binds, want)
		}
	}
	labels, _ := createBody["Labels"].(map[string]any)
	if labels[labelManaged] != "true" || labels[labelRecipeID] != r.ID || labels[labelRecipeVersion] != strconv.Itoa(r.Version) {
		t.Fatalf("rebuilt container labels = %#v", labels)
	}
	// A container never exists without the record of how it was launched, even
	// when the start job is the step that built it.
	if _, err := os.Stat(executor.configPath(r)); err != nil {
		t.Fatalf("the launch record was not written for the rebuilt container: %v", err)
	}
}

// A container whose mounts still match is left exactly as it is: no stop, no
// remove, no rebuild.
func TestStartContainerLeavesAMatchingContainerAlone(t *testing.T) {
	r := chatTemplateRecipe(t)
	executor := &HostExecutor{dataDir: t.TempDir()}
	writeArtifactFixture(t, executor, r)
	newName := containerName(r)
	labels := map[string]string{labelManaged: "true", labelRecipeID: r.ID, labelRecipeVersion: strconv.Itoa(r.Version)}
	executor.docker = &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/start"):
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case request.Method == http.MethodGet && containerFromPath(request.URL.Path) == newName:
			return dockerFixtureResponse(http.StatusOK, containerFixtureJSON("live-id", true, labels, executor.expectedMounts(r))), nil
		default:
			t.Fatalf("a matching container was disturbed: %s %s", request.Method, request.URL)
			return nil, nil
		}
	})}}
	if !executor.Completed(context.Background(), Execution{}, recipe.Operation{Type: "create_container"}, r, nil) {
		t.Error("a container mounted from the current paths was not recognized as created")
	}
	receipt, err := executor.Execute(context.Background(), Execution{}, recipe.Operation{Type: "start_container"}, r, nil)
	if err != nil {
		t.Fatalf("start_container: %v", err)
	}
	if receipt["container_id"] != "live-id" || receipt["running"] != true {
		t.Fatalf("receipt=%#v", receipt)
	}
	if _, rebuilt := receipt["recreated_after_move"]; rebuilt {
		t.Error("a matching container was reported as rebuilt")
	}
	if _, missing := receipt["recreated_missing"]; missing {
		t.Error("a container that was right there was reported as missing")
	}
}

// The two Sparks meet at whatever address the kernel gave the cabled port,
// and nothing guarantees the same address after a reboot. A start job creates
// no container, so the container built by the last deployment would otherwise
// come back up telling its rank to meet at an address that no longer exists,
// and the model would hang with nothing to show for it. The address is
// re-resolved for every job; this is the container being rebuilt against it.
func TestStartContainerRebuildsWhenTheFabricAddressChanged(t *testing.T) {
	r := twoSparkRecipe(t)
	executor := &HostExecutor{dataDir: t.TempDir()}
	for index := range r.Artifacts {
		if err := os.MkdirAll(executor.artifactPath(r, index), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	labels := map[string]string{labelManaged: "true", labelRecipeID: r.ID, labelRecipeVersion: strconv.Itoa(r.Version), labelNodeRole: RoleWorker}
	// What the last deployment baked in: the head's address as it was then.
	yesterday := []string{"serve", "/model", "--node-rank", "1", "--master-addr", "169.254.205.1", "--master-port", "29501"}
	created, started, stopped, removed := false, false, false, false
	executor.docker = &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		path := request.URL.Path
		switch {
		case request.Method == http.MethodPost && strings.HasPrefix(path, "/containers/create"):
			created = true
			return dockerFixtureResponse(http.StatusCreated, `{"ID":"rebuilt-id"}`), nil
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/stop"):
			stopped = true
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case request.Method == http.MethodDelete:
			removed = true
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
			started = true
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case containerFromPath(path) == legacyContainerName(r):
			return dockerFixtureResponse(http.StatusNotFound, ""), nil
		case created:
			return dockerFixtureResponse(http.StatusOK, containerFixtureJSON("rebuilt-id", started, labels, executor.expectedMounts(r), "serve", "/model", "--node-rank", "1", "--master-addr", "169.254.37.9", "--master-port", "29501")), nil
		default:
			return dockerFixtureResponse(http.StatusOK, containerFixtureJSON("stale-id", false, labels, executor.expectedMounts(r), yesterday...)), nil
		}
	})}}

	// Today's address, resolved live by this job.
	worker := Placement{Role: RoleWorker, NodeName: "spark-b", NodeCount: 2, MasterAddress: "169.254.37.9", MasterPort: 29501}
	receipt, err := executor.Execute(context.Background(), Execution{Placement: worker}, recipe.Operation{Type: "start_container"}, r, nil)
	if err != nil {
		t.Fatalf("start_container: %v", err)
	}
	if !stopped || !removed || !created || !started {
		t.Fatalf("stopped=%v removed=%v created=%v started=%v, want the container rebuilt at the new address", stopped, removed, created, started)
	}
	drift, ok := receipt["recreated_after_address_change"].([]map[string]any)
	if !ok || len(drift) != 1 {
		t.Fatalf("receipt does not report the address change: %#v", receipt["recreated_after_address_change"])
	}
	if drift[0]["flag"] != "--master-addr" || drift[0]["was"] != "169.254.205.1" || drift[0]["now"] != "169.254.37.9" {
		t.Fatalf("receipt address detail = %#v", drift[0])
	}
	if _, moved := receipt["recreated_after_move"]; moved {
		t.Errorf("an address change was reported as a moved directory: %#v", receipt["recreated_after_move"])
	}
}

// A container already holding this job's address is left exactly as it is:
// re-resolving live must not mean rebuilding every start.
func TestStartContainerLeavesAnUnchangedFabricAddressAlone(t *testing.T) {
	r := twoSparkRecipe(t)
	executor := &HostExecutor{dataDir: t.TempDir()}
	for index := range r.Artifacts {
		if err := os.MkdirAll(executor.artifactPath(r, index), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	labels := map[string]string{labelManaged: "true", labelRecipeID: r.ID, labelRecipeVersion: strconv.Itoa(r.Version), labelNodeRole: RoleWorker}
	command := []string{"serve", "/model", "--node-rank", "1", "--master-addr", "169.254.205.1", "--master-port", "29501"}
	executor.docker = &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/start"):
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case request.Method == http.MethodGet && containerFromPath(request.URL.Path) == containerName(r):
			return dockerFixtureResponse(http.StatusOK, containerFixtureJSON("live-id", true, labels, executor.expectedMounts(r), command...)), nil
		default:
			t.Fatalf("a container already at the right address was disturbed: %s %s", request.Method, request.URL)
			return nil, nil
		}
	})}}

	worker := Placement{Role: RoleWorker, NodeName: "spark-b", NodeCount: 2, MasterAddress: "169.254.205.1", MasterPort: 29501}
	receipt, err := executor.Execute(context.Background(), Execution{Placement: worker}, recipe.Operation{Type: "start_container"}, r, nil)
	if err != nil {
		t.Fatalf("start_container: %v", err)
	}
	if _, rebuilt := receipt["recreated_after_address_change"]; rebuilt {
		t.Errorf("an unchanged address was reported as drift: %#v", receipt)
	}
}

// The same reboot, on the SGLang two-Spark recipe. SGLang bakes the
// rendezvous into one --dist-init-addr host:port value, so a drift check that
// only knew vLLM's --master-addr would find nothing to disagree with and the
// ranks would come back up waiting at an address the kernel no longer hands
// out. Detection is stubbed because CI runners can hold real RDMA hardware.
func TestStartContainerRebuildsAnSGLangRankWhenTheFabricAddressChanged(t *testing.T) {
	withFabric(t, FabricLink{NetDev: "enp1s0f1np1", HCA: "rocep1s0f1"}, nil, "169.254.37.9", nil)
	r := twoSparkSGLangRecipe(t)
	executor := &HostExecutor{dataDir: t.TempDir()}
	for index := range r.Artifacts {
		if err := os.MkdirAll(executor.artifactPath(r, index), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	labels := map[string]string{labelManaged: "true", labelRecipeID: r.ID, labelRecipeVersion: strconv.Itoa(r.Version), labelNodeRole: RoleWorker}
	// What the last deployment baked in: the head's address as it was then.
	yesterday := sglangArgs(r, Placement{Role: RoleWorker, NodeCount: 2, MasterAddress: "169.254.205.1", MasterPort: 29501})
	// Today's address, resolved live by this job.
	worker := Placement{Role: RoleWorker, NodeName: "spark-b", NodeCount: 2, MasterAddress: "169.254.37.9", MasterPort: 29501}
	created, started, stopped, removed := false, false, false, false
	executor.docker = &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		path := request.URL.Path
		switch {
		case request.Method == http.MethodPost && strings.HasPrefix(path, "/containers/create"):
			created = true
			return dockerFixtureResponse(http.StatusCreated, `{"ID":"rebuilt-id"}`), nil
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/stop"):
			stopped = true
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case request.Method == http.MethodDelete:
			removed = true
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
			started = true
			return dockerFixtureResponse(http.StatusNoContent, ""), nil
		case containerFromPath(path) == legacyContainerName(r):
			return dockerFixtureResponse(http.StatusNotFound, ""), nil
		case created:
			return dockerFixtureResponse(http.StatusOK, containerFixtureJSON("rebuilt-id", started, labels, executor.expectedMounts(r), sglangArgs(r, worker)...)), nil
		default:
			return dockerFixtureResponse(http.StatusOK, containerFixtureJSON("stale-id", false, labels, executor.expectedMounts(r), yesterday...)), nil
		}
	})}}

	receipt, err := executor.Execute(context.Background(), Execution{Placement: worker}, recipe.Operation{Type: "start_container"}, r, nil)
	if err != nil {
		t.Fatalf("start_container: %v", err)
	}
	if !stopped || !removed || !created || !started {
		t.Fatalf("stopped=%v removed=%v created=%v started=%v, want the container rebuilt at the new address", stopped, removed, created, started)
	}
	drift, ok := receipt["recreated_after_address_change"].([]map[string]any)
	if !ok || len(drift) != 1 {
		t.Fatalf("receipt does not report the address change: %#v", receipt["recreated_after_address_change"])
	}
	if drift[0]["flag"] != "--dist-init-addr" || drift[0]["was"] != "169.254.205.1:29501" || drift[0]["now"] != "169.254.37.9:29501" {
		t.Fatalf("receipt address detail = %#v", drift[0])
	}
}

// A recipe that names a file the download does not contain must fail as a
// plain sentence naming the file, before any container runs. Left to the
// runtime it surfaces as a Python traceback from argument validation.
func TestStartContainerNamesAMissingChatTemplate(t *testing.T) {
	r := chatTemplateRecipe(t)
	executor := &HostExecutor{dataDir: t.TempDir()}
	executor.docker = &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("Docker was contacted before the missing file was reported: %s %s", request.Method, request.URL)
		return nil, nil
	})}}

	// Nothing downloaded at all.
	_, err := executor.Execute(context.Background(), Execution{}, recipe.Operation{Type: "start_container"}, r, nil)
	if err == nil || !strings.Contains(err.Error(), "the downloaded files for "+r.Artifacts[0].Repository+" are not on this machine") {
		t.Fatalf("missing weights error = %v", err)
	}

	// Weights present, the one file the command names absent.
	if err := os.MkdirAll(executor.artifactPath(r, 0), 0o750); err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), Execution{}, recipe.Operation{Type: "start_container"}, r, nil)
	if err == nil {
		t.Fatal("a missing chat template started anyway")
	}
	for _, want := range []string{r.Service.VLLM.ChatTemplateFile, filepath.Join(executor.artifactPath(r, 0), r.Service.VLLM.ChatTemplateFile), "install it again"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Traceback") {
		t.Fatalf("error is not a plain sentence: %v", err)
	}
}
