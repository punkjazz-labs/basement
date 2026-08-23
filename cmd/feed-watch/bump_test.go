package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

// copyRecipeFixture copies a real recipe file from internal/recipe/recipes
// into a fresh temp directory, so a bump test edits a throwaway copy and
// never the repository's own tracked file. Tests run with the package
// directory as their working directory, so the fixture is two levels up.
func copyRecipeFixture(t *testing.T, name string) (dir, path string, original []byte) {
	t.Helper()
	src := filepath.Join("..", "..", "internal", "recipe", "recipes", name)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	dir = t.TempDir()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture copy: %v", err)
	}
	return dir, path, data
}

// assertOnlyExpectedLinesChanged fails the test unless every changed line
// (before and after) appears in allowed, and the two texts have the same
// number of lines. This is how the bump tests confirm a recipe's comments
// survive byte for byte outside the lines feed-watch deliberately edits.
func assertOnlyExpectedLinesChanged(t *testing.T, original, updated string, allowed map[string]bool) {
	t.Helper()
	origLines := strings.Split(original, "\n")
	newLines := strings.Split(updated, "\n")
	if len(origLines) != len(newLines) {
		t.Fatalf("line count changed: %d -> %d lines", len(origLines), len(newLines))
	}
	for i := range origLines {
		if origLines[i] == newLines[i] {
			continue
		}
		if !allowed[origLines[i]] {
			t.Errorf("line %d changed unexpectedly, old content: %q", i+1, origLines[i])
		}
		if !allowed[newLines[i]] {
			t.Errorf("line %d changed unexpectedly, new content: %q", i+1, newLines[i])
		}
	}
}

func TestBumpWholeSnapshotEndToEnd(t *testing.T) {
	dir, path, original := copyRecipeFixture(t, "qwen38-27b-nvfp4-1s.yaml")
	r, err := recipe.DecodeStrict(original)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	index, ok := r.ArtifactIndex("primary")
	if !ok {
		t.Fatal("fixture has no primary artifact")
	}
	artifact := r.Artifacts[index]
	const newRevision = "3333333333333333333333333333333333333333"
	newTotal := artifact.ExpectedBytes + 12345 + 100

	base := newHFTestServer(t, &hfFixture{
		modelInfo: map[string]hfModelInfo{
			artifact.Repository: {SHA: newRevision, CardData: hfCardData{License: "apache-2.0"}},
		},
		revisionInfo: map[string]hfRevisionInfo{
			artifact.Repository + "@" + artifact.Revision: {
				SHA: artifact.Revision, Siblings: []hfSibling{{RFilename: "LICENSE", Size: 100}},
			},
			artifact.Repository + "@" + newRevision: {
				SHA: newRevision, Siblings: []hfSibling{
					{RFilename: "model.safetensors", Size: artifact.ExpectedBytes + 12345},
					{RFilename: "LICENSE", Size: 100},
				},
			},
		},
	})
	hf := newHFClient(base)

	drift, err := scanArtifact(artifact, hf)
	if err != nil {
		t.Fatalf("scanArtifact: %v", err)
	}
	if !drift.Drifted {
		t.Fatal("scanArtifact: want drift when the fixture reports a new sha")
	}

	scan := recipeScan{Recipe: r, Artifacts: []artifactDrift{drift}}
	findings, bumped, err := applyBumps([]recipeScan{scan}, dir, hf)
	if err != nil {
		t.Fatalf("applyBumps: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %+v, want none: a clean whole-snapshot drift is the safe subset", findings)
	}
	if len(bumped) != 1 {
		t.Fatalf("bumped = %+v, want exactly one recipe bumped", bumped)
	}
	if bumped[0].OldVersion != r.Version || bumped[0].NewVersion != r.Version+1 {
		t.Fatalf("bumped[0] version = %d -> %d, want %d -> %d", bumped[0].OldVersion, bumped[0].NewVersion, r.Version, r.Version+1)
	}

	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read updated file: %v", err)
	}
	decoded, err := recipe.DecodeStrict(updated)
	if err != nil {
		t.Fatalf("bumped recipe fails recipe.DecodeStrict: %v", err)
	}
	if decoded.Version != r.Version+1 {
		t.Errorf("Version = %d, want %d", decoded.Version, r.Version+1)
	}
	newIndex, _ := decoded.ArtifactIndex("primary")
	newArtifact := decoded.Artifacts[newIndex]
	if newArtifact.Revision != newRevision {
		t.Errorf("Revision = %s, want %s", newArtifact.Revision, newRevision)
	}
	if newArtifact.ExpectedBytes != newTotal {
		t.Errorf("ExpectedBytes = %d, want %d", newArtifact.ExpectedBytes, newTotal)
	}
	if !strings.Contains(newArtifact.LicenceURL, newRevision) {
		t.Errorf("LicenceURL = %s, want it to carry the new revision", newArtifact.LicenceURL)
	}

	assertOnlyExpectedLinesChanged(t, string(original), string(updated), map[string]bool{
		fmt.Sprintf("version: %d", r.Version):                         true,
		fmt.Sprintf("version: %d", r.Version+1):                       true,
		"    revision: " + artifact.Revision:                          true,
		"    revision: " + newRevision:                                true,
		fmt.Sprintf("    expected_bytes: %d", artifact.ExpectedBytes): true,
		fmt.Sprintf("    expected_bytes: %d", newTotal):               true,
		"    licence_url: " + artifact.LicenceURL:                     true,
		"    licence_url: " + newArtifact.LicenceURL:                  true,
	})
}

func TestBumpRefusesOnLicenceChange(t *testing.T) {
	dir, path, original := copyRecipeFixture(t, "qwen38-27b-nvfp4-1s.yaml")
	r, err := recipe.DecodeStrict(original)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	index, _ := r.ArtifactIndex("primary")
	artifact := r.Artifacts[index]
	const newRevision = "4444444444444444444444444444444444444444"

	base := newHFTestServer(t, &hfFixture{
		modelInfo: map[string]hfModelInfo{
			// The repository's own recorded licence is Apache-2.0; the live
			// API now reports MIT, so the bump must be refused.
			artifact.Repository: {SHA: newRevision, CardData: hfCardData{License: "MIT"}},
		},
		revisionInfo: map[string]hfRevisionInfo{
			artifact.Repository + "@" + newRevision: {
				SHA: newRevision, Siblings: []hfSibling{{RFilename: "model.safetensors", Size: artifact.ExpectedBytes}},
			},
		},
	})
	hf := newHFClient(base)

	drift, err := scanArtifact(artifact, hf)
	if err != nil {
		t.Fatalf("scanArtifact: %v", err)
	}
	scan := recipeScan{Recipe: r, Artifacts: []artifactDrift{drift}}
	findings, bumped, err := applyBumps([]recipeScan{scan}, dir, hf)
	if err != nil {
		t.Fatalf("applyBumps: %v", err)
	}
	if len(bumped) != 0 {
		t.Fatalf("bumped = %+v, want nothing bumped when the licence tag changed", bumped)
	}
	if len(findings) != 1 || findings[0].Reason != "needs judgment" || !strings.Contains(findings[0].Details, "licence changed") {
		t.Fatalf("findings = %+v, want one needs-judgment finding about the licence change", findings)
	}

	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(unchanged) != string(original) {
		t.Error("the recipe file was modified despite the licence mismatch")
	}
}

func TestBumpPerFilePinRefusesOnSizeChange(t *testing.T) {
	dir, path, original := copyRecipeFixture(t, "qwen38-27b-obliterated-q8-0-1s.yaml")
	r, err := recipe.DecodeStrict(original)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	index, _ := r.ArtifactIndex("primary")
	artifact := r.Artifacts[index]
	if len(artifact.Files) != 2 {
		t.Fatalf("fixture pins %d files, want 2 (the qwen38 obliterated recipe pins a gguf and a chat template)", len(artifact.Files))
	}
	const newRevision = "5555555555555555555555555555555555555555"

	siblings := make([]hfSibling, len(artifact.Files))
	for i, f := range artifact.Files {
		size := f.ExpectedBytes
		if i == 0 {
			size++ // the gguf's size moved at the new revision
		}
		siblings[i] = hfSibling{RFilename: f.Name, Size: size}
	}
	base := newHFTestServer(t, &hfFixture{
		modelInfo: map[string]hfModelInfo{
			artifact.Repository: {SHA: newRevision},
		},
		revisionInfo: map[string]hfRevisionInfo{
			artifact.Repository + "@" + newRevision: {SHA: newRevision, Siblings: siblings},
		},
	})
	hf := newHFClient(base)

	drift, err := scanArtifact(artifact, hf)
	if err != nil {
		t.Fatalf("scanArtifact: %v", err)
	}
	scan := recipeScan{Recipe: r, Artifacts: []artifactDrift{drift}}
	findings, bumped, err := applyBumps([]recipeScan{scan}, dir, hf)
	if err != nil {
		t.Fatalf("applyBumps: %v", err)
	}
	if len(bumped) != 0 {
		t.Fatalf("bumped = %+v, want nothing bumped when a pinned file changed size", bumped)
	}
	if len(findings) != 1 || findings[0].Reason != "needs judgment" || !strings.Contains(findings[0].Details, "changed size") {
		t.Fatalf("findings = %+v, want one needs-judgment finding about the changed file size", findings)
	}

	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(unchanged) != string(original) {
		t.Error("the recipe file was modified despite the pinned file changing size")
	}
}

func TestBumpPerFilePinEndToEndWhenFilesAreUnchanged(t *testing.T) {
	dir, path, original := copyRecipeFixture(t, "qwen38-27b-obliterated-q8-0-1s.yaml")
	r, err := recipe.DecodeStrict(original)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	index, _ := r.ArtifactIndex("primary")
	artifact := r.Artifacts[index]
	const newRevision = "6666666666666666666666666666666666666666"

	siblings := make([]hfSibling, len(artifact.Files))
	for i, f := range artifact.Files {
		siblings[i] = hfSibling{RFilename: f.Name, Size: f.ExpectedBytes}
	}
	base := newHFTestServer(t, &hfFixture{
		modelInfo: map[string]hfModelInfo{
			artifact.Repository: {SHA: newRevision},
		},
		revisionInfo: map[string]hfRevisionInfo{
			artifact.Repository + "@" + newRevision: {SHA: newRevision, Siblings: siblings},
		},
	})
	hf := newHFClient(base)

	drift, err := scanArtifact(artifact, hf)
	if err != nil {
		t.Fatalf("scanArtifact: %v", err)
	}
	scan := recipeScan{Recipe: r, Artifacts: []artifactDrift{drift}}
	findings, bumped, err := applyBumps([]recipeScan{scan}, dir, hf)
	if err != nil {
		t.Fatalf("applyBumps: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %+v, want none: every pinned file kept its name and size", findings)
	}
	if len(bumped) != 1 {
		t.Fatalf("bumped = %+v, want exactly one recipe bumped", bumped)
	}

	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read updated file: %v", err)
	}
	decoded, err := recipe.DecodeStrict(updated)
	if err != nil {
		t.Fatalf("bumped recipe fails recipe.DecodeStrict: %v", err)
	}
	newIndex, _ := decoded.ArtifactIndex("primary")
	if decoded.Artifacts[newIndex].Revision != newRevision {
		t.Errorf("Revision = %s, want %s", decoded.Artifacts[newIndex].Revision, newRevision)
	}
	if decoded.Artifacts[newIndex].ExpectedBytes != artifact.ExpectedBytes {
		t.Errorf("ExpectedBytes = %d, want unchanged %d: file sizes did not move", decoded.Artifacts[newIndex].ExpectedBytes, artifact.ExpectedBytes)
	}
	if decoded.Version != r.Version+1 {
		t.Errorf("Version = %d, want %d", decoded.Version, r.Version+1)
	}
}

func TestIndexRecipeFilesMatchesByDecodedID(t *testing.T) {
	dir, _, _ := copyRecipeFixture(t, "qwen38-27b-nvfp4-1s.yaml")
	index, err := indexRecipeFiles(dir)
	if err != nil {
		t.Fatalf("indexRecipeFiles: %v", err)
	}
	if _, ok := index["qwen38-27b-nvfp4-1s"]; !ok {
		t.Fatalf("index = %+v, want the fixture's own id as a key", index)
	}
}
