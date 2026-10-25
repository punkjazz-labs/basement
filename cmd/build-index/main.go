// Command build-index builds index.json, the unsigned document at the heart
// of the remote recipe feed (ADR 0009), from the recipes embedded in this
// binary. The binary is the source of truth for what gets published: there
// is no flag to point it at a directory of YAML files instead, because an
// index that did not come from a real build would defeat the point of
// publishing one at all: a manager fetching it should be able to trust that
// every recipe inside it once passed the same validator this binary itself
// was built against.
//
// Usage:
//
//	go run ./cmd/build-index -out index.json
//
// The output is unsigned. Pair this with cmd/sign-index (or `make
// sign-index`) to produce the detached signature before publishing; see
// docs/RECIPE-FEED.md for the complete publish flow.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

func main() {
	out := flag.String("out", "index.json", "path to write the built index to")
	generatedAt := flag.String("generated-at", "", "optional RFC3339 timestamp for generated_at; default is now, UTC. Set this to reproduce a publish byte for byte")
	flag.Parse()
	summary, err := run(*out, *generatedAt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build-index:", err)
		os.Exit(1)
	}
	fmt.Println(summary)
}

// run loads the embedded recipe pack, re-validates it, and writes the
// resulting index atomically to outPath. It touches outPath only after every
// check has already passed, so a run that fails for any reason leaves no
// output file behind: a partial or invalid index is worse than none, because
// nothing downstream distinguishes "not published yet" from "published but
// wrong".
func run(outPath, generatedAtFlag string) (string, error) {
	recipes, err := recipe.Builtin()
	if err != nil {
		return "", fmt.Errorf("load embedded recipes: %w", err)
	}
	// recipe.Builtin already validates every recipe as it decodes
	// (recipe.DecodeStrict calls recipe.Validate), but the index this tool
	// produces is a promise made to every manager that fetches it, so that
	// promise is checked again here, explicitly, rather than trusted as a
	// side effect of how the embedded pack happened to load.
	for _, r := range recipes {
		if err := recipe.Validate(r); err != nil {
			return "", fmt.Errorf("recipe %s failed validation: %w", r.ID, err)
		}
	}
	generated, err := resolveGeneratedAt(generatedAtFlag)
	if err != nil {
		return "", err
	}
	idx := recipe.Index{
		SchemaVersion: recipe.IndexSchemaVersion,
		GeneratedAt:   generated,
		Recipes:       recipes,
	}
	body, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal index: %w", err)
	}
	body = append(body, '\n')
	if err := writeAtomic(outPath, body); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s: %d recipe(s)", outPath, len(recipes)), nil
}

// resolveGeneratedAt returns time.Now in UTC when flagValue is empty, and
// otherwise parses flagValue as RFC3339. The flag exists so a publish can be
// reproduced byte for byte: rerunning this tool with the same embedded
// binary and the same -generated-at must write the same bytes.
func resolveGeneratedAt(flagValue string) (time.Time, error) {
	if flagValue == "" {
		return time.Now().UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339, flagValue)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse -generated-at: %w", err)
	}
	return parsed.UTC(), nil
}

// writeAtomic writes data to a temporary file beside path and renames it
// into place, the same pattern internal/recipefeed uses for its cache: a
// crash or a concurrent read mid-write can never observe a truncated index.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return fmt.Errorf("write temp index file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename index file into place: %w", err)
	}
	return nil
}
