package operations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// withoutNegotiation answers the client's /version negotiation probe with a
// 404 so it falls back to unversioned paths, keeping request expectations
// in these tests version-free.
func withoutNegotiation(inner roundTripFunc) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/version" {
			return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return inner(r)
	}
}

func TestDockerCreateUsesConstrainedStructuredRequest(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("Qwen 35 recipe missing")
	}
	var body map[string]any
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || !strings.Contains(request.URL.Path, "/containers/create") {
			t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL)
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ID":"container-id"}`))}, nil
	})}}
	id, err := client.Create(context.Background(), "managed-name", r.Runtime.Reference(), []string{"/managed/model"}, "/managed/cache", r)
	if err != nil {
		t.Fatal(err)
	}
	if id != "container-id" {
		t.Fatalf("id=%s", id)
	}
	environment, _ := body["Env"].([]any)
	tritonSteered := false
	for _, entry := range environment {
		if entry == "TRITON_CACHE_DIR=/root/.cache/triton" {
			tritonSteered = true
		}
	}
	if !tritonSteered {
		t.Fatalf("Triton cache is not steered into the writable mount (read-only rootfs would crash vLLM): %#v", body["Env"])
	}
	if body["Image"] != r.Runtime.Reference() {
		t.Fatalf("image is not digest pinned: %#v", body["Image"])
	}
	if _, ok := body["Entrypoint"].([]any); !ok {
		t.Fatalf("entrypoint is not structured argv: %#v", body["Entrypoint"])
	}
	host := body["HostConfig"].(map[string]any)
	if _, exists := host["Privileged"]; exists {
		t.Fatal("container requested privileged mode")
	}
	binds := host["Binds"].([]any)
	if len(binds) != 2 || binds[0] != "/managed/model:/model:ro" || binds[1] != "/managed/cache:/root/.cache:rw" {
		t.Fatalf("unsafe binds: %#v", binds)
	}
	if host["ReadonlyRootfs"] != true || host["ShmSize"] != float64(34359738368) {
		t.Fatalf("runtime constraints missing: %#v", host)
	}
	if _, exists := host["IpcMode"]; exists {
		t.Fatal("container requested host IPC namespace")
	}
	bindings := host["PortBindings"].(map[string]any)
	for _, value := range bindings {
		binding := value.([]any)[0].(map[string]any)
		if binding["HostIp"] != "127.0.0.1" {
			t.Fatalf("model port must stay loopback-only behind the manager proxy: %#v", binding)
		}
	}
}

func TestLagunaUsesSeparateReadOnlyDrafterMount(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "laguna-s-2-1-nvfp4-dflash-1s")
	if !ok {
		t.Fatal("Laguna recipe missing")
	}
	var body map[string]any
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ID":"container-id"}`))}, nil
	})}}
	_, err = client.Create(context.Background(), "laguna", r.Runtime.Reference(), []string{"/owned/target", "/owned/draft"}, "/owned/cache", r)
	if err != nil {
		t.Fatal(err)
	}
	binds := body["HostConfig"].(map[string]any)["Binds"].([]any)
	if len(binds) != 3 || binds[0] != "/owned/target:/model:ro" || binds[1] != "/owned/draft:/drafter:ro" {
		t.Fatalf("Laguna mounts=%#v", binds)
	}
	joined := strings.Join(toStrings(body["Cmd"].([]any)), " ")
	if !strings.Contains(joined, `"method":"dflash"`) || !strings.Contains(joined, `"model":"/drafter"`) {
		t.Fatalf("DFlash arguments missing: %s", joined)
	}
}

func TestChatTemplateKwargsAlwaysReachVLLM(t *testing.T) {
	// Laguna's own template defaults enable_thinking to true; the recipe's
	// explicit false must override it. Omitting the flag when both values
	// were false is how raw reasoning leaked into the playground.
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "laguna-s-2-1-nvfp4-dflash-1s")
	if !ok {
		t.Fatal("Laguna recipe missing")
	}
	if r.Service.VLLM.ChatTemplate.EnableThinking || r.Service.VLLM.ChatTemplate.PreserveThinking {
		t.Fatalf("this test assumes Laguna disables thinking; recipe says %+v", r.Service.VLLM.ChatTemplate)
	}
	joined := strings.Join(vllmArgs(r), " ")
	want := `--default-chat-template-kwargs {"enable_thinking":false,"preserve_thinking":false}`
	if !strings.Contains(joined, want) {
		t.Fatalf("explicit false kwargs missing from vLLM args: %s", joined)
	}
}

// sglangRecipe is the shape an SGLang recipe will have once one is
// qualified: no such recipe ships yet, so the command builder is pinned
// against a hand-built recipe rather than the catalog.
func sglangRecipe() recipe.Recipe {
	return recipe.Recipe{
		Runtime: recipe.Runtime{Kind: "sglang"},
		Artifacts: []recipe.Artifact{
			{Role: "primary", Repository: "example-lab/Example-NVFP4"},
			{Role: "drafter", Repository: "example-lab/Example-Draft"},
		},
		Service: recipe.Service{
			InternalPort: 8000, DefaultHostPort: 8000, ServedModelID: "example-lab/Example-NVFP4",
			SGLang: &recipe.SGLangConfig{
				TensorParallelSize: 1, MemFractionStatic: "0.85", ContextLength: 262144, MaxRunningRequests: 8,
				Quantization: "modelopt_fp4", KVCacheDType: "fp8_e4m3", AttentionBackend: "flashinfer",
				SpeculativeAlgorithm: "EAGLE3", SpeculativeNumDraftTokens: 4, SpeculativeModelRole: "drafter",
				ChatTemplateFile: "chat_template.jinja", ToolCallParser: "qwen3_coder", ReasoningParser: "qwen3",
			},
		},
	}
}

func TestSGLangCommandIsPinnedAndComplete(t *testing.T) {
	entrypoint, args, err := runtimeCommand(sglangRecipe())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(entrypoint, " ") != "python3 -m sglang.launch_server" {
		t.Fatalf("entrypoint=%q", entrypoint)
	}
	want := []string{
		"--model-path", "/model",
		"--host", "0.0.0.0",
		"--port", "8000",
		"--served-model-name", "example-lab/Example-NVFP4",
		"--enable-metrics",
		"--tp-size", "1",
		"--mem-fraction-static", "0.85",
		"--context-length", "262144",
		"--max-running-requests", "8",
		"--quantization", "modelopt_fp4",
		"--kv-cache-dtype", "fp8_e4m3",
		"--attention-backend", "flashinfer",
		"--speculative-algorithm", "EAGLE3",
		"--speculative-num-draft-tokens", "4",
		"--speculative-draft-model-path", "/drafter",
		"--chat-template", "/model/chat_template.jinja",
		"--tool-call-parser", "qwen3_coder",
		"--reasoning-parser", "qwen3",
	}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("sglang arguments\n got: %s\nwant: %s", strings.Join(args, " "), strings.Join(want, " "))
	}
}

// An unset field must leave the flag off the command line entirely, so the
// runtime's own default applies instead of a value the manager invented.
func TestSGLangCommandOmitsUnsetFields(t *testing.T) {
	r := sglangRecipe()
	r.Service.SGLang = &recipe.SGLangConfig{TensorParallelSize: 1, MemFractionStatic: "0.7", ContextLength: 8192, MaxRunningRequests: 1}
	joined := strings.Join(sglangArgs(r), " ")
	want := "--model-path /model --host 0.0.0.0 --port 8000 --served-model-name example-lab/Example-NVFP4 " +
		"--enable-metrics --tp-size 1 --mem-fraction-static 0.7 --context-length 8192 --max-running-requests 1"
	if joined != want {
		t.Fatalf("sglang arguments\n got: %s\nwant: %s", joined, want)
	}
}

func TestRuntimeCommandRejectsAMismatchedRecipe(t *testing.T) {
	r := sglangRecipe()
	r.Runtime.Kind = "vllm"
	if _, _, err := runtimeCommand(r); err == nil || !strings.Contains(err.Error(), "service.vllm") {
		t.Fatalf("runtimeCommand()=%v, want a missing-block error", err)
	}
	r.Runtime.Kind = "llamacpp"
	if _, _, err := runtimeCommand(r); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("runtimeCommand()=%v, want an unsupported-kind error", err)
	}
}

func toStrings(values []any) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index], _ = value.(string)
	}
	return result
}

func TestDockerNotFoundIsDistinguishableFromDaemonFailure(t *testing.T) {
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"missing"}`))}, nil
	})}}
	_, err := client.Container(context.Background(), "missing")
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Container()=%v", err)
	}
}

// ManagedContainers must recognize containers labelled under the pre-rename
// (spec 10) namespace, not just the current one: a machine set up before the
// rename still has containers created under it, and they are never
// relabeled in place (docs/plans/10-rename-basement.md).
func TestManagedContainersReadsBothLabelNamespaces(t *testing.T) {
	var requestedFilters string
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		if !strings.Contains(request.URL.Path, "/containers/json") {
			t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL)
		}
		requestedFilters = request.URL.Query().Get("filters")
		body := `[
			{"Names":["/basement-qwen-v1"],"State":"running","Labels":{"ai.basement.managed":"true","ai.basement.recipe-id":"qwen","ai.basement.recipe-version":"1"}},
			{"Names":["/runonspark-laguna-v3"],"State":"exited","Labels":{"ai.runonspark.managed":"true","ai.runonspark.recipe-id":"laguna","ai.runonspark.recipe-version":"3"}}
		]`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	containers, err := client.ManagedContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestedFilters, "ai.basement.managed=true") || !strings.Contains(requestedFilters, "ai.runonspark.managed=true") {
		t.Fatalf("filter did not request both label namespaces: %s", requestedFilters)
	}
	if len(containers) != 2 {
		t.Fatalf("got %d containers, want 2: %#v", len(containers), containers)
	}
	byName := map[string]ManagedContainer{}
	for _, c := range containers {
		byName[c.Name] = c
	}
	current, ok := byName["basement-qwen-v1"]
	if !ok || current.RecipeID != "qwen" || current.Version != "1" || !current.Running {
		t.Errorf("current-namespace container = %#v", current)
	}
	legacy, ok := byName["runonspark-laguna-v3"]
	if !ok || legacy.RecipeID != "laguna" || legacy.Version != "3" || legacy.Running {
		t.Errorf("legacy-namespace container = %#v", legacy)
	}
}

// The client must never pin a Docker API version: it negotiates the daemon's
// own ApiVersion via the unversioned /version endpoint and uses that for
// every call. A pinned version breaks either new daemons ("client version
// too old") or old ones.
func TestDockerClientNegotiatesAPIVersion(t *testing.T) {
	dir, err := os.MkdirTemp("", "rosm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var paths []string
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/version":
			json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.51"})
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go server.Serve(listener)
	defer server.Close()

	client := NewDockerClient(socket)
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 3 || paths[0] != "/version" || paths[1] != "/v1.51/_ping" || paths[2] != "/v1.51/_ping" {
		t.Fatalf("negotiation requests were %v; want unversioned /version once, then /v1.51-prefixed calls", paths)
	}
}

func TestPullAggregatesLayersIntoMonotonicProgress(t *testing.T) {
	// Docker reports one layer per event; the receipt must aggregate them so
	// the console bar never shrinks when the reported layer switches.
	events := []string{
		`{"status":"Downloading","id":"aaa","progressDetail":{"current":100,"total":1000}}`,
		`{"status":"Downloading","id":"bbb","progressDetail":{"current":50,"total":500}}`,
		`{"status":"Downloading","id":"aaa","progressDetail":{"current":900,"total":1000}}`,
		`{"status":"Download complete","id":"aaa","progressDetail":{}}`,
		`{"status":"Downloading","id":"bbb","progressDetail":{"current":500,"total":500}}`,
		`{"status":"Pull complete","id":"aaa","progressDetail":{}}`,
		`{"status":"Pull complete","id":"bbb","progressDetail":{}}`,
	}
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		if !strings.Contains(request.URL.Path, "/images/create") {
			t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Join(events, "\n")))}, nil
	})}}
	var receipts []map[string]any
	last, err := client.Pull(context.Background(), "vllm/vllm-openai@sha256:abc", func(update any) error {
		receipt, ok := update.(map[string]any)
		if !ok {
			t.Fatalf("unexpected receipt type %T", update)
		}
		copied := map[string]any{}
		for k, v := range receipt {
			copied[k] = v
		}
		receipts = append(receipts, copied)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	previous := int64(-1)
	for _, receipt := range receipts {
		complete := receipt["bytes_complete"].(int64)
		if complete < previous {
			t.Fatalf("aggregate bytes went backwards: %d -> %d", previous, complete)
		}
		previous = complete
	}
	if last["bytes_complete"].(int64) != 1500 || last["bytes_total"].(int64) != 1500 {
		t.Fatalf("final aggregate wrong: %v", last)
	}
	if last["status"] != "Extracting" || last["layers_done"] != 2 {
		t.Fatalf("final status wrong: %v", last)
	}
}
