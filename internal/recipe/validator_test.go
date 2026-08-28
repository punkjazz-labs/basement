package recipe

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// isPinnedRevision reports whether s is a full git commit hash. The pack test
// asserts this shape rather than exact hashes because feed-watch moves pins
// to new revisions on safe upstream bumps; what must never change is that
// every artifact is pinned to one.
func isPinnedRevision(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func TestBuiltinRecipePackIsPinnedCandidate(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(recipes) != 12 {
		t.Fatalf("got %d recipes, want 12", len(recipes))
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
	if !ok || len(laguna.Artifacts) != 2 || laguna.TotalArtifactBytes() == 0 || laguna.Service.VLLM.SpeculativeModelRole != "drafter" {
		t.Fatalf("unexpected Laguna recipe: %#v", laguna)
	}
	for _, artifact := range laguna.Artifacts {
		if !isPinnedRevision(artifact.Revision) {
			t.Fatalf("Laguna %s weights are not pinned: %#v", artifact.Role, artifact)
		}
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
	if len(inkling.Artifacts) != 1 || inkling.TotalArtifactBytes() == 0 || !isPinnedRevision(inkling.Artifacts[0].Revision) {
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
	// A floor, not an exact pin: version 3 is where image_to_video shipped,
	// and feed-watch moves versions upward on safe upstream pin bumps.
	if h3.Version < 3 {
		t.Fatalf("MiniMax H3 version=%d, want at least 3 now that image_to_video ships", h3.Version)
	}
	config, media := h3.MediaGeneration()
	if !media || len(config.Graphs) != 2 || config.Graphs[ModeTextToVideo] != "minimax-h3-t2v.json" || config.Graphs[ModeImageToVideo] != "minimax-h3-i2v.json" {
		t.Fatalf("MiniMax H3 does not expose both its text-to-video and image-to-video graphs: %#v", config.Graphs)
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
	// size_waits is not asserted here: the shipped recipe carries none until a
	// re-measurement lands with edges that match the console's 16:9 canvas
	// ladder. TestSizeWaitsAreServedAndStayAbsentByDefault and
	// TestRecipeCatalogServesMeasuredSizeWaits cover the field itself, on a
	// synthetic fixture built for the purpose.
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
	// Bytes stay exact here: per-file pins only auto-bump when every file is
	// byte-identical, so a byte change can only come from a human edit.
	if len(flash3bit.Artifacts) != 1 || flash3bit.TotalArtifactBytes() != 104207848032 || !isPinnedRevision(flash3bit.Artifacts[0].Revision) {
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
	// The pack's first hybrid-attention model: Qwen3.8 mixes Gated DeltaNet
	// linear-attention with full-attention layers, runs on a model-specific
	// SGLang build, and speculates through the checkpoint's own MTP head, so
	// EAGLE here needs no second artifact.
	qwen38, ok := Find(recipes, "qwen38-27b-nvfp4-1s")
	if !ok || qwen38.Runtime.Kind != "sglang" || qwen38.Distributed() {
		t.Fatalf("unexpected Qwen 3.8 recipe: %#v", qwen38)
	}
	if qwen38.Runtime.Reference() != "lmsysorg/sglang@sha256:febfb971c7352570fc445c466ebd6ffc9d896024958e544a60f2137fd85856b1" {
		t.Fatalf("Qwen 3.8 runtime is not pinned: %#v", qwen38.Runtime)
	}
	if len(qwen38.Artifacts) != 1 || qwen38.TotalArtifactBytes() == 0 || !isPinnedRevision(qwen38.Artifacts[0].Revision) {
		t.Fatalf("Qwen 3.8 weights are not pinned: %#v", qwen38.Artifacts)
	}
	if s := qwen38.Service.SGLang; s.SpeculativeAlgorithm != "EAGLE" || s.SpeculativeModelRole != "" ||
		!s.TrustRemoteCode || s.MambaRadixCacheStrategy != "extra_buffer_lazy" || s.MaxMambaCacheSize != s.MaxRunningRequests*4 {
		t.Fatalf("unexpected Qwen 3.8 serve configuration: %#v", s)
	}
	// The pack's first compressed sparse attention model, its first NEXTN
	// speculation, and its first serve on an image basement builds and
	// publishes itself. 135.25 GB of weights do not fit one Spark, so it runs
	// at TP=2 with the draft head that ships inside the checkpoint, which is
	// why a two-Spark recipe still declares exactly one artifact.
	flashNext, ok := Find(recipes, "qwen38-flash-next-nvfp4-2s")
	if !ok || flashNext.Runtime.Kind != "sglang" || !flashNext.Distributed() || flashNext.Topology.SparkCount != 2 {
		t.Fatalf("unexpected Qwen 3.8 Flash Next recipe: %#v", flashNext)
	}
	if flashNext.Runtime.Reference() != "ghcr.io/punkjazz-labs/basement-sglang-qwen38-flash-next@sha256:a7da51c60dcb673c72e3769187159b08207b51cf642cd4c3990d25083d8b7a4c" {
		t.Fatalf("Qwen 3.8 Flash Next runtime is not pinned: %#v", flashNext.Runtime)
	}
	if len(flashNext.Artifacts) != 1 || flashNext.TotalArtifactBytes() != 135253622894 || !isPinnedRevision(flashNext.Artifacts[0].Revision) {
		t.Fatalf("Qwen 3.8 Flash Next weights are not pinned: %#v", flashNext.Artifacts)
	}
	// The checkpoint carries no licence file, so the terms come from the base
	// model's own pinned tree rather than from a licence this recipe names by
	// itself.
	if flashNext.Artifacts[0].LicenceRepository != "Qwen/Qwen3.8-Flash-Next" || !isPinnedRevision(flashNext.Artifacts[0].LicenceRevision) {
		t.Fatalf("Qwen 3.8 Flash Next licence is not pinned upstream: %#v", flashNext.Artifacts[0])
	}
	if s := flashNext.Service.SGLang; s.TensorParallelSize != 2 || s.SpeculativeAlgorithm != "NEXTN" || s.SpeculativeEagleTopK != 1 ||
		s.SpeculativeNumDraftTokens != s.SpeculativeNumSteps+1 || s.SpeculativeModelRole != "" ||
		s.PageSize != 64 || s.MambaTrackInterval != 64 || !s.AllowAutoTruncate || !s.PLEOffloadEmbedding ||
		s.CUDAGraphBSDecode == "" {
		t.Fatalf("unexpected Qwen 3.8 Flash Next serve configuration: %#v", s)
	}
	if flashNext.MemoryModel != nil {
		t.Fatalf("a TP=2 recipe must not carry the flat single-node memory model: %#v", flashNext.MemoryModel)
	}
	// The pack's first behaviour-modified derivative: Qwen3.8-27B with its
	// refusal behaviour removed by weight ablation, shipped under the
	// publisher's own OBLITERATED name. It pins two files out of a repository
	// that also carries five other quantizations, a vision projector and two
	// overlapping bf16 shard sets, and it must serve the card's own bundled
	// chat template, because that template is what the publisher's quality
	// claims describe.
	obliterated, ok := Find(recipes, "qwen38-27b-obliterated-q8-0-1s")
	if !ok || obliterated.Runtime.Kind != "llamacpp" || obliterated.Distributed() {
		t.Fatalf("unexpected Obliterated recipe: %#v", obliterated)
	}
	if obliterated.Runtime.Reference() != flash3bit.Runtime.Reference() {
		t.Fatalf("Obliterated must share the DeepSeek 3-bit llama.cpp image pin: %#v", obliterated.Runtime)
	}
	if len(obliterated.Artifacts) != 1 || obliterated.TotalArtifactBytes() != 29047084826 || !isPinnedRevision(obliterated.Artifacts[0].Revision) {
		t.Fatalf("Obliterated weights are not pinned: %#v", obliterated.Artifacts)
	}
	if len(obliterated.Artifacts[0].Files) != 2 || obliterated.Service.LlamaCpp.ModelFile != obliterated.Artifacts[0].Files[0].Name ||
		obliterated.Service.LlamaCpp.ChatTemplateFile != obliterated.Artifacts[0].Files[1].Name {
		t.Fatalf("Obliterated must pin and serve its GGUF and the card's chat template: %#v", obliterated.Artifacts[0].Files)
	}
	if !obliterated.Service.LlamaCpp.Jinja || obliterated.MemoryModel == nil || obliterated.MemoryModel.KVBytesPerToken != 65536 {
		t.Fatalf("unexpected Obliterated serve configuration: %#v %#v", obliterated.Service.LlamaCpp, obliterated.MemoryModel)
	}
	planned, ok = obliterated.PlannedMemoryBytes()
	if !ok || planned+obliterated.Requirements.MemoryReserveBytes > obliterated.Requirements.MinimumMemoryBytes {
		t.Fatalf("Obliterated plans %d bytes, which does not leave its own reserve", planned)
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

// TestValidateAcceptsSGLangHybridAttentionFields exercises the fields a
// hybrid Gated-DeltaNet (linear-attention) plus full-attention model's
// qualified launcher configuration needs, none of which the minimal
// acceptance test above sets.
func TestValidateAcceptsSGLangHybridAttentionFields(t *testing.T) {
	candidate := sglangCandidate(t)
	candidate.Service.SGLang.TrustRemoteCode = true
	candidate.Service.SGLang.ChunkedPrefillSize = 8192
	candidate.Service.SGLang.DisablePrefillCUDAGraph = true
	candidate.Service.SGLang.MambaSSMDType = "bfloat16"
	candidate.Service.SGLang.MambaFullMemoryRatio = "4.21"
	candidate.Service.SGLang.MambaRadixCacheStrategy = "extra_buffer_lazy"
	candidate.Service.SGLang.MaxMambaCacheSize = 40
	candidate.Service.SGLang.SpeculativeAlgorithm = "EAGLE"
	candidate.Service.SGLang.SpeculativeNumDraftTokens = 4
	candidate.Service.SGLang.SpeculativeNumSteps = 3
	candidate.Service.SGLang.SpeculativeEagleTopK = 1
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate()=%v, want nil", err)
	}
}

// sparseAttentionSGLang is the launch configuration of a compressed sparse
// attention model: NEXTN drafting from the checkpoint's own head, a pinned
// KV cache page, Mamba state tracked in whole pages, offloaded PLE embedding
// tables and a short decode CUDA graph ladder. Every value here is one a
// qualified launcher pins.
func sparseAttentionSGLang(s *SGLangConfig) {
	s.TrustRemoteCode = true
	s.SpeculativeAlgorithm = "NEXTN"
	s.SpeculativeNumSteps = 3
	s.SpeculativeEagleTopK = 1
	s.SpeculativeNumDraftTokens = 4
	s.MambaSSMDType = "bfloat16"
	s.MambaRadixCacheStrategy = "extra_buffer"
	s.ReasoningParser = "auto"
	s.ToolCallParser = "auto"
	s.PageSize = 64
	s.MambaTrackInterval = 64
	s.AllowAutoTruncate = true
	s.PLEOffloadEmbedding = true
	// The launcher's own pair: a decode graph ladder that stops at the
	// largest batch the server admits.
	s.MaxRunningRequests = 16
	s.CUDAGraphBSDecode = "1 2 3 4 5 6 7 8 10 12 14 16"
}

// TestValidateAcceptsSGLangSparseAttentionFields covers the settings the
// hybrid-attention test above does not reach: the page and tracking pair, the
// two new switches, the decode graph list, NEXTN drafting with a top-k, the
// plain extra_buffer Mamba strategy and the parsers a recipe leaves to the
// chat template with "auto".
func TestValidateAcceptsSGLangSparseAttentionFields(t *testing.T) {
	candidate := sglangCandidate(t)
	sparseAttentionSGLang(candidate.Service.SGLang)
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate()=%v, want nil", err)
	}
	// NEXTN drafts with one branch, which is the only top-k its launcher
	// starts with, and leaving the value unset keeps the flag off the command
	// line the way every other number in this block does.
	candidate.Service.SGLang.SpeculativeEagleTopK = 0
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate() with an unpinned top-k=%v, want nil", err)
	}
	// A ladder that stops short of max_running_requests is a smaller capture
	// set, not an error; only a batch above it is refused.
	candidate.Service.SGLang.SpeculativeEagleTopK = 1
	candidate.Service.SGLang.CUDAGraphBSDecode = "1 2 4"
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate() with a short decode ladder=%v, want nil", err)
	}
}

// TestSGLangSparseAttentionFieldsSurviveARoundTrip proves the new settings
// are schema rather than Go: a recipe carrying them is written out and read
// back by the strict decoder, which refuses any field the schema does not
// name, and every value arrives unchanged.
func TestSGLangSparseAttentionFieldsSurviveARoundTrip(t *testing.T) {
	candidate := sglangCandidate(t)
	sparseAttentionSGLang(candidate.Service.SGLang)
	document, err := yaml.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeStrict(document)
	if err != nil {
		t.Fatalf("DecodeStrict()=%v", err)
	}
	if !reflect.DeepEqual(*decoded.Service.SGLang, *candidate.Service.SGLang) {
		t.Fatalf("decoded sglang block=%#v, want %#v", *decoded.Service.SGLang, *candidate.Service.SGLang)
	}
}

// TestSGLangSparseAttentionKeysAreTheDocumentedNames reads the new settings
// from a hand-written fragment instead of from a marshalled struct. The round
// trip above encodes with the same tags it then decodes with, so it would
// accept a misspelled key; a recipe author writes these names by hand, and
// each one has to match the flag it stands for.
func TestSGLangSparseAttentionKeysAreTheDocumentedNames(t *testing.T) {
	fragment := "page_size: 64\n" +
		"mamba_track_interval: 64\n" +
		"allow_auto_truncate: true\n" +
		"ple_offload_embedding: true\n" +
		"cuda_graph_bs_decode: \"1 2 4 8\"\n"
	decoder := yaml.NewDecoder(strings.NewReader(fragment))
	decoder.KnownFields(true)
	var block SGLangConfig
	if err := decoder.Decode(&block); err != nil {
		t.Fatalf("decode sglang fragment=%v", err)
	}
	if block.PageSize != 64 || block.MambaTrackInterval != 64 || !block.AllowAutoTruncate ||
		!block.PLEOffloadEmbedding || block.CUDAGraphBSDecode != "1 2 4 8" {
		t.Fatalf("decoded block=%#v", block)
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
		{"mamba radix cache strategy outside policy", func(r *Recipe) { r.Service.SGLang.MambaRadixCacheStrategy = "other" }, "outside the recipe policy"},
		{"mamba full memory ratio too low", func(r *Recipe) { r.Service.SGLang.MambaFullMemoryRatio = "0" }, "mamba_full_memory_ratio must be a decimal above 0 and no greater than 16"},
		{"mamba full memory ratio too high", func(r *Recipe) { r.Service.SGLang.MambaFullMemoryRatio = "17" }, "mamba_full_memory_ratio must be a decimal above 0 and no greater than 16"},
		{"sampling defaults outside policy", func(r *Recipe) { r.Service.SGLang.SamplingDefaults = "openai" }, "outside the recipe policy"},
		{"speculative num steps without an algorithm", func(r *Recipe) { r.Service.SGLang.SpeculativeNumSteps = 3 }, "speculative_num_steps requires speculative_algorithm"},
		{"speculative eagle topk with an algorithm outside the EAGLE family", func(r *Recipe) {
			r.Service.SGLang.SpeculativeAlgorithm = "NGRAM"
			r.Service.SGLang.SpeculativeNumDraftTokens = 4
			r.Service.SGLang.SpeculativeEagleTopK = 1
		}, "speculative_eagle_topk requires speculative_algorithm EAGLE, EAGLE3 or NEXTN"},
		{"chunked prefill size too large", func(r *Recipe) { r.Service.SGLang.ChunkedPrefillSize = 65537 }, "chunked_prefill_size must be between 0 and 65536"},
		{"max mamba cache size too large", func(r *Recipe) { r.Service.SGLang.MaxMambaCacheSize = 4097 }, "max_mamba_cache_size must be between 0 and 4096"},
		{"NEXTN drafting with more than one branch", func(r *Recipe) {
			r.Service.SGLang.SpeculativeAlgorithm = "NEXTN"
			r.Service.SGLang.SpeculativeNumDraftTokens = 4
			r.Service.SGLang.SpeculativeEagleTopK = 2
		}, "speculative_eagle_topk must be 1 with speculative_algorithm NEXTN"},
		{"page size no launcher has run", func(r *Recipe) { r.Service.SGLang.PageSize = 32 }, "page_size must be unset or 64"},
		{"mamba track interval off the page grid", func(r *Recipe) {
			r.Service.SGLang.PageSize = 64
			r.Service.SGLang.MambaTrackInterval = 96
		}, "mamba_track_interval must be a multiple of page_size"},
		{"mamba track interval with no page to sit on", func(r *Recipe) { r.Service.SGLang.MambaTrackInterval = 64 }, "mamba_track_interval requires page_size"},
		{"negative mamba track interval", func(r *Recipe) { r.Service.SGLang.MambaTrackInterval = -64 }, "mamba_track_interval must not be negative"},
		{"decode graph batch sizes that repeat", func(r *Recipe) { r.Service.SGLang.CUDAGraphBSDecode = "1 2 2" }, "cuda_graph_bs_decode must be positive whole numbers"},
		{"decode graph batch sizes that fall", func(r *Recipe) { r.Service.SGLang.CUDAGraphBSDecode = "4 2" }, "cuda_graph_bs_decode must be positive whole numbers"},
		{"decode graph batch size that is not a number", func(r *Recipe) { r.Service.SGLang.CUDAGraphBSDecode = "1 two 3" }, "cuda_graph_bs_decode must be positive whole numbers"},
		{"decode graph batch size of zero", func(r *Recipe) { r.Service.SGLang.CUDAGraphBSDecode = "0 1" }, "cuda_graph_bs_decode must be positive whole numbers"},
		{"decode graph batch size with a sign", func(r *Recipe) { r.Service.SGLang.CUDAGraphBSDecode = "1 +2 4" }, "cuda_graph_bs_decode must be positive whole numbers"},
		{"decode graph batch size with a leading zero", func(r *Recipe) { r.Service.SGLang.CUDAGraphBSDecode = "1 007 8" }, "cuda_graph_bs_decode must be positive whole numbers"},
		{"decode graph batch sizes that are only spaces", func(r *Recipe) { r.Service.SGLang.CUDAGraphBSDecode = "   " }, "cuda_graph_bs_decode must be positive whole numbers"},
		{"decode graph batch above the largest the server admits", func(r *Recipe) {
			// The candidate serves 8 requests at once, so a graph for 16
			// could never be replayed.
			r.Service.SGLang.CUDAGraphBSDecode = "1 2 16"
		}, "cuda_graph_bs_decode must not name a batch above max_running_requests"},
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
		{"quantization outside the evidence", func(v *VLLMConfig) { v.Quantization = "marlin" }, "outside the recipe policy"},
		{"a GLM parser name the launcher does not run", func(v *VLLMConfig) { v.ReasoningParser = "glm5" }, "outside the recipe policy"},
		{"video limit past the image ceiling", func(v *VLLMConfig) { v.MultimodalVideoLimit = 9 }, "multimodal_video_limit must be between 0 and 8"},
		{"negative video limit", func(v *VLLMConfig) { v.MultimodalVideoLimit = -1 }, "multimodal_video_limit must be between 0 and 8"},
		{"the tool parser name used as a reasoning parser", func(v *VLLMConfig) { v.ReasoningParser = "glm47" }, "outside the recipe policy"},
		{"the reasoning parser name used as a tool parser", func(v *VLLMConfig) { v.ToolCallParser = "glm45" }, "outside the recipe policy"},
		{"unbounded decode batch", func(v *VLLMConfig) { v.MaxNumSeqs = maxVLLMNumSeqs + 1 }, "max_num_seqs must be no more than"},
		{"unbounded scheduler token budget", func(v *VLLMConfig) {
			v.MaxBatchedTokens = maxVLLMBatchedTokens + 1
		}, "max_num_batched_tokens must be no more than"},
		{"more MTP drafts than a checkpoint head produces", func(v *VLLMConfig) {
			v.SpeculativeMethod, v.SpeculativeTokens = "mtp", maxMTPSpeculativeTokens+1
			v.SpeculativeDraftSampleMethod = ""
		}, "no more than 3 with speculative_method mtp"},
		{"two chat templates at once", func(v *VLLMConfig) {
			v.ChatTemplateFile, v.ChatTemplateImagePath = "chat_template.jinja", "/opt/glm53/chat_template.jinja"
		}, "not both"},
		{"image chat template that is not an absolute path", func(v *VLLMConfig) {
			v.ChatTemplateImagePath = "opt/glm53/chat_template.jinja"
		}, "must be an absolute container path"},
		{"image chat template climbing out of its directory", func(v *VLLMConfig) {
			v.ChatTemplateImagePath = "/opt/../model/chat_template.jinja"
		}, "must be an absolute container path"},
		{"image chat template over the weights", func(v *VLLMConfig) {
			v.ChatTemplateImagePath = "/model/chat_template.jinja"
		}, "collides with the container's /model mount"},
		{"image chat template over the scratch tmpfs", func(v *VLLMConfig) {
			v.ChatTemplateImagePath = "/tmp/chat_template.jinja"
		}, "collides with the container's /tmp mount"},
		{"image chat template over the compilation cache", func(v *VLLMConfig) {
			v.ChatTemplateImagePath = "/root/.cache/chat_template.jinja"
		}, "collides with the container's /root/.cache mount"},
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

// vllmCandidate is the pack's two-Spark vLLM recipe, which is the topology an
// EXL3 launcher runs at TP=2. The caller mutates the returned block.
func vllmCandidate(t *testing.T) Recipe {
	t.Helper()
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base, ok := Find(recipes, "deepseek-v4-flash-0731-2s")
	if !ok {
		t.Fatal("DeepSeek V4 Flash two-Spark recipe missing")
	}
	block := *base.Service.VLLM
	base.Service.VLLM = &block
	return base
}

// exl3MTPVLLM is the launch configuration of an EXL3 checkpoint served on the
// MTP speculative path: the exl3 kernel named explicitly, an fp8 KV cache,
// prefix caching on, a small decode batch beside a short scheduler budget, two
// drafted tokens from the checkpoint's own MTP layer, the launcher's own GLM
// parser names, and a chat template the runtime image carries rather than the
// checkpoint.
func exl3MTPVLLM(v *VLLMConfig) {
	v.Quantization = "exl3"
	v.KVCacheDType = "fp8"
	v.ReasoningParser = "glm45"
	v.ToolCallParser = "glm47"
	v.PrefixCaching = true
	v.MaxNumSeqs = 4
	v.MaxBatchedTokens = 1024
	v.MaxModelLen = 900000
	// The EXL3 launcher pins no KV cache block size, unlike the NVFP4 MLA
	// path this base recipe comes from.
	v.BlockSize = 0
	v.MaxCUDAGraphCaptureSize = 32
	v.SpeculativeMethod = "mtp"
	v.SpeculativeTokens = 2
	v.SpeculativeMoE = ""
	v.SpeculativeModelRole = ""
	v.SpeculativeDraftSampleMethod = ""
	v.ChatTemplateFile = ""
	v.ChatTemplateImagePath = "/opt/glm53/chat_template.jinja"
	// Image and video in, and the profiling pass the launcher calls required
	// on this hardware turned off.
	v.MultimodalImageLimit = 4
	v.MultimodalVideoLimit = 1
	v.SkipMultimodalProfiling = true
}

// TestValidateAcceptsVLLMEXL3MTPFields is the admit side of every rule this
// round adds: the quantization value, the two bounded batch budgets, the MTP
// draft count and the image-resident chat template.
func TestValidateAcceptsVLLMEXL3MTPFields(t *testing.T) {
	candidate := vllmCandidate(t)
	exl3MTPVLLM(candidate.Service.VLLM)
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate()=%v, want nil", err)
	}
	// Absent is still valid: no recipe that shipped before this round sets
	// either new field, and both must stay optional.
	candidate.Service.VLLM.Quantization = ""
	candidate.Service.VLLM.ChatTemplateImagePath = ""
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate() with both new fields unset=%v, want nil", err)
	}
}

// TestMTPDraftBoundIsMethodSpecific proves the tight MTP bound narrows the one
// method whose drafts come from the served checkpoint, and leaves a separate
// drafter alone. The pack's DFlash recipe runs 15 drafted tokens from a
// drafter it downloads, which the MTP ceiling must never refuse.
func TestMTPDraftBoundIsMethodSpecific(t *testing.T) {
	candidate := vllmCandidate(t)
	vllm := candidate.Service.VLLM
	vllm.SpeculativeMethod = "mtp"
	vllm.SpeculativeDraftSampleMethod = ""
	for _, tokens := range []int{1, 2, maxMTPSpeculativeTokens} {
		vllm.SpeculativeTokens = tokens
		if err := Validate(candidate); err != nil {
			t.Fatalf("Validate() with %d MTP drafts=%v, want nil", tokens, err)
		}
	}
	vllm.SpeculativeTokens = maxMTPSpeculativeTokens + 1
	if err := Validate(candidate); err == nil || !strings.Contains(err.Error(), "with speculative_method mtp") {
		t.Fatalf("Validate() with %d MTP drafts=%v, want a refusal", maxMTPSpeculativeTokens+1, err)
	}

	drafter := vllmCandidate(t)
	drafter.Artifacts = append(append([]Artifact(nil), drafter.Artifacts...), Artifact{
		Role: "drafter", Repository: drafter.Artifacts[0].Repository,
		Revision: drafter.Artifacts[0].Revision, ExpectedBytes: 2342175855,
		Licence: drafter.Artifacts[0].Licence, LicenceURL: drafter.Artifacts[0].LicenceURL,
	})
	drafter.Service.VLLM.SpeculativeMethod = "dflash"
	drafter.Service.VLLM.SpeculativeModelRole = "drafter"
	drafter.Service.VLLM.SpeculativeTokens = 15
	if err := Validate(drafter); err != nil {
		t.Fatalf("Validate() with 15 drafts from a separate drafter=%v, want nil", err)
	}
}

// TestChatTemplateImagePathRejectsEveryManagedMount covers the artifact mounts
// the table test above cannot reach: a recipe with a second artifact role
// mounts that role's own path, and an image template may not sit there either.
func TestChatTemplateImagePathRejectsEveryManagedMount(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base, ok := Find(recipes, "laguna-s-2-1-nvfp4-dflash-1s")
	if !ok {
		t.Fatal("Laguna DFlash recipe missing")
	}
	block := *base.Service.VLLM
	base.Service.VLLM = &block
	block.ChatTemplateImagePath = "/drafter/chat_template.jinja"
	if err := Validate(base); err == nil || !strings.Contains(err.Error(), "collides with the container's /drafter mount") {
		t.Fatalf("Validate()=%v, want the drafter mount collision", err)
	}
	block.ChatTemplateImagePath = "/opt/glm53/chat_template.jinja"
	if err := Validate(base); err != nil {
		t.Fatalf("Validate() with an image-resident template=%v, want nil", err)
	}
}

// TestImageChatTemplateMustSurviveTheRecipesOwnMounts proves the second half
// of the image template rule. A writable path is a private tmpfs mounted over
// the image's own directory, so a template underneath one is gone before the
// runtime reads it. The pack's two-Spark vLLM recipe declares two such paths.
func TestImageChatTemplateMustSurviveTheRecipesOwnMounts(t *testing.T) {
	candidate := vllmCandidate(t)
	if len(candidate.Runtime.WritablePaths) == 0 {
		t.Fatal("this test needs a recipe that declares a writable path")
	}
	writable := candidate.Runtime.WritablePaths[0]
	candidate.Service.VLLM.ChatTemplateImagePath = writable + "/chat_template.jinja"
	err := Validate(candidate)
	if err == nil || !strings.Contains(err.Error(), "is inside the recipe's writable path "+writable) {
		t.Fatalf("Validate()=%v, want a writable-path refusal", err)
	}
	// The same template outside every mount is exactly what the field is for.
	candidate.Service.VLLM.ChatTemplateImagePath = "/opt/glm53/chat_template.jinja"
	if err := Validate(candidate); err != nil {
		t.Fatalf("Validate()=%v, want nil", err)
	}
}

// TestVLLMEXL3MTPFieldsSurviveARoundTrip proves the new settings are schema
// rather than Go: a recipe carrying them is written out and read back by the
// strict decoder, which refuses any field the schema does not name, and every
// value arrives unchanged.
func TestVLLMEXL3MTPFieldsSurviveARoundTrip(t *testing.T) {
	candidate := vllmCandidate(t)
	exl3MTPVLLM(candidate.Service.VLLM)
	document, err := yaml.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeStrict(document)
	if err != nil {
		t.Fatalf("DecodeStrict()=%v", err)
	}
	if !reflect.DeepEqual(*decoded.Service.VLLM, *candidate.Service.VLLM) {
		t.Fatalf("decoded vllm block=%#v, want %#v", *decoded.Service.VLLM, *candidate.Service.VLLM)
	}
}

// TestVLLMEXL3MTPKeysAreTheDocumentedNames reads the new settings from a
// hand-written fragment instead of from a marshalled struct. The round trip
// above encodes with the same tags it then decodes with, so it would accept a
// misspelled key; a recipe author writes these names by hand, and each one has
// to match the flag it stands for.
func TestVLLMEXL3MTPKeysAreTheDocumentedNames(t *testing.T) {
	fragment := "quantization: exl3\n" +
		"chat_template_image_path: /opt/glm53/chat_template.jinja\n" +
		"multimodal_video_limit: 1\n" +
		"skip_mm_profiling: true\n"
	decoder := yaml.NewDecoder(strings.NewReader(fragment))
	decoder.KnownFields(true)
	var block VLLMConfig
	if err := decoder.Decode(&block); err != nil {
		t.Fatalf("decode vllm fragment=%v", err)
	}
	if block.Quantization != "exl3" || block.ChatTemplateImagePath != "/opt/glm53/chat_template.jinja" ||
		block.MultimodalVideoLimit != 1 || !block.SkipMultimodalProfiling {
		t.Fatalf("decoded block=%#v", block)
	}
}

// TestGLMOverlayEnvironmentAdmitsOnlyThePatchesOwnForms holds the two GLM
// toggles to the forms the image's own patches read. Both are opt-in overlay
// switches rather than tuning knobs, so a value neither patch understands must
// be refused here rather than ignored at run time, where it would read as the
// default and the recipe would silently serve something it did not ask for.
func TestGLMOverlayEnvironmentAdmitsOnlyThePatchesOwnForms(t *testing.T) {
	base := vllmCandidate(t)
	clone := func() Recipe {
		candidate := base
		candidate.Runtime.Environment = make(map[string]string, len(base.Runtime.Environment)+1)
		for name, value := range base.Runtime.Environment {
			candidate.Runtime.Environment[name] = value
		}
		return candidate
	}
	// Admit: every form the two patches document for themselves.
	for name, values := range map[string][]string{
		"GLM53_SUPPRESS_STOPS_IN_REASONING": {"0", "1"},
		"GLM53_MIXED_PREFILL_CHUNK":         {"skip", "-1", "0", "off"},
	} {
		for _, value := range values {
			candidate := clone()
			candidate.Runtime.Environment[name] = value
			if err := Validate(candidate); err != nil {
				t.Fatalf("%s=%s Validate()=%v, want nil", name, value, err)
			}
		}
	}
	// Refuse: forms no launcher runs. "128" is the numeric cap the scheduler
	// patch would accept and whose own note says it still stalls decode, and
	// "SKIP" is a case the patch would lowercase into skip; the allowlist
	// matches exactly, so a recipe writes the form the evidence shows.
	for name, values := range map[string][]string{
		"GLM53_SUPPRESS_STOPS_IN_REASONING": {"2", "true", "on", ""},
		"GLM53_MIXED_PREFILL_CHUNK":         {"128", "SKIP", "no", "yes"},
	} {
		for _, value := range values {
			candidate := clone()
			candidate.Runtime.Environment[name] = value
			err := Validate(candidate)
			if err == nil || !strings.Contains(err.Error(), "outside the allowlist") {
				t.Fatalf("%s=%q Validate()=%v, want an allowlist rejection", name, value, err)
			}
		}
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
