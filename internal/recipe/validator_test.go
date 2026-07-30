package recipe

import (
	"strings"
	"testing"
)

func TestBuiltinQwenRecipeIsPinnedCandidate(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(recipes) != 1 {
		t.Fatalf("got %d recipes, want 1", len(recipes))
	}
	r := recipes[0]
	if r.ID != "qwen36-35b-a3b-nvfp4-1s" || r.Verification != "candidate" {
		t.Fatalf("unexpected recipe identity: %#v", r)
	}
	if r.Runtime.Reference() != "vllm/vllm-openai@sha256:4cebac8c03f2cd9f5fabe72ac7c2a0b3aaa8450ef8f0e47429425fd1bfb83d42" {
		t.Fatalf("runtime is not pinned: %s", r.Runtime.Reference())
	}
	if got := r.Artifacts[0].Revision; got != "bafac91da4448b4c81b820e90a089d4ae953f5f8" {
		t.Fatalf("model revision is not pinned: %s", got)
	}
	if got := r.TotalArtifactBytes(); got != 23462477838 {
		t.Fatalf("artifact bytes=%d", got)
	}
}

func TestRecipePolicyRejectsUnsafeVariants(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base := recipes[0]
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
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.Artifacts = append([]Artifact(nil), base.Artifacts...)
			candidate.Operations = append([]Operation(nil), base.Operations...)
			candidate.Requirements.Secrets = append([]string(nil), base.Requirements.Secrets...)
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
