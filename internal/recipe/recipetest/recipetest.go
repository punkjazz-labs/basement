// Package recipetest supplies the media recipe and the workflow graphs the
// tests of every layer above internal/recipe need. It exists because a media
// recipe cannot be taken from the shipped pack yet — the pack has none until
// a container image and a set of weights are pinned — and because a graph is
// a file the product ships, so a test that needs one must not be able to
// force one into the product just to have something to read.
//
// Nothing in the binary imports this package.
package recipetest

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

// TextToVideoGraph is a minimal ComfyUI API-format document carrying exactly
// the tokens a text_to_video mode requires.
const TextToVideoGraph = `{
  "3": {"class_type": "EmptyLatentVideo", "inputs": {"width": "{{WIDTH}}", "height": "{{HEIGHT}}", "length": "{{FRAMES}}"}},
  "4": {"class_type": "CLIPTextEncode", "inputs": {"text": "{{PROMPT}}"}},
  "5": {"class_type": "KSampler", "inputs": {"seed": "{{SEED}}", "steps": "{{STEPS}}", "latent": ["3", 0]}},
  "6": {"class_type": "SaveVideo", "inputs": {"filename_prefix": "basement", "video": ["5", 0]}}
}`

// ImageToVideoGraph additionally carries the source image token, which makes
// it the right fixture for proving that a graph offered to the wrong mode is
// refused.
const ImageToVideoGraph = `{
  "1": {"class_type": "LoadImage", "inputs": {"image": "{{IMAGE}}"}},
  "3": {"class_type": "EmptyLatentVideo", "inputs": {"width": "{{WIDTH}}", "height": "{{HEIGHT}}", "length": "{{FRAMES}}"}},
  "4": {"class_type": "CLIPTextEncode", "inputs": {"text": "{{PROMPT}}"}},
  "5": {"class_type": "KSampler", "inputs": {"seed": "{{SEED}}", "steps": "{{STEPS}}", "latent": ["3", 0]}}
}`

// WithGraphs points recipe.GraphSource at an in-memory set for one test,
// using the override idiom this repository uses everywhere: save, restore on
// cleanup, then assign.
func WithGraphs(t *testing.T, files map[string]string) {
	t.Helper()
	previous := recipe.GraphSource
	t.Cleanup(func() { recipe.GraphSource = previous })
	mapped := fstest.MapFS{}
	for name, body := range files {
		mapped[name] = &fstest.MapFile{Data: []byte(body)}
	}
	recipe.GraphSource = fs.FS(mapped)
}

// WithTextToVideoGraph is the common case: one workflow, named the way Media
// names it.
func WithTextToVideoGraph(t *testing.T) {
	WithGraphs(t, map[string]string{"t2v.json": TextToVideoGraph})
}

// Media is a complete, valid comfyui recipe. Every figure in it is a fixture
// value, not a measurement of anything: it exists to exercise the schema and
// the adapter, and no part of it is a claim about a real model.
func Media() recipe.Recipe {
	return recipe.Recipe{
		SchemaVersion: 1,
		ID:            "media-test-1s",
		Version:       1,
		DisplayName:   "Media Test",
		Publisher:     "basement",
		Trust:         "basement-candidate",
		Verification:  "candidate",
		Source: recipe.Source{
			URL:      "https://github.com/punkjazz-labs/basement",
			Revision: "0123456789abcdef0123456789abcdef01234567",
		},
		Topology: recipe.Topology{SparkCount: 1},
		Runtime: recipe.Runtime{
			Kind:           "comfyui",
			Image:          "ghcr.io/punkjazz-labs/comfyui-gb10",
			Digest:         "sha256:" + strings.Repeat("a", 64),
			ImageBytes:     8_000_000_000,
			ImageDiskBytes: 16_000_000_000,
		},
		Artifacts: []recipe.Artifact{{
			Role:          "primary",
			Repository:    "Comfy-Org/Media-Test",
			Revision:      strings.Repeat("b", 40),
			ExpectedBytes: 30_000_000_000,
			Files: []recipe.ArtifactFile{
				{Name: "diffusion_models/model.safetensors", ExpectedBytes: 20_000_000_000},
				{Name: "text_encoders/encoder.safetensors", ExpectedBytes: 8_000_000_000},
				{Name: "vae/vae.safetensors", ExpectedBytes: 2_000_000_000},
			},
			Licence:    "Media Test Community License Agreement",
			LicenceURL: "https://huggingface.co/Comfy-Org/Media-Test/blob/main/LICENSE",
		}},
		Requirements: recipe.Requirements{
			Architecture: "aarch64", DGXSpark: true, Docker: true, NvidiaRuntime: true,
			MinimumMemoryBytes: 120_000_000_000, MemoryReserveBytes: 16_000_000_000,
			SafetyMarginBytes: 20_000_000_000, RequiredLicenceAccept: true,
		},
		Service: recipe.Service{
			InternalPort: 8188, DefaultHostPort: 8188, ServedModelID: "Comfy-Org/Media-Test",
			ComfyUI: &recipe.ComfyUIConfig{
				Graphs:                   map[string]string{recipe.ModeTextToVideo: "t2v.json"},
				OutputDirectory:          "/output",
				InputDirectory:           "/input",
				DefaultShortEdge:         768,
				MaxShortEdge:             768,
				MaxLongEdge:              1344,
				FrameBlock:               17,
				FrameOffset:              5,
				FramesPerSecond:          24,
				MinBlocks:                1,
				MaxBlocks:                21,
				DefaultBlocks:            7,
				SamplerSteps:             20,
				VerificationSamplerSteps: 1,
				ConcurrentGenerations:    1,
			},
		},
		MemoryModel: &recipe.MemoryModel{WeightsBytes: 30_000_000_000, KVBytesPerToken: 0, RuntimeOverheadBytes: 20_000_000_000},
		Operations: []recipe.Operation{
			{Type: "verify_architecture"}, {Type: "verify_dgx_spark"}, {Type: "verify_memory_capacity"}, {Type: "verify_disk"},
			{Type: "verify_port"}, {Type: "verify_docker"}, {Type: "verify_nvidia_runtime"}, {Type: "verify_artifact_access"},
			{Type: "pull_image"}, {Type: "download_artifact"}, {Type: "write_generated_config"}, {Type: "create_container"},
			{Type: "verify_memory"}, {Type: "start_container"}, {Type: "wait_http"}, {Type: "verify_media_generation"},
		},
		Uninstall: []recipe.Operation{{Type: "stop_container"}, {Type: "remove_container"}, {Type: "remove_artifact_if_unshared"}},
	}
}

// Copy returns a media recipe whose maps and slices are its own, so a test
// that mutates one field does not reach into another test's fixture.
func Copy(r recipe.Recipe) recipe.Recipe {
	r.Artifacts = append([]recipe.Artifact(nil), r.Artifacts...)
	for index := range r.Artifacts {
		r.Artifacts[index].Files = append([]recipe.ArtifactFile(nil), r.Artifacts[index].Files...)
	}
	r.Operations = append([]recipe.Operation(nil), r.Operations...)
	r.Uninstall = append([]recipe.Operation(nil), r.Uninstall...)
	if r.Service.ComfyUI != nil {
		config := *r.Service.ComfyUI
		graphs := make(map[string]string, len(config.Graphs))
		for mode, name := range config.Graphs {
			graphs[mode] = name
		}
		config.Graphs = graphs
		r.Service.ComfyUI = &config
	}
	if r.MemoryModel != nil {
		memory := *r.MemoryModel
		r.MemoryModel = &memory
	}
	return r
}
