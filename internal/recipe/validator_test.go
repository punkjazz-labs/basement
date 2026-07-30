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
	if len(recipes) != 3 {
		t.Fatalf("got %d recipes, want 3", len(recipes))
	}
	for _, r := range recipes {
		if r.Verification != "candidate" || r.Trust != "runonspark-candidate" {
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
		{"false verification", func(r *Recipe) { r.Trust = "runonspark-verified" }, "real DGX verification"},
		{"mutable source", func(r *Recipe) { r.Source.Revision = "main" }, "immutable revision"},
		{"unsafe environment", func(r *Recipe) { r.Runtime.Environment["LD_PRELOAD"] = "/tmp/hook.so" }, "allowlist"},
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

func TestDecodeStrictRejectsUnknownField(t *testing.T) {
	_, err := DecodeStrict([]byte("schema_version: 1\nid: valid-recipe\nunexpected: true\n"))
	if err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("DecodeStrict()=%v", err)
	}
}
