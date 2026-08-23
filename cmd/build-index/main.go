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
//	go run ./cmd/build-index -out index.json -revoked revoked.json
//
// The output is unsigned. Pair this with cmd/sign-index (or `make
// sign-index`) to produce the detached signature before publishing; see
// docs/RECIPE-FEED.md for the complete publish flow.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

func main() {
	out := flag.String("out", "index.json", "path to write the built index to")
	revokedPath := flag.String("revoked", "", "optional path to a JSON file listing revocations to fold into the index (see docs/RECIPE-FEED.md)")
	generatedAt := flag.String("generated-at", "", "optional RFC3339 timestamp for generated_at; default is now, UTC. Set this to reproduce a publish byte for byte")
	flag.Parse()
	summary, err := run(*out, *revokedPath, *generatedAt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build-index:", err)
		os.Exit(1)
	}
	fmt.Println(summary)
}

// run loads the embedded recipe pack, re-validates it, folds in whatever
// revocations -revoked names, and writes the resulting index atomically to
// outPath. It touches outPath only after every check has already passed, so
// a run that fails for any reason leaves no output file behind: a partial or
// invalid index is worse than none, because nothing downstream distinguishes
// "not published yet" from "published but wrong".
func run(outPath, revokedPath, generatedAtFlag string) (string, error) {
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
	revocations, err := loadRevocations(revokedPath)
	if err != nil {
		return "", err
	}
	idx := recipe.Index{
		SchemaVersion: recipe.IndexSchemaVersion,
		GeneratedAt:   generated,
		Recipes:       recipes,
		Revoked:       revocations,
	}
	body, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal index: %w", err)
	}
	body = append(body, '\n')
	if err := writeAtomic(outPath, body); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s: %d recipe(s), %d revocation(s)", outPath, len(recipes), len(revocations)), nil
}

// resolveGeneratedAt returns time.Now in UTC when flagValue is empty, and
// otherwise parses flagValue as RFC3339. The flag exists so a publish can be
// reproduced byte for byte: rerunning this tool with the same embedded
// binary, the same revoked file, and the same -generated-at must write the
// same bytes.
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

// loadRevocations reads and validates the optional -revoked file. It holds
// the file to the same rules recipe.VerifyAndParseIndex holds the published
// revoked array to (unknown fields refused, every field required, id and
// version naming exactly one recipe version), plus one this tool alone can
// check before anything is published: no two entries may name the same
// (id, version) pair, since two revocations for one recipe version is
// either a duplicate that adds nothing or a contradiction, and either way
// belongs to whoever is drafting the file, not to every manager that fetches
// the result.
func loadRevocations(path string) ([]recipe.Revocation, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read revoked file: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var revocations []recipe.Revocation
	if err := decoder.Decode(&revocations); err != nil {
		return nil, fmt.Errorf("decode revoked file: %w", err)
	}
	seen := make(map[string]bool, len(revocations))
	for i, r := range revocations {
		if strings.TrimSpace(r.ID) == "" {
			return nil, fmt.Errorf("revoked[%d]: id is required", i)
		}
		if r.Version < 1 {
			return nil, fmt.Errorf("revoked[%d]: version must name one exact recipe version (>= 1)", i)
		}
		if strings.TrimSpace(r.Reason) == "" {
			return nil, fmt.Errorf("revoked[%d]: reason is required and must be human-readable", i)
		}
		if r.RevokedAt.IsZero() {
			return nil, fmt.Errorf("revoked[%d]: revoked_at is required", i)
		}
		key := fmt.Sprintf("%s@%d", r.ID, r.Version)
		if seen[key] {
			return nil, fmt.Errorf("revoked[%d]: duplicate revocation for %s version %d", i, r.ID, r.Version)
		}
		seen[key] = true
	}
	return revocations, nil
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
