package recipe_test

import (
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/recipe/recipetest"
)

func TestMediaRecipeValidates(t *testing.T) {
	recipetest.WithTextToVideoGraph(t)
	r := recipetest.Media()
	if err := recipe.Validate(r); err != nil {
		t.Fatalf("a complete media recipe must validate: %v", err)
	}
	config, media := r.MediaGeneration()
	if !media {
		t.Fatal("a comfyui recipe must report itself as a media model")
	}
	// The duration grid is arithmetic on the recipe's own numbers, never a
	// separately stated second count.
	if got := config.Frames(1); got != 22 {
		t.Fatalf("Frames(1)=%d, want 22", got)
	}
	if got := config.Seconds(1); got < 0.9 || got > 0.92 {
		t.Fatalf("Seconds(1)=%v, want about 0.92", got)
	}
	planned, ok := r.PlannedMemoryBytes()
	if !ok || planned != 50_000_000_000 {
		t.Fatalf("PlannedMemoryBytes()=%d,%v; want the weights plus the measured overhead", planned, ok)
	}
}

// TestMediaRecipeRejections is the strictness table. Every case is a recipe
// that would otherwise install and then fail on hardware, or install and
// serve something nobody qualified.
func TestMediaRecipeRejections(t *testing.T) {
	recipetest.WithGraphs(t, map[string]string{
		"t2v.json":     recipetest.TextToVideoGraph,
		"i2v.json":     recipetest.ImageToVideoGraph,
		"nopoint.json": `{"1":{"inputs":{"seed":"{{SEED}}","width":"{{WIDTH}}","height":"{{HEIGHT}}","length":"{{FRAMES}}"}}}`,
		"broken.json":  `{"1": not json}`,
	})
	tests := []struct {
		name   string
		mutate func(*recipe.Recipe)
		want   string
	}{
		{"text port on a media recipe", func(r *recipe.Recipe) { r.Service.InternalPort = 8000 }, "internal_port 8188"},
		{"install ends in the text verification", func(r *recipe.Recipe) {
			r.Operations[len(r.Operations)-1] = recipe.Operation{Type: "verify_openai_inference"}
		}, "ending in verify_media_generation"},
		{"two runtime blocks", func(r *recipe.Recipe) {
			r.Service.LlamaCpp = &recipe.LlamaCppConfig{ModelFile: "x.gguf", ContextSize: 1, Parallel: 1}
		}, "keep only the block that matches runtime.kind"},
		{"kind and block disagree", func(r *recipe.Recipe) { r.Runtime.Kind = "vllm" }, "requires service.vllm"},
		{"two Sparks", func(r *recipe.Recipe) {
			r.Topology.SparkCount = 2
			r.Topology.Interconnect = &recipe.Interconnect{Kind: "connectx7", MasterPort: 29500, SharedEnvironment: map[string]string{"NCCL_SOCKET_IFNAME": "enp1s0f1np1"}}
		}, "single Spark"},
		{"no graphs", func(r *recipe.Recipe) { r.Service.ComfyUI.Graphs = nil }, "at least one workflow"},
		{"no text_to_video graph", func(r *recipe.Recipe) {
			r.Service.ComfyUI.Graphs = map[string]string{recipe.ModeImageToVideo: "i2v.json"}
		}, "must include text_to_video"},
		{"unknown mode", func(r *recipe.Recipe) { r.Service.ComfyUI.Graphs["text_to_music"] = "t2v.json" }, "is not one of"},
		{"missing graph file", func(r *recipe.Recipe) { r.Service.ComfyUI.Graphs[recipe.ModeTextToVideo] = "absent.json" }, "read workflow graph"},
		{"graph is not JSON", func(r *recipe.Recipe) { r.Service.ComfyUI.Graphs[recipe.ModeTextToVideo] = "broken.json" }, "not valid JSON"},
		{"graph ignores the prompt", func(r *recipe.Recipe) { r.Service.ComfyUI.Graphs[recipe.ModeTextToVideo] = "nopoint.json" }, "requires exactly"},
		{"graph carries a token its mode never substitutes", func(r *recipe.Recipe) {
			r.Service.ComfyUI.Graphs[recipe.ModeTextToVideo] = "i2v.json"
		}, "requires exactly"},
		{"output directory over the weights", func(r *recipe.Recipe) {
			r.Service.ComfyUI.OutputDirectory = recipe.PrimaryMountPath
		}, "collides with the container's /model mount"},
		{"input directory over the cache", func(r *recipe.Recipe) {
			r.Service.ComfyUI.InputDirectory = recipe.CacheMountPath + "/x"
		}, "collides with the container's /root/.cache mount"},
		{"output inside input", func(r *recipe.Recipe) { r.Service.ComfyUI.OutputDirectory = "/input/results" }, "must be separate paths"},
		{"relative output directory", func(r *recipe.Recipe) { r.Service.ComfyUI.OutputDirectory = "output" }, "absolute container path"},
		{"missing input directory", func(r *recipe.Recipe) { r.Service.ComfyUI.InputDirectory = "" }, "input_directory is required"},
		{"canvas off the grid", func(r *recipe.Recipe) { r.Service.ComfyUI.DefaultShortEdge = 700 }, "multiple of 32"},
		{"canvas beyond the cap", func(r *recipe.Recipe) { r.Service.ComfyUI.MaxLongEdge = 8192 }, "no greater than 4096"},
		{"default larger than the maximum", func(r *recipe.Recipe) { r.Service.ComfyUI.DefaultShortEdge = 1024 }, "must not exceed max_short_edge"},
		{"short edge larger than the long edge", func(r *recipe.Recipe) { r.Service.ComfyUI.MaxLongEdge = 512 }, "must not exceed max_long_edge"},
		{"no frame block", func(r *recipe.Recipe) { r.Service.ComfyUI.FrameBlock = 0 }, "frame_block must be positive"},
		{"no frame rate", func(r *recipe.Recipe) { r.Service.ComfyUI.FramesPerSecond = 0 }, "frames_per_second"},
		{"blocks inverted", func(r *recipe.Recipe) { r.Service.ComfyUI.MinBlocks = 30 }, "min_blocks <= max_blocks"},
		{"default outside the block range", func(r *recipe.Recipe) { r.Service.ComfyUI.DefaultBlocks = 40 }, "default_blocks must be between"},
		{"more than one generation at a time", func(r *recipe.Recipe) { r.Service.ComfyUI.ConcurrentGenerations = 2 }, "concurrent_generations must be 1"},
		{"whole snapshot download", func(r *recipe.Recipe) { r.Artifacts[0].Files = nil }, "pin its weights file by file"},
		{"weights outside a model folder", func(r *recipe.Recipe) {
			r.Artifacts[0].Files = []recipe.ArtifactFile{{Name: "model.safetensors", ExpectedBytes: 30_000_000_000}}
		}, "ComfyUI's model folders"},
		{"weights in a folder ComfyUI does not resolve", func(r *recipe.Recipe) {
			r.Artifacts[0].Files = []recipe.ArtifactFile{{Name: "weights/model.safetensors", ExpectedBytes: 30_000_000_000}}
		}, "ComfyUI's model folders"},
		{"no memory model", func(r *recipe.Recipe) { r.MemoryModel = nil }, "must declare memory_model"},
		{"a KV cache a media model does not have", func(r *recipe.Recipe) { r.MemoryModel.KVBytesPerToken = 4 }, "kv_bytes_per_token must be 0"},
		{"weights disagree with the artifact", func(r *recipe.Recipe) { r.MemoryModel.WeightsBytes = 1_000_000 }, "must equal the primary artifact"},
		{"footprint eats the host reserve", func(r *recipe.Recipe) {
			r.MemoryModel.RuntimeOverheadBytes = 90_000_000_000
		}, "does not preserve the per-node memory reserve"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := recipetest.Copy(recipetest.Media())
			test.mutate(&candidate)
			err := recipe.Validate(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate()=%v, want error containing %q", err, test.want)
			}
		})
	}
}

// TestTextRecipesKeepTheirOwnPortAndVerification proves the two kind-aware
// rules did not change what already shipped.
func TestTextRecipesKeepTheirOwnPortAndVerification(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recipes {
		if r.Service.InternalPort != 8000 {
			t.Fatalf("%s serves on %d, want 8000", r.ID, r.Service.InternalPort)
		}
		if got := recipe.InferenceVerification(r.Runtime.Kind); got != "verify_openai_inference" {
			t.Fatalf("%s (%s) verifies with %s", r.ID, r.Runtime.Kind, got)
		}
		if _, media := r.MediaGeneration(); media {
			t.Fatalf("%s reports itself as a media model", r.ID)
		}
	}
	if got := recipe.InferenceVerification("comfyui"); got != "verify_media_generation" {
		t.Fatalf("InferenceVerification(comfyui)=%s", got)
	}
}

// TestMediaRecipeDecodesFromYAML proves the block reaches the schema through
// the strict decoder recipes are actually loaded with, not only through a
// struct built in Go.
func TestMediaRecipeDecodesFromYAML(t *testing.T) {
	recipetest.WithTextToVideoGraph(t)
	document := `
schema_version: 1
id: media-yaml-1s
version: 1
display_name: Media YAML
publisher: basement
trust: basement-candidate
verification: candidate
source:
  url: https://github.com/punkjazz-labs/basement
  revision: 0123456789abcdef0123456789abcdef01234567
topology:
  spark_count: 1
runtime:
  kind: comfyui
  image: ghcr.io/punkjazz-labs/comfyui-gb10
  digest: sha256:` + strings.Repeat("a", 64) + `
  image_bytes: 8000000000
  image_disk_bytes: 16000000000
  start_timeout_minutes: 45
artifacts:
  - role: primary
    repository: Comfy-Org/Media-Test
    revision: ` + strings.Repeat("b", 40) + `
    expected_bytes: 30000000000
    files:
      - name: diffusion_models/model.safetensors
        expected_bytes: 20000000000
      - name: text_encoders/encoder.safetensors
        expected_bytes: 8000000000
      - name: vae/vae.safetensors
        expected_bytes: 2000000000
    licence: Media Test Community License Agreement
    licence_url: https://huggingface.co/Comfy-Org/Media-Test/blob/main/LICENSE
    licence_territory_exclusions:
      - European Union
requirements:
  architecture: aarch64
  dgx_spark: true
  docker: true
  nvidia_container_runtime: true
  per_node_minimum_memory_bytes: 120000000000
  per_node_memory_reserve_bytes: 16000000000
  safety_margin_bytes: 20000000000
  required_licence_acceptance: true
service:
  internal_port: 8188
  default_host_port: 8188
  served_model_id: Comfy-Org/Media-Test
  comfyui:
    graphs:
      text_to_video: t2v.json
    output_directory: /output
    input_directory: /input
    default_short_edge: 768
    max_short_edge: 768
    max_long_edge: 1344
    frame_block: 17
    frame_offset: 5
    frames_per_second: 24
    min_blocks: 1
    max_blocks: 21
    default_blocks: 7
    concurrent_generations: 1
memory_model:
  weights_bytes: 30000000000
  kv_bytes_per_token: 0
  runtime_overhead_bytes: 20000000000
operations:
  - type: verify_architecture
  - type: verify_dgx_spark
  - type: verify_memory_capacity
  - type: verify_disk
  - type: verify_port
  - type: verify_docker
  - type: verify_nvidia_runtime
  - type: verify_artifact_access
  - type: pull_image
  - type: download_artifact
  - type: write_generated_config
  - type: create_container
  - type: verify_memory
  - type: start_container
  - type: wait_http
  - type: verify_media_generation
uninstall:
  - type: stop_container
  - type: remove_container
  - type: remove_artifact_if_unshared
`
	decoded, err := recipe.DecodeStrict([]byte(document))
	if err != nil {
		t.Fatalf("DecodeStrict()=%v", err)
	}
	config, media := decoded.MediaGeneration()
	if !media || config.Graphs[recipe.ModeTextToVideo] != "t2v.json" || config.MaxLongEdge != 1344 {
		t.Fatalf("decoded media block=%#v", config)
	}
	if !decoded.RequiresTerritoryConfirmation() {
		t.Fatal("a territory-gated media recipe must still require its confirmation")
	}
}
