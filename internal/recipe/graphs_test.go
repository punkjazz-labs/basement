package recipe_test

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/recipe/recipetest"
)

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
