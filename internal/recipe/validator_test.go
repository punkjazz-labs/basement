package recipe

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestBuiltinRecipePackIsPinnedCandidate(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(recipes) != 9 {
		t.Fatalf("got %d recipes, want 9", len(recipes))
	}
	for _, r := range recipes {
		if r.Verification != "candidate" || r.Trust != "basement-candidate" {
			t.Fatalf("recipe falsely verified: %#v", r)
		}
	}
	qwen35, ok := Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok || qwen35.Runtime.Reference() != "ghcr.io/miaai-lab/mia-vllm-gb10-linear-b12x@sha256:19627342e1da2607f4db50745dca30e57d7dd0ebff06062f03fd69b43a252931" {
		t.Fatalf("Qwen 35 runtime is not pinned: %#v", qwen35.Runtime)
	}
	if got := qwen35.Artifacts[0].Revision; got != "739af1e7aac320af1682ed1e0cce369af4c5265d" {
		t.Fatalf("Qwen 35 model revision is not pinned: %s", got)
	}
	qwen27, ok := Find(recipes, "qwen36-27b-nvfp4-1s")
	if !ok || qwen27.Artifacts[0].Repository != "nvidia/Qwen3.6-27B-NVFP4" || qwen27.TotalArtifactBytes() != 21941623844 {
		t.Fatalf("unexpected Qwen 27 recipe: %#v", qwen27)
	}
	laguna, ok := Find(recipes, "laguna-s-2-1-nvfp4-dflash-1s")
	if !ok || len(laguna.Artifacts) != 2 || laguna.TotalArtifactBytes() != 74168605863 || laguna.Service.VLLM.SpeculativeModelRole != "drafter" {
		t.Fatalf("unexpected Laguna recipe: %#v", laguna)
	}
	// The pack's first SGLang recipe, and the first two-Spark one that is not
	// vLLM: 170.8 GB of weights that do not fit a single Spark, sharded at
	// TP=2, with no drafter (the DSpark artifact declares no licence).
	inkling, ok := Find(recipes, "inkling-small-nvfp4-2s")
	if !ok || inkling.Runtime.Kind != "sglang" || !inkling.Distributed() || inkling.Topology.SparkCount != 2 {
		t.Fatalf("unexpected Inkling recipe: %#v", inkling)
	}
	if inkling.Runtime.Reference() != "lmsysorg/sglang@sha256:7d3617a95c2d09b233dc6eb654ed746eaa1f69903b1012c2bedc13732d2e3590" {
		t.Fatalf("Inkling runtime is not pinned: %#v", inkling.Runtime)
	}
	if len(inkling.Artifacts) != 1 || inkling.TotalArtifactBytes() != 170764923366 || inkling.Artifacts[0].Revision != "b6a99534467840620d411e4cd4ad5819b2610d9c" {
		t.Fatalf("Inkling weights are not pinned: %#v", inkling.Artifacts)
	}
	if inkling.Service.SGLang.TensorParallelSize != 2 || inkling.Service.SGLang.SpeculativeAlgorithm != "" || inkling.Service.SGLang.SpeculativeModelRole != "" {
		t.Fatalf("unexpected Inkling serve configuration: %#v", inkling.Service.SGLang)
	}
	if inkling.MemoryModel != nil {
		t.Fatalf("a TP=2 recipe must not carry the flat single-node memory model: %#v", inkling.MemoryModel)
	}
	// The pack's first media recipe uses ComfyUI's own port and pins only the
	// four files its text-to-video graph loads. It remains a candidate until
	// the complete install lifecycle runs on hardware.
	h3, ok := Find(recipes, "minimax-h3-comfyui-1s")
	if !ok || h3.Runtime.Kind != "comfyui" || h3.Distributed() {
		t.Fatalf("unexpected MiniMax H3 recipe: %#v", h3)
	}
	if h3.Runtime.Reference() != "ghcr.io/punkjazz-labs/basement-comfyui@sha256:8e6715f3e133c03b12f7730c4d66124554952bf9dae81263a153be05f96d23a9" {
		t.Fatalf("MiniMax H3 runtime is not pinned: %#v", h3.Runtime)
	}
	if len(h3.Artifacts) != 1 || len(h3.Artifacts[0].Files) != 4 || h3.TotalArtifactBytes() != 42470585471 {
		t.Fatalf("MiniMax H3 weights are not the four-file optimized set: %#v", h3.Artifacts)
	}
	wantFiles := []ArtifactFile{
		{Name: "diffusion_models/minimax_h3_fl2va_pruned_int8_convrot.safetensors", ExpectedBytes: 20970379616},
		{Name: "text_encoders/qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors", ExpectedBytes: 15687142551},
		{Name: "vae/minimax_h3_video_vae_fp16.safetensors", ExpectedBytes: 5207808496},
		{Name: "vae/minimax_h3_audio_vae_fp32.safetensors", ExpectedBytes: 605254808},
	}
	for index, want := range wantFiles {
		if got := h3.Artifacts[0].Files[index]; got != want {
			t.Errorf("MiniMax H3 file %d=%#v, want %#v", index, got, want)
		}
	}
	if h3.Runtime.ImageBytes != 5632799027 || h3.Runtime.ImageDiskBytes != 9246461463 {
		t.Fatalf("MiniMax H3 image sizes are not the registry and Docker measurements: %#v", h3.Runtime)
	}
	if h3.RequiredBytes() != 71717046934 {
		t.Fatalf("MiniMax H3 requires %d disk bytes, want artifact plus expanded image plus safety margin", h3.RequiredBytes())
	}
	if h3.Service.InternalPort != 8188 || h3.Service.DefaultHostPort != 8188 {
		t.Fatalf("MiniMax H3 does not use ComfyUI's port: %#v", h3.Service)
	}
	config, media := h3.MediaGeneration()
	if !media || len(config.Graphs) != 1 || config.Graphs[ModeTextToVideo] != "minimax-h3-t2v.json" {
		t.Fatalf("MiniMax H3 does not expose only its reachable text-to-video graph: %#v", config.Graphs)
	}
	if config.DefaultShortEdge != 768 || config.MaxShortEdge != 1440 || config.MaxLongEdge != 2560 {
		t.Fatalf("MiniMax H3 canvas=%#v, want the measured default and QHD cap", config)
	}
	if config.MinBlocks != 7 || config.DefaultBlocks != 7 || config.MaxBlocks != 21 || config.Frames(7) != 124 || config.Frames(21) != 362 {
		t.Fatalf("MiniMax H3 duration grid=%#v", config)
	}
	if config.ConcurrentGenerations != 1 || h3.Topology.SparkCount != 1 {
		t.Fatalf("MiniMax H3 concurrency or topology is not single-device: config=%#v topology=%#v", config, h3.Topology)
	}
	// Wall times measured in docs/H3-MEASUREMENTS.md for a 5.17 s clip: 1061 s
	// at 768, 3021 s at 1088, 7750 s at 1440. Factors are those times divided
	// by the smallest one.
	wantSizeWaits := []SizeWait{
		{ShortEdge: 768, Factor: 1},
		{ShortEdge: 1088, Factor: 2.85},
		{ShortEdge: 1440, Factor: 7.3},
	}
	if !reflect.DeepEqual(config.SizeWaits, wantSizeWaits) {
		t.Fatalf("MiniMax H3 size_waits=%#v, want %#v", config.SizeWaits, wantSizeWaits)
	}
	if h3.Runtime.StartTimeoutMinutes != 0 {
		t.Fatalf("MiniMax H3 startup timeout=%d minutes, want the product's 20-minute health-wait default", h3.Runtime.StartTimeoutMinutes)
	}
	planned, ok := h3.PlannedMemoryBytes()
	if !ok || planned != 99090000000 {
		t.Fatalf("MiniMax H3 planned memory is %d,%v, want the measured 99.09 GB", planned, ok)
	}
	// The pack's first llama.cpp recipe, and the first artifact that pins
	// individual files instead of a whole snapshot: one quantization out of a
	// repository that publishes fourteen of them.
	flash3bit, ok := Find(recipes, "deepseek-v4-flash-0731-ud-iq3-xxs-1s")
	if !ok || flash3bit.Runtime.Kind != "llamacpp" || flash3bit.Distributed() {
		t.Fatalf("unexpected DeepSeek 3-bit recipe: %#v", flash3bit)
	}
	if flash3bit.Runtime.Reference() != "ghcr.io/ggml-org/llama.cpp@sha256:866ad568474de9e835e487ae841ad6ace1a494b5eab4f292cbd45adb6180f711" {
		t.Fatalf("DeepSeek 3-bit runtime is not pinned: %#v", flash3bit.Runtime)
	}
	if len(flash3bit.Artifacts) != 1 || flash3bit.TotalArtifactBytes() != 104207848032 || flash3bit.Artifacts[0].Revision != "57326b941c4603e24d1a5e71c22520c66e086eb8" {
		t.Fatalf("DeepSeek 3-bit weights are not pinned: %#v", flash3bit.Artifacts)
	}
	if len(flash3bit.Artifacts[0].Files) != 4 {
		t.Fatalf("DeepSeek 3-bit must pin its four shards, not a whole snapshot: %#v", flash3bit.Artifacts[0].Files)
	}
	// The recipe serves the first shard by name, and that name has to be one
	// of the files the download actually fetches.
	if flash3bit.Service.LlamaCpp.ModelFile != flash3bit.Artifacts[0].Files[0].Name {
		t.Fatalf("DeepSeek 3-bit serves %q, which is not its first pinned shard", flash3bit.Service.LlamaCpp.ModelFile)
	}
	planned, ok = flash3bit.PlannedMemoryBytes()
	if !ok || planned+flash3bit.Requirements.MemoryReserveBytes > flash3bit.Requirements.MinimumMemoryBytes {
		t.Fatalf("DeepSeek 3-bit plans %d bytes, which does not leave its own reserve", planned)
	}
}

func TestRecipePolicyRejectsUnsafeVariants(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base, ok := Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("Qwen 35 recipe missing")
	}
	tests := []struct {
		name   string
		mutate func(*Recipe)
		want   string
	}{
		{"floating image", func(r *Recipe) { r.Runtime.Image = "vllm/vllm-openai:nightly" }, "without a tag"},
		{"missing digest", func(r *Recipe) { r.Runtime.Digest = "sha256:REQUIRED" }, "immutable sha256"},
		{"mutable revision", func(r *Recipe) { r.Artifacts[0].Revision = "main" }, "40-character immutable"},
		{"unknown operation", func(r *Recipe) { r.Operations = append(r.Operations, Operation{Type: "run_shell"}) }, "unknown operation"},
		{"missing inference gate", func(r *Recipe) { r.Operations = r.Operations[:len(r.Operations)-1] }, "complete verified lifecycle"},
		{"unsafe repository", func(r *Recipe) { r.Artifacts[0].Repository = "../../etc/passwd" }, "role/repository"},
		{"undeclared secret", func(r *Recipe) { r.Requirements.Secrets = []string{"AWS_SECRET_ACCESS_KEY"} }, "allowlisted"},
		{"false verification", func(r *Recipe) { r.Trust = "basement-verified" }, "real DGX verification"},
		{"mutable source", func(r *Recipe) { r.Source.Revision = "main" }, "immutable revision"},
		{"unsafe environment", func(r *Recipe) { r.Runtime.Environment["LD_PRELOAD"] = "/tmp/hook.so" }, "allowlist"},
		{"compressed-only image budget", func(r *Recipe) { r.Runtime.ImageDiskBytes = r.Runtime.ImageBytes - 1 }, "expanded image storage"},
		{"missing memory reserve", func(r *Recipe) { r.Requirements.MemoryReserveBytes = 0 }, "per-node memory"},
		{"memory overcommit", func(r *Recipe) { r.Requirements.MemoryReserveBytes = 30_000_000_000 }, "does not preserve"},
		{"licence url for another repository", func(r *Recipe) {
			r.Artifacts[0].LicenceURL = "https://huggingface.co/someone-else/Other-Model/blob/main/LICENSE.md"
		}, "artifact's own repository"},
		{"empty territory exclusions list", func(r *Recipe) {
			r.Artifacts[0].LicenceTerritoryExclusions = []string{}
		}, "licence_territory_exclusions must be a non-empty list"},
		{"blank territory exclusion entry", func(r *Recipe) {
			r.Artifacts[0].LicenceTerritoryExclusions = []string{"European Union", "   "}
		}, "licence_territory_exclusions must be a non-empty list"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.Artifacts = append([]Artifact(nil), base.Artifacts...)
			candidate.Operations = append([]Operation(nil), base.Operations...)
			candidate.Requirements.Secrets = append([]string(nil), base.Requirements.Secrets...)
			candidate.Runtime.Environment = make(map[string]string, len(base.Runtime.Environment))
			for name, value := range base.Runtime.Environment {
				candidate.Runtime.Environment[name] = value
			}
			test.mutate(&candidate)
			err := Validate(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate()=%v, want error containing %q", err, test.want)
			}
		})
	}
}

// TestLicenceTerritoryExclusionsAcceptedAndTracked proves the field is
// accepted when it is a well-formed, non-empty list of non-blank strings,
// that an absent field leaves a recipe unaffected (the shipped pack has no
// such field yet and still validates), and that RequiresTerritoryConfirmation
// answers correctly in both cases.
func TestLicenceTerritoryExclusionsAcceptedAndTracked(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base, ok := Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("Qwen 35 recipe missing")
	}
	if base.RequiresTerritoryConfirmation() {
		t.Fatalf("a recipe with no licence_territory_exclusions must not require territory confirmation: %#v", base.Artifacts)
	}

	candidate := base
	candidate.Artifacts = append([]Artifact(nil), base.Artifacts...)
	candidate.Operations = append([]Operation(nil), base.Operations...)
	candidate.Requirements.Secrets = append([]string(nil), base.Requirements.Secrets...)
	candidate.Runtime.Environment = make(map[string]string, len(base.Runtime.Environment))
	for name, value := range base.Runtime.Environment {
		candidate.Runtime.Environment[name] = value
	}
	candidate.Artifacts[0].LicenceTerritoryExclusions = []string{
		"European Union", "United Kingdom", "Republic of Korea", "United States of America",
	}
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate() with a well-formed licence_territory_exclusions list = %v, want nil", err)
	}
	if !candidate.RequiresTerritoryConfirmation() {
		t.Fatal("a recipe whose artifact carries licence_territory_exclusions must require territory confirmation")
	}
}

func TestUpstreamLicenceRepository(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base, ok := Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("Qwen 35 recipe missing")
	}

	const (
		repository = "MiniMaxAI/MiniMax-H3"
		revision   = "fa9c8ab1eaa21c8ae25e7e40b83b2e6002f340af"
		licenceURL = "https://huggingface.co/MiniMaxAI/MiniMax-H3/blob/fa9c8ab1eaa21c8ae25e7e40b83b2e6002f340af/LICENSE"
	)
	clone := func() Recipe {
		candidate := base
		candidate.Artifacts = append([]Artifact(nil), base.Artifacts...)
		return candidate
	}

	t.Run("existing recipe", func(t *testing.T) {
		if err := Validate(clone()); err != nil {
			t.Fatalf("Validate() for a recipe without upstream licence fields = %v, want nil", err)
		}
	})
	t.Run("pinned upstream licence", func(t *testing.T) {
		candidate := clone()
		candidate.Artifacts[0].LicenceRepository = repository
		candidate.Artifacts[0].LicenceRevision = revision
		candidate.Artifacts[0].LicenceURL = licenceURL
		if err := Validate(candidate); err != nil {
			t.Fatalf("Validate() with a pinned upstream licence = %v, want nil", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*Artifact)
		want   string
	}{
		{"repository without revision", func(artifact *Artifact) {
			artifact.LicenceRepository = repository
			artifact.LicenceURL = licenceURL
		}, "licence_repository and licence_revision must be set together"},
		{"mutable licence revision", func(artifact *Artifact) {
			artifact.LicenceRepository = repository
			artifact.LicenceRevision = "main"
			artifact.LicenceURL = "https://huggingface.co/MiniMaxAI/MiniMax-H3/blob/main/LICENSE"
		}, "licence_revision must be a 40-character immutable commit"},
		{"licence URL at main", func(artifact *Artifact) {
			artifact.LicenceRepository = repository
			artifact.LicenceRevision = revision
			artifact.LicenceURL = "https://huggingface.co/MiniMaxAI/MiniMax-H3/blob/main/LICENSE"
		}, "licence_url must reference licence_repository at licence_revision"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := clone()
			test.mutate(&candidate.Artifacts[0])
			err := Validate(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate()=%v, want error containing %q", err, test.want)
			}
		})
	}
}

// sglangCandidate is a pinned vLLM recipe re-pointed at SGLang. The shipped
// SGLang recipe is two-Spark, so the single-node side of the schema is
// exercised against a recipe whose every other field is already policy-clean.
func sglangCandidate(t *testing.T) Recipe {
	t.Helper()
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base, ok := Find(recipes, "qwen36-27b-nvfp4-1s")
	if !ok {
		t.Fatal("Qwen 27 recipe missing")
	}
	base.Runtime.Kind = "sglang"
	base.Service.VLLM = nil
	base.Service.SGLang = &SGLangConfig{
		TensorParallelSize: 1, MemFractionStatic: "0.85", ContextLength: 262144, MaxRunningRequests: 8,
		Quantization: "modelopt_fp4", KVCacheDType: "fp8_e4m3", AttentionBackend: "flashinfer",
		ReasoningParser: "qwen3", ToolCallParser: "qwen3_coder",
	}
	return base
}

func TestValidateAcceptsMinimalSGLangRecipe(t *testing.T) {
	candidate := sglangCandidate(t)
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate()=%v, want nil", err)
	}
	// Only the four command-line essentials are mandatory; everything else
	// may be left unset so the runtime's own default applies.
	candidate.Service.SGLang = &SGLangConfig{TensorParallelSize: 1, MemFractionStatic: "0.85", ContextLength: 8192, MaxRunningRequests: 1}
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate() on a bare sglang block=%v, want nil", err)
	}
}

func TestValidateEnforcesOneRuntimeBlockMatchingTheKind(t *testing.T) {
	vllmBlock := func() *VLLMConfig {
		recipes, err := Builtin()
		if err != nil {
			t.Fatal(err)
		}
		base, _ := Find(recipes, "qwen36-27b-nvfp4-1s")
		return base.Service.VLLM
	}
	tests := []struct {
		name   string
		mutate func(*Recipe)
		want   string
	}{
		{"kind sglang with a vllm block", func(r *Recipe) {
			r.Service.SGLang = nil
			r.Service.VLLM = vllmBlock()
		}, "runtime kind sglang requires service.sglang, but the recipe declares service.vllm"},
		{"both blocks", func(r *Recipe) { r.Service.VLLM = vllmBlock() }, "keep only the block that matches runtime.kind"},
		{"neither block", func(r *Recipe) { r.Service.SGLang = nil }, "service must declare exactly one runtime block"},
		{"unknown kind", func(r *Recipe) { r.Runtime.Kind = "tensorrt" }, "runtime kind must be one of: comfyui, llamacpp, sglang, vllm"},
		{"draft tokens without an algorithm", func(r *Recipe) { r.Service.SGLang.SpeculativeNumDraftTokens = 4 }, "require speculative_algorithm"},
		{"draft model that is not a declared role", func(r *Recipe) {
			r.Service.SGLang.SpeculativeAlgorithm = "EAGLE3"
			r.Service.SGLang.SpeculativeNumDraftTokens = 4
			r.Service.SGLang.SpeculativeModelRole = "primary"
		}, "non-primary artifact role"},
		{"quantization outside policy", func(r *Recipe) { r.Service.SGLang.Quantization = "gguf" }, "outside the recipe policy"},
		{"memory fraction overcommit", func(r *Recipe) { r.Service.SGLang.MemFractionStatic = "0.95" }, "does not preserve the per-node memory reserve"},
		{"unsafe chat template", func(r *Recipe) { r.Service.SGLang.ChatTemplateFile = "../../etc/passwd" }, "chat template file is unsafe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := sglangCandidate(t)
			block := *candidate.Service.SGLang
			candidate.Service.SGLang = &block
			test.mutate(&candidate)
			err := Validate(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate()=%v, want error containing %q", err, test.want)
			}
		})
	}
}

// llamaCppCandidate is the shipped llama.cpp recipe with its runtime block
// and artifact copied, so a subtest can mutate either without the mutation
// leaking into the next one.
func llamaCppCandidate(t *testing.T) Recipe {
	t.Helper()
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base, ok := Find(recipes, "deepseek-v4-flash-0731-ud-iq3-xxs-1s")
	if !ok {
		t.Fatal("DeepSeek 3-bit recipe missing")
	}
	block := *base.Service.LlamaCpp
	base.Service.LlamaCpp = &block
	model := *base.MemoryModel
	base.MemoryModel = &model
	base.Artifacts = append([]Artifact(nil), base.Artifacts...)
	base.Artifacts[0].Files = append([]ArtifactFile(nil), base.Artifacts[0].Files...)
	return base
}

func TestValidateRejectsUnsafeLlamaCppVariants(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Recipe)
		want   string
	}{
		// llama.cpp can spread a model across machines with its RPC backend,
		// but nothing here builds those ranks. A recipe that asked for two
		// Sparks would run on one and silently be a different model.
		{"two Sparks", func(r *Recipe) {
			r.Topology.SparkCount = 2
			r.Topology.Interconnect = &Interconnect{
				Kind: "connectx7", MasterPort: 29501,
				SharedEnvironment: map[string]string{"NCCL_SOCKET_IFNAME": "enp1s0f0np0"},
			}
		}, "llama.cpp recipes run on a single Spark"},
		{"model file the artifact does not pin", func(r *Recipe) {
			r.Service.LlamaCpp.ModelFile = "UD-IQ4_XS/DeepSeek-V4-Flash-0731-UD-IQ4_XS-00001-of-00004.gguf"
		}, "is not one of the files the primary artifact pins"},
		{"artifact pins no files at all", func(r *Recipe) { r.Artifacts[0].Files = nil }, "is not one of the files the primary artifact pins"},
		{"model file climbing out of the mount", func(r *Recipe) { r.Service.LlamaCpp.ModelFile = "../../etc/passwd" }, "model_file is unsafe"},
		{"model file that is not a GGUF", func(r *Recipe) { r.Service.LlamaCpp.ModelFile = "README.md" }, "must name a .gguf file"},
		{"no context", func(r *Recipe) { r.Service.LlamaCpp.ContextSize = 0 }, "context_size must be positive"},
		{"no slots", func(r *Recipe) { r.Service.LlamaCpp.Parallel = 0 }, "parallel must be between 1 and 64"},
		{"invented flash attention value", func(r *Recipe) { r.Service.LlamaCpp.FlashAttention = "yes" }, "flash_attention must be one of"},
		{"unsafe chat template", func(r *Recipe) { r.Service.LlamaCpp.ChatTemplateFile = "../../etc/passwd" }, "chat template file is unsafe"},
		// A safe path is not the same as a path that will exist. When the
		// artifact pins files, an unpinned template is never downloaded, and
		// a file left there by hand would be read without ever being
		// verified against anything.
		{"chat template nothing downloads", func(r *Recipe) { r.Service.LlamaCpp.ChatTemplateFile = "chat_template.jinja" }, "nothing will download it"},
		// llama.cpp claims an absolute footprint, so without a memory model
		// there is no number to check the guardrail against at all.
		{"no memory model", func(r *Recipe) { r.MemoryModel = nil }, "must declare memory_model"},
		{"weights that are not the pinned weights", func(r *Recipe) { r.MemoryModel.WeightsBytes = 1 }, "must equal the primary artifact's expected_bytes"},
		{"context the machine cannot hold", func(r *Recipe) { r.Service.LlamaCpp.ContextSize = 1 << 20 }, "does not preserve the per-node memory reserve"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := llamaCppCandidate(t)
			test.mutate(&candidate)
			err := Validate(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate()=%v, want error containing %q", err, test.want)
			}
		})
	}
}

// The reachability rule follows the artifact, not the runtime kind: any
// recipe whose primary artifact pins files must pin its chat template too,
// and a recipe pinning no files keeps meaning the whole snapshot.
func TestChatTemplateMustBeReachableWhateverTheRuntime(t *testing.T) {
	pinned := llamaCppCandidate(t)
	pinned.Artifacts[0].Files = append(pinned.Artifacts[0].Files, ArtifactFile{Name: "chat_template.jinja", ExpectedBytes: 1024})
	pinned.Artifacts[0].ExpectedBytes += 1024
	// weights_bytes tracks the artifact's own total, template included.
	pinned.MemoryModel.WeightsBytes = pinned.Artifacts[0].ExpectedBytes
	pinned.Service.LlamaCpp.ChatTemplateFile = "chat_template.jinja"
	if err := Validate(pinned); err != nil {
		t.Fatalf("Validate() on a pinned template=%v, want nil", err)
	}
	// The same template name on an SGLang recipe whose artifact pins nothing
	// is still fine: that artifact downloads the whole repository.
	snapshot := sglangCandidate(t)
	block := *snapshot.Service.SGLang
	block.ChatTemplateFile = "chat_template.jinja"
	snapshot.Service.SGLang = &block
	if len(snapshot.Artifacts[0].Files) != 0 {
		t.Fatal("this test needs a whole-snapshot artifact")
	}
	if err := Validate(snapshot); err != nil {
		t.Fatalf("Validate() on a whole-snapshot template=%v, want nil", err)
	}
	// Give that same recipe a pinned file list that omits the template, and
	// it must be refused.
	snapshot.Artifacts = append([]Artifact(nil), snapshot.Artifacts...)
	snapshot.Artifacts[0].Files = []ArtifactFile{{Name: "weights.safetensors", ExpectedBytes: snapshot.Artifacts[0].ExpectedBytes}}
	if err := Validate(snapshot); err == nil || !strings.Contains(err.Error(), "nothing will download it") {
		t.Fatalf("Validate()=%v, want an unreachable-template error", err)
	}
}

// A recipe is data this process did not write. Multiplying an enormous
// kv_bytes_per_token by an enormous context wraps int64 to a small positive
// number, and a small positive number is what an OOM guardrail waves
// through, so the wrap has to be caught rather than trusted.
func TestLlamaCppMemoryArithmeticRefusesToOverflow(t *testing.T) {
	candidate := llamaCppCandidate(t)
	candidate.MemoryModel.KVBytesPerToken = 1 << 40
	candidate.Service.LlamaCpp.ContextSize = 1 << 30
	if planned, ok := candidate.PlannedMemoryBytes(); ok {
		t.Fatalf("PlannedMemoryBytes()=%d, true; want the overflow reported", planned)
	}
	err := Validate(candidate)
	if err == nil || !strings.Contains(err.Error(), "too large to represent") {
		t.Fatalf("Validate()=%v, want an overflow refusal", err)
	}
	// The wrapped product is exactly zero here, which a guardrail would have
	// read as a model that needs no memory at all.
	kv, context := candidate.MemoryModel.KVBytesPerToken, int64(candidate.Service.LlamaCpp.ContextSize)
	if wrapped := kv * context; wrapped >= candidate.Requirements.MinimumMemoryBytes {
		t.Fatalf("this test no longer demonstrates a wrap: product=%d", wrapped)
	}
	// Overflow on the final addition is caught the same way.
	overshoot := llamaCppCandidate(t)
	overshoot.MemoryModel.RuntimeOverheadBytes = math.MaxInt64
	if _, ok := overshoot.PlannedMemoryBytes(); ok {
		t.Fatal("an overflowing overhead was accepted")
	}
}

// Per-file pinning has to account for every byte the artifact claims, or the
// disk guardrail and the download would be measuring different things.
func TestValidateRejectsIncoherentArtifactFilePinning(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Recipe)
		want   string
	}{
		{"sizes that do not add up", func(r *Recipe) { r.Artifacts[0].Files[0].ExpectedBytes++ }, "but expected_bytes is"},
		{"a file listed twice", func(r *Recipe) {
			r.Artifacts[0].Files[1] = r.Artifacts[0].Files[0]
		}, "more than once"},
		{"a file with no size", func(r *Recipe) { r.Artifacts[0].Files[0].ExpectedBytes = 0 }, "must declare positive expected_bytes"},
		{"a file escaping the artifact", func(r *Recipe) { r.Artifacts[0].Files[0].Name = "../../etc/passwd" }, "file name is unsafe"},
		{"an absolute file name", func(r *Recipe) { r.Artifacts[0].Files[0].Name = "/etc/passwd" }, "file name is unsafe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := llamaCppCandidate(t)
			test.mutate(&candidate)
			err := Validate(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate()=%v, want error containing %q", err, test.want)
			}
		})
	}
}

// An artifact that pins no files keeps meaning the whole snapshot, so every
// runtime that does not require a file selection still validates untouched.
func TestWholeSnapshotArtifactsStayValidWithoutFilePinning(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recipes {
		for _, artifact := range r.Artifacts {
			if r.Runtime.Kind == "llamacpp" || r.Runtime.Kind == "comfyui" {
				continue
			}
			if len(artifact.Files) != 0 {
				t.Fatalf("%s pins files on a whole-snapshot runtime: %#v", r.ID, artifact.Files)
			}
		}
		if err := Validate(r); err != nil {
			t.Fatalf("Validate(%s)=%v", r.ID, err)
		}
	}
}

// writablePathCandidate is a pinned recipe with room for writable paths. The
// base is policy-clean in every other respect, so a subtest's failure is
// always about the path it set.
func writablePathCandidate(t *testing.T, id string) Recipe {
	t.Helper()
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base, ok := Find(recipes, id)
	if !ok {
		t.Fatalf("%s recipe missing", id)
	}
	base.Runtime.WritablePaths = nil
	return base
}

// A writable path is the only part of the schema that changes what the
// container's filesystem means, so it is held to the layout the manager owns.
// A tmpfs over /model would hide the weights behind an empty directory and the
// runtime would fail inside its own argument validation; one over /root/.cache
// would throw the compilation cache away on every restart.
func TestValidateRejectsWritablePathsOutsideTheManagedLayout(t *testing.T) {
	tests := []struct {
		name  string
		id    string
		paths []string
		want  string
	}{
		{"the container root", "qwen36-35b-a3b-nvfp4-1s", []string{"/"}, "must not be the container root"},
		{"a relative path", "qwen36-35b-a3b-nvfp4-1s", []string{"root/.tilelang"}, "must be an absolute container path"},
		{"a trailing slash", "qwen36-35b-a3b-nvfp4-1s", []string{"/root/.tilelang/"}, "must be an absolute container path"},
		{"a climbing segment", "qwen36-35b-a3b-nvfp4-1s", []string{"/root/../etc"}, "must be an absolute container path"},
		{"a mount option smuggled into the path", "qwen36-35b-a3b-nvfp4-1s", []string{"/root/.tilelang,size=512g"}, "must be an absolute container path"},
		{"a path longer than the limit", "qwen36-35b-a3b-nvfp4-1s", []string{"/" + strings.Repeat("a", maxWritablePathLength)}, "must be an absolute container path"},
		{"the scratch tmpfs", "qwen36-35b-a3b-nvfp4-1s", []string{"/tmp"}, "collides with the container's /tmp mount"},
		{"a directory inside the scratch tmpfs", "qwen36-35b-a3b-nvfp4-1s", []string{"/tmp/kernels"}, "collides with the container's /tmp mount"},
		{"the model mount", "qwen36-35b-a3b-nvfp4-1s", []string{"/model"}, "collides with the container's /model mount"},
		{"a directory inside the model mount", "qwen36-35b-a3b-nvfp4-1s", []string{"/model/kernels"}, "collides with the container's /model mount"},
		{"the compilation cache", "qwen36-35b-a3b-nvfp4-1s", []string{"/root/.cache"}, "collides with the container's /root/.cache mount"},
		{"a directory containing the compilation cache", "qwen36-35b-a3b-nvfp4-1s", []string{"/root"}, "collides with the container's /root/.cache mount"},
		// Every artifact role is a mount, not just the primary one.
		{"a second artifact's mount", "laguna-s-2-1-nvfp4-dflash-1s", []string{"/drafter"}, "collides with the container's /drafter mount"},
		{"the same path twice", "qwen36-35b-a3b-nvfp4-1s", []string{"/root/.tilelang", "/root/.tilelang"}, "declared more than once"},
		{"a path inside another declared path", "qwen36-35b-a3b-nvfp4-1s", []string{"/root/.tilelang", "/root/.tilelang/kernels"}, "is inside /root/.tilelang"},
		{"more paths than the limit", "qwen36-35b-a3b-nvfp4-1s", []string{"/a", "/b", "/c", "/d", "/e"}, "at most 4 paths"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := writablePathCandidate(t, test.id)
			candidate.Runtime.WritablePaths = test.paths
			err := Validate(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate()=%v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestValidateAcceptsWritableCachePaths(t *testing.T) {
	candidate := writablePathCandidate(t, "qwen36-35b-a3b-nvfp4-1s")
	candidate.Runtime.WritablePaths = []string{"/root/.tilelang", "/opt/kernel-cache"}
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate()=%v, want nil", err)
	}
	// A recipe that declares none keeps exactly the filesystem it had.
	candidate.Runtime.WritablePaths = nil
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate() without writable paths=%v, want nil", err)
	}
}

// vLLM builds DeepSeek V4 Flash's "mhc" kernels with tilelang, which writes
// its JIT cache to /root/.tilelang at import time and dies on the read-only
// root filesystem after the weights have loaded. No other recipe in the pack
// compiles kernels that way, and every one of them serves on the unchanged
// filesystem today, so none of them may quietly gain a writable mount.
func TestOnlyTheTwoSparkFlashRecipeDeclaresAWritablePath(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recipes {
		if r.ID == "deepseek-v4-flash-0731-2s" {
			want := []string{"/root/.tilelang", "/root/tmp"}
			if len(r.Runtime.WritablePaths) != len(want) || r.Runtime.WritablePaths[0] != want[0] || r.Runtime.WritablePaths[1] != want[1] {
				t.Fatalf("DeepSeek V4 Flash writable paths = %#v, want the tilelang kernel cache and its exec temp dir", r.Runtime.WritablePaths)
			}
			continue
		}
		if len(r.Runtime.WritablePaths) != 0 {
			t.Fatalf("%s gained writable paths: %#v", r.ID, r.Runtime.WritablePaths)
		}
	}
}

// The GB10 vLLM build the two-Spark DeepSeek V4 Flash recipe pins accepts
// values no other image in the pack does. Widening the policy for it must not
// widen it into a passthrough: each new field still has a closed set of
// answers, and the settings that only mean something together still have to
// arrive together.
func TestVLLMPolicyBoundsTheGB10RuntimeSettings(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base, ok := Find(recipes, "deepseek-v4-flash-0731-2s")
	if !ok {
		t.Fatal("DeepSeek V4 Flash two-Spark recipe missing")
	}
	tests := []struct {
		name   string
		mutate func(*VLLMConfig)
		want   string
	}{
		{"unknown kv cache dtype", func(v *VLLMConfig) { v.KVCacheDType = "nvfp4_anything" }, "outside the recipe policy"},
		{"unknown moe backend", func(v *VLLMConfig) { v.MoEBackend = "b12x_experimental" }, "outside the recipe policy"},
		{"unknown tokenizer mode", func(v *VLLMConfig) { v.TokenizerMode = "mistral" }, "outside the recipe policy"},
		{"unknown draft sample method", func(v *VLLMConfig) { v.SpeculativeDraftSampleMethod = "greedy" }, "outside the recipe policy"},
		{"unknown speculative method", func(v *VLLMConfig) { v.SpeculativeMethod = "eagle3" }, "outside the recipe policy"},
		{"block size vLLM does not take", func(v *VLLMConfig) { v.BlockSize = 200 }, "block_size must be unset"},
		{"capture size below the batch it must cover", func(v *VLLMConfig) { v.MaxCUDAGraphCaptureSize = v.MaxNumSeqs - 1 }, "max_cudagraph_capture_size"},
		{"unbounded capture ladder", func(v *VLLMConfig) { v.MaxCUDAGraphCaptureSize = 4096 }, "max_cudagraph_capture_size"},
		{"negative capture size", func(v *VLLMConfig) { v.MaxCUDAGraphCaptureSize = -1 }, "max_cudagraph_capture_size"},
		{"draft sampling without a method", func(v *VLLMConfig) {
			v.SpeculativeMethod, v.SpeculativeTokens = "", 0
		}, "require a speculative method"},
		{"DSpark pointed at a second model", func(v *VLLMConfig) { v.SpeculativeModelRole = "primary" }, "must not reference a separate speculative model"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			vllm := *base.Service.VLLM
			test.mutate(&vllm)
			candidate.Service.VLLM = &vllm
			err := Validate(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate()=%v, want error containing %q", err, test.want)
			}
		})
	}
}

// The runtime environment stays an allowlist of exact pairs. A GB10 arch list
// is only ever the GB10 target, and the B12X MoE switch can only be turned on.
func TestRuntimeEnvironmentAllowlistPinsValuesNotJustNames(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base, ok := Find(recipes, "deepseek-v4-flash-0731-2s")
	if !ok {
		t.Fatal("DeepSeek V4 Flash two-Spark recipe missing")
	}
	for name, value := range map[string]string{
		"TORCH_CUDA_ARCH_LIST":      "12.0+PTX",
		"FLASHINFER_CUDA_ARCH_LIST": "9.0a",
		"VLLM_USE_B12X_MOE":         "0",
	} {
		candidate := base
		candidate.Runtime.Environment = make(map[string]string, len(base.Runtime.Environment))
		for existing, current := range base.Runtime.Environment {
			candidate.Runtime.Environment[existing] = current
		}
		candidate.Runtime.Environment[name] = value
		err := Validate(candidate)
		if err == nil || !strings.Contains(err.Error(), "outside the allowlist") {
			t.Fatalf("%s=%s Validate()=%v, want an allowlist rejection", name, value, err)
		}
	}
}

func TestDecodeStrictRejectsUnknownField(t *testing.T) {
	_, err := DecodeStrict([]byte("schema_version: 1\nid: valid-recipe\nunexpected: true\n"))
	if err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("DecodeStrict()=%v", err)
	}
}
