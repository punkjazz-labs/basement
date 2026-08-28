package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
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
	id, err := client.Create(context.Background(), "managed-name", r.Runtime.Reference(), []string{"/managed/model"}, "/managed/cache", nil, r, Placement{})
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
	labels := body["Labels"].(map[string]any)
	if labels[labelHostPort] != fmt.Sprint(r.Service.DefaultHostPort) {
		t.Fatalf("container host-port label=%#v", labels[labelHostPort])
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

// TestMiniMaxH3ContainerPublishesComfyUIPortWithIsolatedIPC proves the first
// non-8000 recipe reaches Docker as its own port and never falls into the
// distributed host-IPC branch that would override its shared-memory policy.
func TestMiniMaxH3ContainerPublishesComfyUIPortWithIsolatedIPC(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	h3, ok := recipe.Find(recipes, "minimax-h3-comfyui-1s")
	if !ok {
		t.Fatal("MiniMax H3 recipe missing")
	}
	var body map[string]any
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ID":"container-id"}`))}, nil
	})}}
	writable := map[string]string{"/output": "/managed/output", "/input": "/managed/input"}
	if _, err := client.Create(context.Background(), "minimax-h3", h3.Runtime.Reference(), []string{"/managed/model"}, "/managed/cache", writable, h3, Placement{}); err != nil {
		t.Fatal(err)
	}
	host := body["HostConfig"].(map[string]any)
	bindings := host["PortBindings"].(map[string]any)
	binding, ok := bindings["8188/tcp"].([]any)
	if !ok || len(binding) != 1 {
		t.Fatalf("ComfyUI port binding=%#v", bindings)
	}
	target := binding[0].(map[string]any)
	if target["HostIp"] != "127.0.0.1" || target["HostPort"] != "8188" {
		t.Fatalf("ComfyUI binding=%#v, want loopback port 8188", target)
	}
	if _, exists := host["ShmSize"]; exists {
		t.Fatalf("MiniMax H3 requested an unmeasured shared-memory allocation: %#v", host["ShmSize"])
	}
	if _, exists := host["IpcMode"]; exists {
		t.Fatal("single-Spark MiniMax H3 requested the host IPC namespace")
	}
	if exposed := body["ExposedPorts"].(map[string]any); exposed["8188/tcp"] == nil {
		t.Fatalf("ComfyUI exposed ports=%#v", exposed)
	}
	joined := strings.Join(toStrings(body["Cmd"].([]any)), " ")
	if !strings.Contains(joined, "--port 8188") {
		t.Fatalf("ComfyUI command does not bind port 8188: %s", joined)
	}
}

// TestComfyUICachesLandInTheWritableMount is a regression from a real install
// failure. The comfyui image points TMPDIR and every library cache at
// /root/comfyui-temp and its home at /root/comfyui-user, both of which sit on
// the read-only root filesystem. torch._dynamo creates its inductor cache
// while it is still being imported, so the container exited before ComfyUI
// ever bound a port and the install failed at the health wait. Every path
// below has to be inside the one writable persistent mount.
func TestComfyUICachesLandInTheWritableMount(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	h3, ok := recipe.Find(recipes, "minimax-h3-comfyui-1s")
	if !ok {
		t.Fatal("MiniMax H3 recipe missing")
	}
	var body map[string]any
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ID":"container-id"}`))}, nil
	})}}
	if _, err := client.Create(context.Background(), "minimax-h3", h3.Runtime.Reference(), []string{"/managed/model"}, "/managed/cache", nil, h3, Placement{}); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{}
	for _, entry := range toStrings(body["Env"].([]any)) {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			t.Fatalf("container environment entry is not a name=value pair: %q", entry)
		}
		if previous, duplicated := environment[name]; duplicated {
			t.Fatalf("%s is set twice, as %q and %q, so which one applies depends on Docker", name, previous, value)
		}
		environment[name] = value
	}
	for _, name := range []string{
		"TMPDIR", "XDG_CACHE_HOME", "CUDA_CACHE_PATH", "HF_HOME", "NUMBA_CACHE_DIR",
		"TORCH_HOME", "TORCHINDUCTOR_CACHE_DIR", "TRITON_CACHE_DIR",
		"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
	} {
		value := environment[name]
		if value == "" {
			t.Fatalf("%s is unset, so the image's own read-only default applies", name)
		}
		if !strings.HasPrefix(value, recipe.CacheMountPath+"/") {
			t.Fatalf("%s=%s is not inside the writable mount %s", name, value, recipe.CacheMountPath)
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
	_, err = client.Create(context.Background(), "laguna", r.Runtime.Reference(), []string{"/owned/target", "/owned/draft"}, "/owned/cache", nil, r, Placement{})
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
	joined := strings.Join(vllmArgs(r, Placement{}), " ")
	want := `--default-chat-template-kwargs {"enable_thinking":false,"preserve_thinking":false}`
	if !strings.Contains(joined, want) {
		t.Fatalf("explicit false kwargs missing from vLLM args: %s", joined)
	}
}

// vLLM's fuse_norm_quant and fuse_act_quant compilation passes are on by
// default and garble DeepSeek V4 Flash output on SM121 (vllm-project/vllm
// issue 50773). The recipe asks for the documented workaround with a boolean,
// and the boolean may only ever produce this one document: the string below
// is the whole surface a recipe has over vLLM's compiler.
func TestDisabledQuantFusionsEmitTheDocumentedCompilationConfig(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	flash, ok := recipe.Find(recipes, "deepseek-v4-flash-0731-2s")
	if !ok {
		t.Fatal("DeepSeek V4 Flash two-Spark recipe missing")
	}
	if !flash.Service.VLLM.DisableQuantFusions {
		t.Fatal("this test assumes the DeepSeek V4 Flash recipe disables the quantization fusion passes")
	}
	head := Placement{Role: RoleHead, NodeName: "spark-a", NodeCount: 2, MasterAddress: "169.254.10.1", MasterPort: 29501}
	args := vllmArgs(flash, head)
	got, ok := argumentValue(args, "--compilation-config")
	if !ok {
		t.Fatalf("head arguments carry no --compilation-config: %s", strings.Join(args, " "))
	}
	want := `{"pass_config":{"fuse_norm_quant":false,"fuse_act_quant":false}}`
	if got != want {
		t.Fatalf("--compilation-config %s, want %s", got, want)
	}
	// One flag, one value: the knob must not be able to accumulate passes.
	count := 0
	for _, arg := range args {
		if arg == "--compilation-config" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("--compilation-config appeared %d times", count)
	}
}

// Every other recipe leaves vLLM's compiler alone, and a recipe that never
// names the field must launch byte-for-byte as it did before the field
// existed: no --compilation-config at all, not an empty or default one.
func TestRecipesWithoutTheKnobSendNoCompilationConfig(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range recipes {
		if r.Service.VLLM == nil || r.Service.VLLM.DisableQuantFusions {
			continue
		}
		seen++
		if hasArgument(vllmArgs(r, Placement{}), "--compilation-config") {
			t.Fatalf("%s gained a compilation config it never asked for", r.ID)
		}
	}
	if seen == 0 {
		t.Fatal("no vLLM recipe left the knob unset, so this test proved nothing")
	}
}

// The two-Spark DeepSeek V4 Flash recipe exists to reproduce a deployment of
// this model that has been observed serving, on the GB10 vLLM build that
// deployment runs. Every flag below was read off that deployment's own serve
// command, so this test is what keeps the recipe and the command builder from
// drifting away from it one field at a time.
func TestDeepSeekV4FlashLaunchesTheObservedServeCommand(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	flash, ok := recipe.Find(recipes, "deepseek-v4-flash-0731-2s")
	if !ok {
		t.Fatal("DeepSeek V4 Flash two-Spark recipe missing")
	}
	head := Placement{Role: RoleHead, NodeName: "spark-a", NodeCount: 2, MasterAddress: "169.254.10.1", MasterPort: 29501}
	args := vllmArgs(flash, head)
	for flag, want := range map[string]string{
		"--kv-cache-dtype":               "nvfp4_ds_mla",
		"--block-size":                   "256",
		"--moe-backend":                  "flashinfer_b12x",
		"--tokenizer-mode":               "deepseek_v4",
		"--reasoning-parser":             "deepseek_v4",
		"--tool-call-parser":             "deepseek_v4",
		"--max-cudagraph-capture-size":   "36",
		"--max-num-batched-tokens":       "8192",
		"--max-num-seqs":                 "6",
		"--gpu-memory-utilization":       "0.78",
		"--distributed-executor-backend": "mp",
	} {
		got, present := argumentValue(args, flag)
		if !present || got != want {
			t.Fatalf("%s=%q present=%v, want %q", flag, got, present, want)
		}
	}
	for _, flag := range []string{"--enable-prefix-caching", "--async-scheduling", "--enable-chunked-prefill", "--enable-auto-tool-choice", "--enable-flashinfer-autotune", "--trust-remote-code"} {
		if !hasArgument(args, flag) {
			t.Fatalf("%s missing from: %s", flag, strings.Join(args, " "))
		}
	}
	// The draft heads live in the served checkpoint, so the document names a
	// method and how it samples, and never a second model.
	spec, ok := argumentValue(args, "--speculative-config")
	if !ok {
		t.Fatalf("no --speculative-config: %s", strings.Join(args, " "))
	}
	want := `{"draft_sample_method":"probabilistic","method":"dspark","num_speculative_tokens":5}`
	if spec != want {
		t.Fatalf("--speculative-config %s, want %s", spec, want)
	}
	// The capture ladder must bound the batches this recipe can actually run:
	// max_num_seqs sequences drafting speculative_tokens tokens each.
	v := flash.Service.VLLM
	if v.MaxCUDAGraphCaptureSize != v.MaxNumSeqs*(v.SpeculativeTokens+1) {
		t.Fatalf("capture size %d does not match %d sequences of %d drafted tokens", v.MaxCUDAGraphCaptureSize, v.MaxNumSeqs, v.SpeculativeTokens)
	}
}

// The GB10 build's flags must stay opt-in. A recipe that names none of these
// fields has to launch byte-for-byte as it did before they existed, because
// every other image in the pack would reject them at start.
func TestRecipesWithoutTheGB10KnobsSendNoneOfTheirFlags(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range recipes {
		v := r.Service.VLLM
		if v == nil || v.BlockSize > 0 || v.MaxCUDAGraphCaptureSize > 0 || v.TokenizerMode != "" || v.FlashInferAutotune || v.SpeculativeDraftSampleMethod != "" {
			continue
		}
		seen++
		args := vllmArgs(r, Placement{})
		for _, flag := range []string{"--block-size", "--max-cudagraph-capture-size", "--tokenizer-mode", "--enable-flashinfer-autotune"} {
			if hasArgument(args, flag) {
				t.Fatalf("%s gained %s it never asked for", r.ID, flag)
			}
		}
		if spec, ok := argumentValue(args, "--speculative-config"); ok && strings.Contains(spec, "draft_sample_method") {
			t.Fatalf("%s gained a draft sample method it never asked for: %s", r.ID, spec)
		}
	}
	if seen == 0 {
		t.Fatal("every vLLM recipe set the new knobs, so this test proved nothing")
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
				TrustRemoteCode: true, ChunkedPrefillSize: 8192, DisablePrefillCUDAGraph: true,
				MambaSSMDType: "bfloat16", MambaFullMemoryRatio: "4.21", MambaRadixCacheStrategy: "extra_buffer_lazy",
				MaxMambaCacheSize: 40, SpeculativeNumSteps: 3, SpeculativeEagleTopK: 1, SamplingDefaults: "model",
				PageSize: 64, MambaTrackInterval: 64, AllowAutoTruncate: true, PLEOffloadEmbedding: true,
				CudaGraphBSDecode: "1 2 4 8",
			},
		},
	}
}

func TestSGLangCommandIsPinnedAndComplete(t *testing.T) {
	entrypoint, args, err := runtimeCommand(sglangRecipe(), Placement{})
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
		"--trust-remote-code",
		"--chunked-prefill-size", "8192",
		"--disable-prefill-cuda-graph",
		"--mamba-ssm-dtype", "bfloat16",
		"--mamba-full-memory-ratio", "4.21",
		"--mamba-radix-cache-strategy", "extra_buffer_lazy",
		"--max-mamba-cache-size", "40",
		"--speculative-num-steps", "3",
		"--speculative-eagle-topk", "1",
		"--sampling-defaults", "model",
		"--page-size", "64",
		"--mamba-track-interval", "64",
		"--allow-auto-truncate",
		"--ple-offload-embedding",
		// One batch size per argument: SGLang's own parser reads a list here,
		// so a single "1 2 4 8" argument would be read as one bad number.
		"--cuda-graph-bs-decode", "1", "2", "4", "8",
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
	args := sglangArgs(r, Placement{})
	joined := strings.Join(args, " ")
	want := "--model-path /model --host 0.0.0.0 --port 8000 --served-model-name example-lab/Example-NVFP4 " +
		"--enable-metrics --tp-size 1 --mem-fraction-static 0.7 --context-length 8192 --max-running-requests 1"
	if joined != want {
		t.Fatalf("sglang arguments\n got: %s\nwant: %s", joined, want)
	}
	for _, flag := range []string{
		"--trust-remote-code", "--chunked-prefill-size", "--disable-prefill-cuda-graph",
		"--mamba-ssm-dtype", "--mamba-full-memory-ratio", "--mamba-radix-cache-strategy",
		"--max-mamba-cache-size", "--speculative-num-steps", "--speculative-eagle-topk",
		"--sampling-defaults", "--page-size", "--mamba-track-interval", "--allow-auto-truncate",
		"--ple-offload-embedding", "--cuda-graph-bs-decode",
	} {
		if hasArgument(args, flag) {
			t.Fatalf("sglang arguments carry %s with every hybrid-attention field unset", flag)
		}
	}
}

// A decode graph list that names no batch size must leave the flag off. The
// validator refuses such a list, so this can only come from a caller that
// built a configuration in Go; a bare --cuda-graph-bs-decode would then take
// the next flag as its first batch size and the server would refuse to start.
func TestSGLangCommandOmitsAnEmptyDecodeGraphList(t *testing.T) {
	r := sglangRecipe()
	block := *r.Service.SGLang
	block.CudaGraphBSDecode = "   "
	r.Service.SGLang = &block
	if args := sglangArgs(r, Placement{}); hasArgument(args, "--cuda-graph-bs-decode") {
		t.Fatalf("sglang arguments carry a decode graph flag with no batch size: %s", strings.Join(args, " "))
	}
}

// twoSparkSGLangRecipe is the shipped Inkling recipe: the pack's SGLang
// two-Spark serve, taken from the catalog rather than hand-built so these
// assertions are about a model the manager really installs.
func twoSparkSGLangRecipe(t *testing.T) recipe.Recipe {
	t.Helper()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "inkling-small-nvfp4-2s")
	if !ok {
		t.Fatal("Inkling recipe missing")
	}
	if !r.Distributed() || r.Runtime.Kind != "sglang" {
		t.Fatalf("fixture is not a two-Spark SGLang recipe: kind=%s spark_count=%d", r.Runtime.Kind, r.Topology.SparkCount)
	}
	return r
}

// SGLang launches across two nodes with its own vocabulary: --nnodes,
// --node-rank and a single --dist-init-addr host:port. Borrowing vLLM's
// --master-addr/--master-port here would leave both ranks waiting for a
// rendezvous neither of them was ever told about.
func TestDistributedSGLangUsesItsOwnMultiNodeFlags(t *testing.T) {
	r := twoSparkSGLangRecipe(t)
	head := Placement{Role: RoleHead, NodeName: "spark-a", NodeCount: 2, MasterAddress: "169.254.10.1", MasterPort: 29501}
	worker := Placement{Role: RoleWorker, NodeName: "spark-b", NodeCount: 2, MasterAddress: "169.254.10.1", MasterPort: 29501}

	entrypoint, headArgs, err := runtimeCommand(r, head)
	if err != nil {
		t.Fatalf("distributed sglang command: %v", err)
	}
	if strings.Join(entrypoint, " ") != "python3 -m sglang.launch_server" {
		t.Fatalf("entrypoint=%q", entrypoint)
	}
	for flag, want := range map[string]string{
		"--nnodes": "2", "--node-rank": "0", "--dist-init-addr": "169.254.10.1:29501", "--tp-size": "2",
	} {
		if got, ok := argumentValue(headArgs, flag); !ok || got != want {
			t.Fatalf("head %s=%q, want %q", flag, got, want)
		}
	}
	// vLLM's flags are not SGLang's; none of them may leak into this command.
	for _, flag := range []string{"--master-addr", "--master-port", "--headless", "--distributed-executor-backend", "--nccl-init-addr"} {
		if hasArgument(headArgs, flag) {
			t.Fatalf("sglang launch grew a flag from another runtime: %s", flag)
		}
	}
	// ADR 0007: under host networking nothing publishes a port, so the head
	// binds the recipe's host port on loopback itself.
	if host, _ := argumentValue(headArgs, "--host"); host != "127.0.0.1" {
		t.Fatalf("head bound %q, want loopback", host)
	}
	if port, _ := argumentValue(headArgs, "--port"); port != fmt.Sprint(r.Service.DefaultHostPort) {
		t.Fatalf("head port %q, want the recipe host port", port)
	}

	workerArgs := sglangArgs(r, worker)
	if rank, _ := argumentValue(workerArgs, "--node-rank"); rank != "1" {
		t.Fatalf("worker rank %q, want 1", rank)
	}
	// Both ranks meet at the head's address, so the worker carries the same
	// rendezvous value and differs only in its rank.
	if address, _ := argumentValue(workerArgs, "--dist-init-addr"); address != "169.254.10.1:29501" {
		t.Fatalf("worker rendezvous %q, want the head's", address)
	}
	// The worker is handed a host and a port like every other rank and, with
	// no headless mode to stop it, binds them on its own machine. Its Spark's
	// own preflight has to know that, or the port is checked on one machine
	// and bound on the other.
	if host, _ := argumentValue(workerArgs, "--host"); host != "127.0.0.1" {
		t.Fatalf("worker bound %q, want loopback", host)
	}
	if port, binds := RankBindsHostPort(r, worker); !binds || port != r.Service.DefaultHostPort {
		t.Fatalf("sglang worker binds (%d, %v), want the recipe host port", port, binds)
	}
	if port, binds := RankBindsHostPort(r, head); !binds || port != r.Service.DefaultHostPort {
		t.Fatalf("sglang head binds (%d, %v), want the recipe host port", port, binds)
	}
	// A vLLM worker is --headless and binds nothing, and must stay that way.
	vllm := twoSparkRecipe(t)
	if _, binds := RankBindsHostPort(vllm, worker); binds {
		t.Fatal("a headless vLLM worker was reported as binding a host port")
	}
	if port, binds := RankBindsHostPort(vllm, head); !binds || port != vllm.Service.DefaultHostPort {
		t.Fatalf("vllm head binds (%d, %v), want the recipe host port", port, binds)
	}
	// Single-node serving binds the recipe's host port, as it always has.
	if port, binds := RankBindsHostPort(vllm, Placement{}); !binds || port != vllm.Service.DefaultHostPort {
		t.Fatalf("single-node binds (%d, %v), want the recipe host port", port, binds)
	}

	// A single-node SGLang serve must be untouched by any of this.
	singleArgs := sglangArgs(sglangRecipe(), Placement{})
	for _, flag := range []string{"--nnodes", "--node-rank", "--dist-init-addr"} {
		if hasArgument(singleArgs, flag) {
			t.Fatalf("single-Spark sglang launch grew %s", flag)
		}
	}
	if host, _ := argumentValue(singleArgs, "--host"); host != "0.0.0.0" {
		t.Fatalf("single-Spark sglang host changed to %q", host)
	}
}

// Reboot drift is measured against the flag the runtime was actually given.
// SGLang carries the rendezvous as one host:port value, so comparing vLLM's
// two flags against an SGLang container would find nothing to disagree with
// and the ranks would come back up pointed at an address that is gone.
func TestStaleLaunchComparesEachRuntimesRendezvousFlag(t *testing.T) {
	sglang := twoSparkSGLangRecipe(t)
	vllm := twoSparkRecipe(t)
	worker := Placement{Role: RoleWorker, NodeName: "spark-b", NodeCount: 2, MasterAddress: "169.254.37.9", MasterPort: 29501}

	yesterday := ContainerState{Command: sglangArgs(sglang, Placement{Role: RoleWorker, NodeCount: 2, MasterAddress: "169.254.205.1", MasterPort: 29501})}
	drift := staleLaunch(yesterday, sglang, worker)
	if len(drift) != 1 || drift[0].Flag != "--dist-init-addr" || drift[0].Actual != "169.254.205.1:29501" || drift[0].Expected != "169.254.37.9:29501" {
		t.Fatalf("sglang drift=%#v, want the rendezvous flag alone", drift)
	}

	today := ContainerState{Command: sglangArgs(sglang, worker)}
	if drift := staleLaunch(today, sglang, worker); len(drift) != 0 {
		t.Fatalf("a container already at this job's address was called stale: %#v", drift)
	}

	// A half-resolved placement knows nothing about where the ranks meet, and
	// SGLang's single value cannot be compared on one half alone.
	unresolved := Placement{Role: RoleWorker, NodeCount: 2, MasterAddress: "169.254.37.9"}
	if drift := staleLaunch(yesterday, sglang, unresolved); len(drift) != 0 {
		t.Fatalf("an unresolved rendezvous produced drift: %#v", drift)
	}

	// The vLLM shape is unchanged, and a single-node placement never drifts.
	vllmYesterday := ContainerState{Command: vllmArgs(vllm, Placement{Role: RoleWorker, NodeCount: 2, MasterAddress: "169.254.205.1", MasterPort: 29501})}
	drift = staleLaunch(vllmYesterday, vllm, worker)
	if len(drift) != 1 || drift[0].Flag != "--master-addr" || drift[0].Expected != "169.254.37.9" {
		t.Fatalf("vllm drift=%#v, want the master address alone", drift)
	}
	if drift := staleLaunch(yesterday, sglang, Placement{}); drift != nil {
		t.Fatalf("a single-node placement produced drift: %#v", drift)
	}
}

// llamaCppRecipe is the shipped DeepSeek V4 Flash 3-bit recipe, taken from
// the catalog rather than hand-built so these assertions are about the model
// the manager really installs.
func llamaCppRecipe(t *testing.T) recipe.Recipe {
	t.Helper()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "deepseek-v4-flash-0731-ud-iq3-xxs-1s")
	if !ok {
		t.Fatal("DeepSeek 3-bit recipe missing")
	}
	if r.Runtime.Kind != "llamacpp" || r.Distributed() {
		t.Fatalf("fixture is not a single-Spark llama.cpp recipe: kind=%s spark_count=%d", r.Runtime.Kind, r.Topology.SparkCount)
	}
	return r
}

// llama-server is pointed at one GGUF file rather than at a snapshot
// directory, which is the structural difference between this kind and the
// other two: the model argument is a path built from the recipe's own pinned
// file name, joined to the primary mount.
func TestLlamaCppCommandIsPinnedAndComplete(t *testing.T) {
	r := llamaCppRecipe(t)
	entrypoint, args, err := runtimeCommand(r, Placement{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(entrypoint, " ") != "/app/llama-server" {
		t.Fatalf("entrypoint=%q", entrypoint)
	}
	want := []string{
		"--model", "/model/UD-IQ3_XXS/DeepSeek-V4-Flash-0731-UD-IQ3_XXS-00001-of-00004.gguf",
		"--host", "0.0.0.0",
		"--port", "8000",
		"--alias", "unsloth/DeepSeek-V4-Flash-0731-GGUF",
		"--metrics",
		"--ctx-size", "32768",
		"--n-gpu-layers", "999",
		"--parallel", "1",
		"--jinja",
	}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("llamacpp arguments\n got: %s\nwant: %s", strings.Join(args, " "), strings.Join(want, " "))
	}
}

// An unset field must leave the flag off the command line entirely, so the
// runtime's own default applies instead of a value the manager invented. This
// matters more than usual for --flash-attn: llama-server requires a value for
// it, so a manager that emitted the bare flag would consume the next argument
// as its value and the server would refuse to start.
func TestLlamaCppCommandOmitsUnsetFields(t *testing.T) {
	r := llamaCppRecipe(t)
	r.Service.LlamaCpp = &recipe.LlamaCppConfig{ModelFile: "weights.gguf", ContextSize: 8192, Parallel: 1}
	joined := strings.Join(llamaCppArgs(r, Placement{}), " ")
	want := "--model /model/weights.gguf --host 0.0.0.0 --port 8000 " +
		"--alias unsloth/DeepSeek-V4-Flash-0731-GGUF --metrics --ctx-size 8192 --parallel 1"
	if joined != want {
		t.Fatalf("llamacpp arguments\n got: %s\nwant: %s", joined, want)
	}
	r.Service.LlamaCpp.FlashAttention = "on"
	r.Service.LlamaCpp.ChatTemplateFile = "template.jinja"
	joined = strings.Join(llamaCppArgs(r, Placement{}), " ")
	for _, want := range []string{"--flash-attn on", "--chat-template-file /model/template.jinja"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("llamacpp arguments missing %q: %s", want, joined)
		}
	}
}

// The weights are a named file for this kind, so the pre-start file check has
// to cover them: a missing shard is caught before the container runs instead
// of as a loader crash inside it.
func TestLlamaCppChecksItsWeightsFileBeforeStarting(t *testing.T) {
	r := llamaCppRecipe(t)
	refs := runtimeFileReferences(r)
	if len(refs) != 1 || refs[0].Role != "primary" || refs[0].Name != r.Service.LlamaCpp.ModelFile {
		t.Fatalf("runtimeFileReferences()=%#v, want the pinned GGUF", refs)
	}
	r.Service.LlamaCpp.ChatTemplateFile = "template.jinja"
	if refs := runtimeFileReferences(r); len(refs) != 2 || refs[1].Name != "template.jinja" {
		t.Fatalf("runtimeFileReferences()=%#v, want the GGUF and the template", refs)
	}
}

// Nothing here builds llama.cpp RPC ranks. The validator refuses a multi-Spark
// llama.cpp recipe, and if one reached the command builder anyway it must
// refuse rather than quietly launch a single-node server on each machine.
func TestLlamaCppRefusesADistributedPlacement(t *testing.T) {
	r := llamaCppRecipe(t)
	placement := Placement{Role: RoleHead, NodeName: "spark-a", NodeCount: 2, MasterAddress: "169.254.10.1", MasterPort: 29501}
	if _, _, err := runtimeCommand(r, placement); err == nil || !strings.Contains(err.Error(), "single Spark") {
		t.Fatalf("runtimeCommand()=%v, want a single-Spark refusal", err)
	}
}

func TestRuntimeCommandRejectsAMismatchedRecipe(t *testing.T) {
	r := sglangRecipe()
	r.Runtime.Kind = "vllm"
	if _, _, err := runtimeCommand(r, Placement{}); err == nil || !strings.Contains(err.Error(), "service.vllm") {
		t.Fatalf("runtimeCommand()=%v, want a missing-block error", err)
	}
	r.Runtime.Kind = "llamacpp"
	if _, _, err := runtimeCommand(r, Placement{}); err == nil || !strings.Contains(err.Error(), "service.llamacpp") {
		t.Fatalf("runtimeCommand()=%v, want a missing-block error", err)
	}
	r.Runtime.Kind = "tensorrt"
	if _, _, err := runtimeCommand(r, Placement{}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("runtimeCommand()=%v, want an unsupported-kind error", err)
	}
}

// createHostConfig runs a container creation against a stub daemon and hands
// back the HostConfig the manager asked for.
func createHostConfig(t *testing.T, r recipe.Recipe) map[string]any {
	t.Helper()
	var body map[string]any
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ID":"container-id"}`))}, nil
	})}}
	paths := make([]string, len(r.Artifacts))
	for index := range r.Artifacts {
		paths[index] = fmt.Sprintf("/managed/artifact-%d", index)
	}
	if _, err := client.Create(context.Background(), "managed-name", r.Runtime.Reference(), paths, "/managed/cache", nil, r, Placement{}); err != nil {
		t.Fatal(err)
	}
	host, ok := body["HostConfig"].(map[string]any)
	if !ok {
		t.Fatalf("create request carried no host configuration: %#v", body)
	}
	return host
}

// A recipe's writable paths become their own tmpfs, and that tmpfs is not
// noexec: tilelang compiles a shared object into its cache and then loads it,
// so a noexec mount would trade the read-only failure for a load failure. The
// scratch mount is untouched by any of this, and a recipe that declares
// nothing gets exactly the filesystem that shipped before.
func TestWritablePathsBecomeTheirOwnLoadableTmpfs(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	flash, ok := recipe.Find(recipes, "deepseek-v4-flash-0731-2s")
	if !ok {
		t.Fatal("DeepSeek V4 Flash two-Spark recipe missing")
	}
	tmpfs, _ := createHostConfig(t, flash)["Tmpfs"].(map[string]any)
	if len(tmpfs) != 3 {
		t.Fatalf("tmpfs=%#v, want the scratch mount, the tilelang cache and its temp dir", tmpfs)
	}
	if tmpfs["/tmp"] != "rw,noexec,nosuid,size=8g" {
		t.Fatalf("the scratch mount changed: %#v", tmpfs["/tmp"])
	}
	if options, _ := tmpfs["/root/tmp"].(string); options != "rw,exec,nosuid,size=4g" {
		t.Fatalf("temp dir mounted %q, want a bounded writable mount", options)
	}
	options, _ := tmpfs["/root/.tilelang"].(string)
	if options != "rw,exec,nosuid,size=4g" {
		t.Fatalf("tilelang cache mounted %q, want a bounded writable mount", options)
	}
	if strings.Contains(options, "noexec") {
		t.Fatalf("a JIT kernel cache was mounted noexec: %q", options)
	}
	if !strings.Contains(options, "nosuid") {
		t.Fatalf("a writable path was mounted without nosuid: %q", options)
	}

	// Every runtime kind gets the same treatment; nothing about this is vLLM's.
	for _, id := range []string{"inkling-small-nvfp4-2s", "deepseek-v4-flash-0731-ud-iq3-xxs-1s"} {
		r, ok := recipe.Find(recipes, id)
		if !ok {
			t.Fatalf("%s recipe missing", id)
		}
		r.Runtime.WritablePaths = []string{"/root/.tilelang"}
		tmpfs, _ := createHostConfig(t, r)["Tmpfs"].(map[string]any)
		if tmpfs["/root/.tilelang"] != "rw,exec,nosuid,size=4g" || tmpfs["/tmp"] != "rw,noexec,nosuid,size=8g" {
			t.Fatalf("%s (%s) tmpfs=%#v", id, r.Runtime.Kind, tmpfs)
		}
	}

	// A recipe declaring nothing keeps the single scratch mount.
	qwen, ok := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("Qwen 35 recipe missing")
	}
	tmpfs, _ = createHostConfig(t, qwen)["Tmpfs"].(map[string]any)
	if len(tmpfs) != 1 || tmpfs["/tmp"] != "rw,noexec,nosuid,size=8g" {
		t.Fatalf("a recipe with no writable paths got tmpfs=%#v", tmpfs)
	}
}

// Docker fixes a container's tmpfs mounts at creation, exactly as it fixes its
// binds, so a container built before its recipe declared a writable path never
// gains one. Without this the update would install a recipe the running
// container does not implement, and the worker would keep dying on a
// read-only cache directory after every weight was loaded.
func TestStaleTmpfsNoticesADriftedWritableSurface(t *testing.T) {
	expected := map[string]string{"/tmp": tempTmpfsOptions, "/root/.tilelang": writableTmpfsOptions}

	yesterday := ContainerState{Tmpfs: map[string]string{"/tmp": tempTmpfsOptions}}
	drift := staleTmpfs(yesterday, expected)
	if len(drift) != 1 || drift[0].MountPoint != "/root/.tilelang" || drift[0].Actual != "" || drift[0].Expected != writableTmpfsOptions {
		t.Fatalf("drift=%#v, want the missing writable path alone", drift)
	}

	if drift := staleTmpfs(ContainerState{Tmpfs: expected}, expected); len(drift) != 0 {
		t.Fatalf("a container already carrying its writable paths was called stale: %#v", drift)
	}

	// A path the recipe has dropped is drift too: a writable, loadable mount
	// must not outlive the recipe that asked for it.
	extra := ContainerState{Tmpfs: map[string]string{"/tmp": tempTmpfsOptions, "/root/.tilelang": writableTmpfsOptions}}
	if drift := staleTmpfs(extra, map[string]string{"/tmp": tempTmpfsOptions}); len(drift) != 1 || drift[0].MountPoint != "/root/.tilelang" || drift[0].Expected != "" {
		t.Fatalf("drift=%#v, want the withdrawn writable path", drift)
	}

	// Changed options count as much as a missing mount.
	relaxed := ContainerState{Tmpfs: map[string]string{"/tmp": tempTmpfsOptions, "/root/.tilelang": "rw,noexec,nosuid,size=4g"}}
	if drift := staleTmpfs(relaxed, expected); len(drift) != 1 || drift[0].MountPoint != "/root/.tilelang" {
		t.Fatalf("drift=%#v, want the differently mounted path", drift)
	}

	// A daemon that reports no host configuration is silence, not
	// disagreement, and no container is rebuilt on silence.
	if drift := staleTmpfs(ContainerState{}, expected); drift != nil {
		t.Fatalf("an unreported tmpfs produced drift: %#v", drift)
	}
}

// A recipe can pin a new image digest while its version stays put, which is
// how the two-Spark Flash recipe moved from vLLM v0.25.1 to v0.26.0 without
// renaming and orphaning its container. Docker fixes a container's image at
// creation, so pulling the new digest changes nothing about the container that
// already exists: create_container finds one with the right name, labels and
// version, reuses it, and the machine keeps serving the old image while every
// receipt names the new one.
func TestStaleImageNoticesAContainerLeftOnTheOldDigest(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "deepseek-v4-flash-0731-2s")
	if !ok {
		t.Fatal("DeepSeek V4 Flash two-Spark recipe missing")
	}
	previous := r.Runtime.Image + "@sha256:e4f88a835143cd22aee2397a26ec6bb80b3a4a6fe0c882bcbc63822904766089"
	if previous == r.Runtime.Reference() {
		t.Fatal("this test needs a digest the recipe has moved off")
	}

	drift := staleImage(ContainerState{Image: previous}, r)
	if drift == nil || drift.Actual != previous || drift.Expected != r.Runtime.Reference() {
		t.Fatalf("drift=%#v, want the old digest against the pinned one", drift)
	}
	if receipt := imageMismatchReceipt(*drift); receipt["was"] != previous || receipt["now"] != r.Runtime.Reference() {
		t.Fatalf("receipt=%#v, want both digests named", receipt)
	}

	if drift := staleImage(ContainerState{Image: r.Runtime.Reference()}, r); drift != nil {
		t.Fatalf("a container already on the pinned image was called stale: %#v", drift)
	}

	// A daemon that reports no image is silence, not disagreement, and no
	// container is rebuilt on silence.
	if drift := staleImage(ContainerState{}, r); drift != nil {
		t.Fatalf("an unreported image produced drift: %#v", drift)
	}
}

// A recipe's serve settings can change without its version changing, just as
// its pinned image can. Docker keeps the old Config.Cmd until the container is
// rebuilt, so reconciliation has to name the changed flag rather than accept
// labels that still happen to match.
func TestStaleCommandNoticesAChangedServeArgument(t *testing.T) {
	r := twoSparkRecipe(t)
	placement := Placement{Role: RoleHead, NodeName: "spark-head", NodeCount: 2, MasterAddress: "192.168.99.20", MasterPort: 29501}
	previous := r
	previousVLLM := *r.Service.VLLM
	previousVLLM.GPUMemoryUtil = "0.81"
	previous.Service.VLLM = &previousVLLM

	drift := staleCommand(ContainerState{Command: vllmArgs(previous, placement)}, r, placement)
	if len(drift) != 1 || drift[0].Flag != "--gpu-memory-utilization" || drift[0].Actual != "0.81" || drift[0].Expected != r.Service.VLLM.GPUMemoryUtil {
		t.Fatalf("drift=%#v, want the changed memory-utilization flag", drift)
	}
	receipt := commandMismatchReceipt(drift)
	if len(receipt) != 1 || receipt[0]["flag"] != "--gpu-memory-utilization" || receipt[0]["was"] != "0.81" || receipt[0]["now"] != r.Service.VLLM.GPUMemoryUtil {
		t.Fatalf("receipt=%#v, want the changed flag and both values", receipt)
	}
	if drift := staleCommand(ContainerState{Command: vllmArgs(r, placement)}, r, placement); len(drift) != 0 {
		t.Fatalf("the expected serve command was called stale: %#v", drift)
	}

	// There is no semantic argv normalization in this package. The builders
	// guarantee one order and Docker preserves it, so even an equivalent
	// reordering is drift rather than a new compatibility promise.
	reordered := append([]string(nil), vllmArgs(r, placement)...)
	reordered[2], reordered[4] = reordered[4], reordered[2]
	reordered[3], reordered[5] = reordered[5], reordered[3]
	if drift := staleCommand(ContainerState{Command: reordered}, r, placement); len(drift) == 0 {
		t.Fatal("a reordered command was silently normalized")
	}
}

// The full command comparison delegates placement-resolved flags to
// staleLaunch. Repeating reconciliation with the same live fabric address
// must therefore leave both vLLM ranks alone instead of rebuilding forever.
func TestCommandReconciliationDoesNotFlapWithResolvedFabricAddress(t *testing.T) {
	r := twoSparkRecipe(t)
	placements := []Placement{
		{Role: RoleHead, NodeName: "spark-head", NodeCount: 2, MasterAddress: "192.168.99.20", MasterPort: 29501},
		{Role: RoleWorker, NodeName: "spark-worker", NodeCount: 2, MasterAddress: "192.168.99.20", MasterPort: 29501},
	}
	for _, placement := range placements {
		state := ContainerState{Command: vllmArgs(r, placement)}
		for reconciliation := 0; reconciliation < 3; reconciliation++ {
			if drift := staleCommand(state, r, placement); len(drift) != 0 {
				t.Fatalf("%s command drift on reconciliation %d: %#v", placement.Role, reconciliation, drift)
			}
			if drift := staleLaunch(state, r, placement); len(drift) != 0 {
				t.Fatalf("%s rendezvous drift on reconciliation %d: %#v", placement.Role, reconciliation, drift)
			}
		}
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
	// This fake applies the filter the way Docker does, which is the whole
	// point: it ANDs the values inside one label filter. A fake that ignores
	// the filter and returns everything let a query for both namespaces at
	// once pass here while matching nothing on a real machine.
	currentJSON := `{"Names":["/basement-qwen-v1"],"State":"running","Labels":{"ai.basement.managed":"true","ai.basement.recipe-id":"qwen","ai.basement.recipe-version":"1","ai.basement.host-port":"8100"}}`
	legacyJSON := `{"Names":["/runonspark-laguna-v3"],"State":"exited","Labels":{"ai.runonspark.managed":"true","ai.runonspark.recipe-id":"laguna","ai.runonspark.recipe-version":"3"}}`
	requested := []string{}
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		if !strings.Contains(request.URL.Path, "/containers/json") {
			t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL)
		}
		filters := request.URL.Query().Get("filters")
		requested = append(requested, filters)
		matched := []string{}
		wantsCurrent := strings.Contains(filters, "ai.basement.managed=true")
		wantsLegacy := strings.Contains(filters, "ai.runonspark.managed=true")
		if wantsCurrent && !wantsLegacy {
			matched = append(matched, currentJSON)
		}
		if wantsLegacy && !wantsCurrent {
			matched = append(matched, legacyJSON)
		}
		body := "[" + strings.Join(matched, ",") + "]"
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	containers, err := client.ManagedContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, filters := range requested {
		if strings.Contains(filters, "ai.basement.managed=true") && strings.Contains(filters, "ai.runonspark.managed=true") {
			t.Fatalf("one query asked for both label namespaces, which Docker ANDs and no container satisfies: %s", filters)
		}
	}
	if len(containers) != 2 {
		t.Fatalf("got %d containers, want 2: %#v", len(containers), containers)
	}
	byName := map[string]ManagedContainer{}
	for _, c := range containers {
		byName[c.Name] = c
	}
	current, ok := byName["basement-qwen-v1"]
	if !ok || current.RecipeID != "qwen" || current.Version != "1" || !current.Running || current.HostPort != 8100 {
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
	// Pacing is asserted separately; here every event has to be observed for
	// the aggregate to be checked at all.
	previousInterval := pullProgressInterval
	t.Cleanup(func() { pullProgressInterval = previousInterval })
	pullProgressInterval = 0
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

// TestPullPacesProgressWithoutHidingPhaseChanges guards the cost side of
// live image progress: Docker emits an event per layer per chunk, and every
// reported receipt costs a free-space check and a database write on the node
// running the pull.
func TestPullPacesProgressWithoutHidingPhaseChanges(t *testing.T) {
	events := []string{
		`{"status":"Downloading","id":"aaa","progressDetail":{"current":100,"total":1000}}`,
		`{"status":"Downloading","id":"aaa","progressDetail":{"current":300,"total":1000}}`,
		`{"status":"Downloading","id":"aaa","progressDetail":{"current":600,"total":1000}}`,
		`{"status":"Downloading","id":"aaa","progressDetail":{"current":1000,"total":1000}}`,
		`{"status":"Pull complete","id":"aaa","progressDetail":{}}`,
	}
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Join(events, "\n")))}, nil
	})}}
	var statuses []string
	if _, err := client.Pull(context.Background(), "vllm/vllm-openai@sha256:abc", func(update any) error {
		statuses = append(statuses, update.(map[string]any)["status"].(string))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// One opening report, then nothing until unpacking starts: the four
	// download events land inside a single interval.
	if len(statuses) != 2 || statuses[0] != "Downloading" || statuses[1] != "Extracting" {
		t.Fatalf("image pull reported %v, want the first download then the phase change", statuses)
	}
}
