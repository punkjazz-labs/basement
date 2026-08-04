package recipe

import (
	"strings"
	"testing"
)

func TestBuiltinRecipePackIsPinnedCandidate(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(recipes) != 7 {
		t.Fatalf("got %d recipes, want 7", len(recipes))
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
		{"unknown kind", func(r *Recipe) { r.Runtime.Kind = "tensorrt" }, "runtime kind must be one of: sglang, vllm"},
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

func TestDecodeStrictRejectsUnknownField(t *testing.T) {
	_, err := DecodeStrict([]byte("schema_version: 1\nid: valid-recipe\nunexpected: true\n"))
	if err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("DecodeStrict()=%v", err)
	}
}
