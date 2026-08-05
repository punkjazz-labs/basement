package recipe

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed graphs
var graphFiles embed.FS

var embeddedGraphSource = mustSubGraphs()

// GraphSource is an overlay for workflow fixtures. Production never
// reassigns it, so it reads the embedded set directly. Tests can replace it
// with an in-memory filesystem without making the recipes shipped in the
// binary lose access to their own embedded graphs: Graph falls back to the
// immutable embedded set when the overlay does not contain a requested name.
var GraphSource fs.FS = embeddedGraphSource

func mustSubGraphs() fs.FS {
	sub, err := fs.Sub(graphFiles, "graphs")
	if err != nil {
		panic("embedded graphs directory is unreadable: " + err.Error())
	}
	return sub
}

// The generation modes a recipe may declare. Each is a path through the
// generation driver rather than a label, so the set is closed: a mode nothing
// implements would be a control the console could offer and basement could
// not honour.
const (
	ModeTextToVideo  = "text_to_video"
	ModeImageToVideo = "image_to_video"
)

var allowedGenerationModes = map[string]bool{ModeTextToVideo: true, ModeImageToVideo: true}

// GenerationModeNames lists the declarable modes in a stable order, for error
// messages that name what was allowed instead of only what was wrong.
func GenerationModeNames() []string {
	names := make([]string, 0, len(allowedGenerationModes))
	for name := range allowedGenerationModes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// The tokens a graph is parameterised by. See graphs/README.md for the
// contract; the short version is that a token is always a whole JSON string
// value and is replaced by a typed one.
const (
	GraphPromptToken = "{{PROMPT}}"
	GraphSeedToken   = "{{SEED}}"
	GraphFramesToken = "{{FRAMES}}"
	GraphWidthToken  = "{{WIDTH}}"
	GraphHeightToken = "{{HEIGHT}}"
	GraphImageToken  = "{{IMAGE}}"
)

// RequiredGraphTokens is the exact token set a mode's graph must carry —
// exact, not minimum. A missing token is a control the user turns and nothing
// reads; an extra one is a placeholder nothing replaces, which would reach
// ComfyUI as the literal text "{{IMAGE}}".
func RequiredGraphTokens(mode string) []string {
	base := []string{GraphPromptToken, GraphSeedToken, GraphFramesToken, GraphWidthToken, GraphHeightToken}
	if mode == ModeImageToVideo {
		return append(base, GraphImageToken)
	}
	return base
}

// Graph reads one pinned workflow by the name a recipe declares. The name has
// already been validated as a plain file name; it is checked again here
// because this is the call that touches the filesystem.
func Graph(name string) ([]byte, error) {
	if err := validateGraphName(name); err != nil {
		return nil, err
	}
	data, err := fs.ReadFile(GraphSource, name)
	if errors.Is(err, fs.ErrNotExist) {
		data, err = fs.ReadFile(embeddedGraphSource, name)
	}
	if err != nil {
		return nil, fmt.Errorf("read workflow graph %s: %w", name, err)
	}
	return data, nil
}

// validateGraphName keeps a graph name a bare file name inside the embedded
// set: it is joined to a filesystem path, so a directory component or a
// climbing segment would read a file the product never shipped as a graph.
func validateGraphName(name string) error {
	if name == "" {
		return errors.New("graph file name is empty")
	}
	if name != path.Base(name) || filepath.IsAbs(name) || strings.Contains(name, "\\") || name == "." || name == ".." {
		return errors.New("graph file name must be a plain file name")
	}
	if !strings.HasSuffix(name, ".json") {
		return errors.New("graph file name must end in .json")
	}
	return nil
}

// GraphInputs are the only things a caller may put into a graph. There is no
// field here for a node, a model path or a sampler setting, and that is the
// point: the workflow is pinned, and a request chooses a prompt and a size,
// never a program.
type GraphInputs struct {
	Prompt string
	Seed   int64
	Frames int
	Width  int
	Height int
	// Image is the staged source file name inside the container's input
	// directory. It is used by image_to_video only and must be empty
	// otherwise, so a text-to-video graph cannot be handed a file name.
	Image string
}

// RenderGraph substitutes a graph's tokens with a request's values and
// returns the ComfyUI API-format document to submit. Substitution is by whole
// value: a JSON string that is exactly a token becomes the typed value, and
// every other string in the document is left untouched, so a prompt that
// happens to contain "{{SEED}}" is prompt text and not a substitution.
//
// The document is re-encoded from the parsed form rather than patched as
// text. A prompt is arbitrary user input, and splicing it into JSON by string
// replacement is how a quote in a prompt becomes a malformed workflow.
func RenderGraph(raw []byte, inputs GraphInputs) ([]byte, error) {
	document, err := decodeGraph(raw)
	if err != nil {
		return nil, err
	}
	replacements := map[string]any{
		GraphPromptToken: inputs.Prompt,
		GraphSeedToken:   inputs.Seed,
		GraphFramesToken: inputs.Frames,
		GraphWidthToken:  inputs.Width,
		GraphHeightToken: inputs.Height,
	}
	if inputs.Image != "" {
		replacements[GraphImageToken] = inputs.Image
	}
	rendered := substituteGraph(document, replacements)
	encoded, err := json.Marshal(rendered)
	if err != nil {
		return nil, fmt.Errorf("encode rendered workflow graph: %w", err)
	}
	return encoded, nil
}

// GraphTokens reports every token a graph carries, so the validator can hold
// a shipped graph to its mode's contract before anything runs.
func GraphTokens(raw []byte) ([]string, error) {
	document, err := decodeGraph(raw)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	collectGraphTokens(document, found)
	tokens := make([]string, 0, len(found))
	for token := range found {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	return tokens, nil
}

// graphTokenSet is every token this package knows how to substitute. A string
// shaped like a token but not in this set is ordinary text and is left alone,
// which is why the validator's check is against this set rather than against
// anything that looks like a placeholder.
var graphTokenSet = map[string]bool{
	GraphPromptToken: true, GraphSeedToken: true, GraphFramesToken: true,
	GraphWidthToken: true, GraphHeightToken: true, GraphImageToken: true,
}

func decodeGraph(raw []byte) (any, error) {
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("workflow graph is not valid JSON: %w", err)
	}
	if _, ok := document.(map[string]any); !ok {
		return nil, errors.New("workflow graph must be a JSON object in ComfyUI API format")
	}
	return document, nil
}

func substituteGraph(node any, replacements map[string]any) any {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			value[key] = substituteGraph(child, replacements)
		}
		return value
	case []any:
		for index, child := range value {
			value[index] = substituteGraph(child, replacements)
		}
		return value
	case string:
		if replacement, ok := replacements[value]; ok {
			return replacement
		}
		return value
	default:
		return node
	}
}

func collectGraphTokens(node any, found map[string]bool) {
	switch value := node.(type) {
	case map[string]any:
		for _, child := range value {
			collectGraphTokens(child, found)
		}
	case []any:
		for _, child := range value {
			collectGraphTokens(child, found)
		}
	case string:
		if graphTokenSet[value] {
			found[value] = true
		}
	}
}
