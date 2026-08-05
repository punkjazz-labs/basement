package recipe_test

import (
	"encoding/json"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/recipe/recipetest"
)

type apiGraphNode struct {
	ClassType string         `json:"class_type"`
	Inputs    map[string]any `json:"inputs"`
}

func TestRenderGraphSubstitutesTypedValues(t *testing.T) {
	rendered, err := recipe.RenderGraph([]byte(recipetest.TextToVideoGraph), recipe.GraphInputs{
		Prompt: "a cat", Seed: 4242, Frames: 22, Width: 768, Height: 768,
	})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]map[string]any
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatal(err)
	}
	latent := document["3"]["inputs"].(map[string]any)
	// Numbers must arrive as numbers. A width of "768" is a string ComfyUI
	// rejects inside its own node validation, with a message about a node.
	if latent["width"] != float64(768) || latent["height"] != float64(768) || latent["length"] != float64(22) {
		t.Fatalf("canvas did not substitute as numbers: %#v", latent)
	}
	if seed := document["5"]["inputs"].(map[string]any)["seed"]; seed != float64(4242) {
		t.Fatalf("seed=%#v, want the number 4242", seed)
	}
	if text := document["4"]["inputs"].(map[string]any)["text"]; text != "a cat" {
		t.Fatalf("prompt=%#v", text)
	}
	// Everything the graph pinned is still there, unchanged.
	if steps := document["5"]["inputs"].(map[string]any)["steps"]; steps != float64(20) {
		t.Fatalf("a pinned setting was lost: %#v", steps)
	}
	if strings.Contains(string(rendered), "{{") {
		t.Fatalf("a token survived rendering: %s", rendered)
	}
}

// TestRenderGraphLeavesPromptTextAlone is the reason substitution is by whole
// value: a prompt is arbitrary user text, and text that looks like a token,
// or carries a quote, must reach the model as itself.
func TestRenderGraphLeavesPromptTextAlone(t *testing.T) {
	prompt := `a sign reading "{{SEED}}" in a window`
	rendered, err := recipe.RenderGraph([]byte(recipetest.TextToVideoGraph), recipe.GraphInputs{
		Prompt: prompt, Seed: 7, Frames: 22, Width: 768, Height: 768,
	})
	if err != nil {
		t.Fatalf("a quote in the prompt broke rendering: %v", err)
	}
	var document map[string]map[string]any
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatalf("a quote in the prompt produced invalid JSON: %v", err)
	}
	if text := document["4"]["inputs"].(map[string]any)["text"]; text != prompt {
		t.Fatalf("prompt=%#v, want it verbatim", text)
	}
}

// TestRenderGraphSkipsPartialTokens proves a token has to be a whole value.
// A partial match left in place is what the validator then rejects, which is
// better than a half-substituted string reaching the runtime.
func TestRenderGraphSkipsPartialTokens(t *testing.T) {
	rendered, err := recipe.RenderGraph([]byte(`{"1":{"inputs":{"name":"clip-{{SEED}}.mp4"}}}`), recipe.GraphInputs{Seed: 9})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "clip-{{SEED}}.mp4") {
		t.Fatalf("a partial token was substituted: %s", rendered)
	}
}

// TestRenderGraphOmitsTheImageWhenThereIsNone keeps a text-to-video request
// from silently blanking a token no mode of it carries.
func TestRenderGraphOmitsTheImageWhenThereIsNone(t *testing.T) {
	rendered, err := recipe.RenderGraph([]byte(recipetest.ImageToVideoGraph), recipe.GraphInputs{
		Prompt: "x", Seed: 1, Frames: 22, Width: 768, Height: 768,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), recipe.GraphImageToken) {
		t.Fatalf("an unsupplied image token was substituted anyway: %s", rendered)
	}
}

func TestRenderGraphRejectsNonJSON(t *testing.T) {
	if _, err := recipe.RenderGraph([]byte("not json"), recipe.GraphInputs{}); err == nil {
		t.Fatal("a graph that is not JSON must be refused")
	}
	if _, err := recipe.RenderGraph([]byte(`["a","b"]`), recipe.GraphInputs{}); err == nil {
		t.Fatal("a graph that is not an object must be refused")
	}
}

func TestGraphTokensReportsWhatAGraphCarries(t *testing.T) {
	tokens, err := recipe.GraphTokens([]byte(recipetest.ImageToVideoGraph))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		recipe.GraphFramesToken, recipe.GraphHeightToken, recipe.GraphImageToken,
		recipe.GraphPromptToken, recipe.GraphSeedToken, recipe.GraphWidthToken,
	}, " ")
	if got := strings.Join(tokens, " "); got != want {
		t.Fatalf("tokens=%s, want %s", got, want)
	}
}

func TestRequiredGraphTokensAddsTheImageOnlyForImageToVideo(t *testing.T) {
	if got := strings.Join(recipe.RequiredGraphTokens(recipe.ModeTextToVideo), " "); strings.Contains(got, recipe.GraphImageToken) {
		t.Fatalf("text_to_video must not require a source image: %s", got)
	}
	if got := strings.Join(recipe.RequiredGraphTokens(recipe.ModeImageToVideo), " "); !strings.Contains(got, recipe.GraphImageToken) {
		t.Fatalf("image_to_video must require a source image: %s", got)
	}
}

func TestGraphRefusesUnsafeNames(t *testing.T) {
	recipetest.WithTextToVideoGraph(t)
	for _, name := range []string{"", ".", "..", "../recipes/qwen.yaml", "sub/dir.json", "t2v.yaml", "/etc/passwd"} {
		if _, err := recipe.Graph(name); err == nil {
			t.Fatalf("Graph(%q) was accepted", name)
		}
	}
	if _, err := recipe.Graph("t2v.json"); err != nil {
		t.Fatalf("Graph(t2v.json)=%v", err)
	}
}

// TestFixtureGraphOverlayKeepsShippedGraphsResolvable reproduces the graph
// lookup context used by the generation API tests. A fixture may add a small
// graph, but it must not hide graphs that recipes in the binary declare.
func TestFixtureGraphOverlayKeepsShippedGraphsResolvable(t *testing.T) {
	recipetest.WithTextToVideoGraph(t)
	if _, err := recipe.Builtin(); err != nil {
		t.Fatalf("the built-in recipe pack lost an embedded graph behind a fixture overlay: %v", err)
	}
	if _, err := recipe.Graph("t2v.json"); err != nil {
		t.Fatalf("the fixture graph stopped resolving: %v", err)
	}
}

// TestShippedGraphsAreValidJSON holds every graph this binary carries to the
// contract, whether or not a recipe names it yet. A graph that shipped broken
// would only be discovered by the first person who installed the model.
func TestShippedGraphsAreValidJSON(t *testing.T) {
	entries, err := fs.ReadDir(recipe.GraphSource, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := recipe.Graph(entry.Name())
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if _, err := recipe.GraphTokens(raw); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
	}
}

func TestMiniMaxH3GraphsRenderAsAPIPrompts(t *testing.T) {
	// These are the node classes used by the converted workflows and present
	// in the pinned ComfyUI v0.30.0 /object_info response. Keeping the set
	// explicit catches a misspelled or frontend-only class before it ships.
	objectInfoClasses := map[string]bool{
		"BasicGuider":           true,
		"BasicScheduler":        true,
		"CLIPLoader":            true,
		"CreateVideo":           true,
		"KSamplerSelect":        true,
		"LoadImage":             true,
		"MiniMaxH3ImageToVideo": true,
		"RandomNoise":           true,
		"SamplerCustomAdvanced": true,
		"SaveVideo":             true,
		"UNETLoader":            true,
		"VAEDecode":             true,
		"VAEDecodeAudio":        true,
		"VAELoader":             true,
	}
	tests := []struct {
		name  string
		mode  string
		image string
	}{
		{"minimax-h3-t2v.json", recipe.ModeTextToVideo, ""},
		{"minimax-h3-i2v.json", recipe.ModeImageToVideo, "source.png"},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			raw, err := recipe.Graph(test.name)
			if err != nil {
				t.Fatal(err)
			}
			tokens, err := recipe.GraphTokens(raw)
			if err != nil {
				t.Fatal(err)
			}
			wantTokens := recipe.RequiredGraphTokens(test.mode)
			slices.Sort(wantTokens)
			if !slices.Equal(tokens, wantTokens) {
				t.Fatalf("tokens=%q, want %q", tokens, wantTokens)
			}
			hasImage := slices.Contains(tokens, recipe.GraphImageToken)
			if hasImage != (test.mode == recipe.ModeImageToVideo) {
				t.Fatalf("image token present=%t for %s", hasImage, test.mode)
			}

			rendered, err := recipe.RenderGraph(raw, recipe.GraphInputs{
				Prompt: "a camera tracks a runner",
				Seed:   42,
				Frames: 124,
				Width:  1344,
				Height: 768,
				Image:  test.image,
			})
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]apiGraphNode
			if err := json.Unmarshal(rendered, &document); err != nil {
				t.Fatalf("rendered graph is not valid JSON: %v", err)
			}
			if strings.Contains(string(rendered), "{{") {
				t.Fatalf("a substitution token survived rendering: %s", rendered)
			}
			for id, node := range document {
				if !objectInfoClasses[node.ClassType] {
					t.Errorf("node %s has class_type %q absent from the pinned object_info", id, node.ClassType)
				}
			}
		})
	}
}

func TestMiniMaxH3GraphsKeepPinnedTemplateSettings(t *testing.T) {
	for _, name := range []string{"minimax-h3-t2v.json", "minimax-h3-i2v.json"} {
		t.Run(name, func(t *testing.T) {
			raw, err := recipe.Graph(name)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]apiGraphNode
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			checks := []struct {
				node  string
				input string
				want  any
			}{
				{"6", "unet_name", "minimax_h3_fl2va_pruned_int8_convrot.safetensors"},
				{"6", "weight_dtype", "default"},
				{"9", "scheduler", "simple"},
				{"9", "steps", float64(20)},
				{"9", "denoise", float64(1)},
				{"11", "vae_name", "minimax_h3_video_vae_fp16.safetensors"},
				{"13", "clip_name", "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors"},
				{"13", "type", "minimax"},
				{"13", "device", "default"},
				{"17", "sampler_name", "res_multistep"},
				{"24", "vae_name", "minimax_h3_audio_vae_fp32.safetensors"},
				{"91", "fps", float64(24)},
				{"91", "bit_depth", float64(8)},
				{"92", "filename_prefix", "video/MiniMax_H3"},
				{"92", "format", "auto"},
				{"92", "codec", "auto"},
			}
			for _, check := range checks {
				if got := document[check.node].Inputs[check.input]; got != check.want {
					t.Errorf("node %s input %s=%#v, want %#v", check.node, check.input, got, check.want)
				}
			}
		})
	}
}

// TestMiniMaxH3GraphLoadersResolveToPinnedArtifactFiles connects the two
// independently pinned surfaces that ComfyUI joins at runtime: graph loaders
// name bare files, while the artifact stores those files under model folders.
// A mismatch here would only appear after the complete artifact downloaded.
func TestMiniMaxH3GraphLoadersResolveToPinnedArtifactFiles(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	h3, ok := recipe.Find(recipes, "minimax-h3-comfyui-1s")
	if !ok {
		t.Fatal("MiniMax H3 recipe missing")
	}
	if len(h3.Artifacts) != 1 {
		t.Fatalf("MiniMax H3 artifacts=%d, want one primary artifact", len(h3.Artifacts))
	}
	pinned := make(map[string]bool, len(h3.Artifacts[0].Files))
	for _, file := range h3.Artifacts[0].Files {
		pinned[file.Name] = true
	}

	raw, err := recipe.Graph(h3.Service.ComfyUI.Graphs[recipe.ModeTextToVideo])
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]apiGraphNode
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	loaded := map[string]bool{}
	for _, node := range document {
		var folder, input string
		switch node.ClassType {
		case "UNETLoader":
			folder, input = "diffusion_models", "unet_name"
		case "CLIPLoader":
			folder, input = "text_encoders", "clip_name"
		case "VAELoader":
			folder, input = "vae", "vae_name"
		default:
			continue
		}
		name, ok := node.Inputs[input].(string)
		if !ok || name == "" {
			t.Fatalf("%s has no string %s input: %#v", node.ClassType, input, node.Inputs[input])
		}
		path := folder + "/" + name
		if !pinned[path] {
			t.Errorf("%s asks for %s, which the recipe does not pin", node.ClassType, path)
		}
		loaded[path] = true
	}
	if len(loaded) != len(pinned) {
		t.Fatalf("graph loads %d distinct files but the recipe pins %d: loaded=%v pinned=%v", len(loaded), len(pinned), loaded, pinned)
	}
	for path := range pinned {
		if !loaded[path] {
			t.Errorf("recipe pins %s, which the text-to-video graph does not load", path)
		}
	}
}
