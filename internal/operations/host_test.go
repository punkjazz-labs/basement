package operations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/runonspark-manager/internal/inventory"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
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
