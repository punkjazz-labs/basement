package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

func minimalRecipe(id, repository, revision string, expectedBytes int64) recipe.Recipe {
	return recipe.Recipe{
		ID:     id,
		Source: recipe.Source{URL: "https://huggingface.co/" + repository, Revision: revision},
		Artifacts: []recipe.Artifact{
			{Role: "primary", Repository: repository, Revision: revision, ExpectedBytes: expectedBytes},
		},
	}
}

func TestRunCheckModeNoDriftExitsZero(t *testing.T) {
	hfBase := newHFTestServer(t, &hfFixture{
		modelInfo: map[string]hfModelInfo{
			"org/repo": {SHA: pinnedSHA},
		},
	})
	recipes := []recipe.Recipe{minimalRecipe("recipe-a", "org/repo", pinnedSHA, 100)}

	out := filepath.Join(t.TempDir(), "report.json")
	var stdout bytes.Buffer
	code, err := run(recipes, "check", out, "", "", hfBase, "https://api.github.com", &stdout)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	report := readReport(t, out)
	if len(report.Findings) != 0 {
		t.Fatalf("Findings = %+v, want none", report.Findings)
	}
}

func TestRunCheckModeDriftExitsThree(t *testing.T) {
	hfBase := newHFTestServer(t, &hfFixture{
		modelInfo: map[string]hfModelInfo{
			"org/repo": {SHA: newSHA},
		},
		revisionInfo: map[string]hfRevisionInfo{
			"org/repo@" + newSHA: {SHA: newSHA, Siblings: []hfSibling{{RFilename: "model.bin", Size: 200}}},
		},
	})
	recipes := []recipe.Recipe{minimalRecipe("recipe-a", "org/repo", pinnedSHA, 100)}

	out := filepath.Join(t.TempDir(), "report.json")
	var stdout bytes.Buffer
	code, err := run(recipes, "check", out, "", "", hfBase, "https://api.github.com", &stdout)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	report := readReport(t, out)
	if len(report.Findings) != 1 || report.Findings[0].Kind != "artifact" {
		t.Fatalf("Findings = %+v, want one artifact finding", report.Findings)
	}
	if stdout.Len() == 0 {
		t.Error("stdout carries no summary")
	}
}

func TestRunCheckModeOnlyErrorsExitsFour(t *testing.T) {
	hfBase := newHFTestServer(t, &hfFixture{
		modelStatus: map[string]int{"org/repo": http.StatusInternalServerError},
	})
	recipes := []recipe.Recipe{minimalRecipe("recipe-a", "org/repo", pinnedSHA, 100)}

	out := filepath.Join(t.TempDir(), "report.json")
	code, err := run(recipes, "check", out, "", "", hfBase, "https://api.github.com", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 4 {
		t.Fatalf("exit code = %d, want 4", code)
	}
	report := readReport(t, out)
	if len(report.Findings) != 1 || report.Findings[0].Kind != "error" {
		t.Fatalf("Findings = %+v, want one error finding", report.Findings)
	}
}

func TestRunBumpModeAppliesSafeSubsetAndReportsTheRest(t *testing.T) {
	recipesDir := t.TempDir()
	_, _, wholeSnapshotOriginal := copyRecipeFixtureInto(t, recipesDir, "qwen38-27b-nvfp4-1s.yaml")
	wholeSnapshotRecipe, err := recipe.DecodeStrict(wholeSnapshotOriginal)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	wholeIndex, _ := wholeSnapshotRecipe.ArtifactIndex("primary")
	wholeArtifact := wholeSnapshotRecipe.Artifacts[wholeIndex]
	const wholeNewRevision = "7777777777777777777777777777777777777777"

	_, _, perFileOriginal := copyRecipeFixtureInto(t, recipesDir, "qwen38-27b-obliterated-q8-0-1s.yaml")
	perFileRecipe, err := recipe.DecodeStrict(perFileOriginal)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	perFileIndex, _ := perFileRecipe.ArtifactIndex("primary")
	perFileArtifact := perFileRecipe.Artifacts[perFileIndex]
	const perFileNewRevision = "8888888888888888888888888888888888888888"

	fixture := &hfFixture{
		modelInfo: map[string]hfModelInfo{
			wholeArtifact.Repository:   {SHA: wholeNewRevision, CardData: hfCardData{License: "apache-2.0"}},
			perFileArtifact.Repository: {SHA: perFileNewRevision},
		},
		revisionInfo: map[string]hfRevisionInfo{
			wholeArtifact.Repository + "@" + wholeArtifact.Revision: {
				SHA: wholeArtifact.Revision, Siblings: []hfSibling{{RFilename: "LICENSE", Size: 10}},
			},
			wholeArtifact.Repository + "@" + wholeNewRevision: {
				SHA: wholeNewRevision, Siblings: []hfSibling{
					{RFilename: "model.safetensors", Size: wholeArtifact.ExpectedBytes},
					{RFilename: "LICENSE", Size: 10},
				},
			},
		},
	}
	// The per-file recipe's second pinned file (the chat template) is dropped
	// at the new revision, which must refuse that recipe's bump entirely.
	revSiblings := []hfSibling{{RFilename: perFileArtifact.Files[0].Name, Size: perFileArtifact.Files[0].ExpectedBytes}}
	fixture.revisionInfo[perFileArtifact.Repository+"@"+perFileNewRevision] = hfRevisionInfo{SHA: perFileNewRevision, Siblings: revSiblings}

	hfBase := newHFTestServer(t, fixture)
	// wholeSnapshotRecipe's source.url is a real github.com repository (it
	// comes straight from the fixture file), so scanRecipe will try a source
	// check; answer it with the recipe's own pinned revision (no drift) so
	// this test stays about the artifact bump, not the source check, and
	// never touches the real network either way.
	owner, repoName, ok := githubOwnerRepo(wholeSnapshotRecipe.Source.URL)
	if !ok {
		t.Fatalf("fixture source url %s did not parse as a github repository", wholeSnapshotRecipe.Source.URL)
	}
	githubBase := newGitHubTestServer(t, &githubFixture{
		repoInfo: map[string]githubRepoInfo{owner + "/" + repoName: {DefaultBranch: "main"}},
		branchInfo: map[string]githubBranchInfo{
			owner + "/" + repoName + "@main": {Commit: githubCommit{SHA: wholeSnapshotRecipe.Source.Revision}},
		},
	})
	recipes := []recipe.Recipe{wholeSnapshotRecipe, perFileRecipe}

	out := filepath.Join(t.TempDir(), "report.json")
	var stdout bytes.Buffer
	code, err := run(recipes, "bump", out, recipesDir, "", hfBase, githubBase, &stdout)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3 (one recipe still needs judgment)", code)
	}
	report := readReport(t, out)
	if len(report.Bumped) != 1 || report.Bumped[0].RecipeID != wholeSnapshotRecipe.ID {
		t.Fatalf("Bumped = %+v, want only %s bumped", report.Bumped, wholeSnapshotRecipe.ID)
	}
	if len(report.Findings) != 1 || report.Findings[0].RecipeID != perFileRecipe.ID || report.Findings[0].Reason != "needs judgment" {
		t.Fatalf("Findings = %+v, want one needs-judgment finding for %s", report.Findings, perFileRecipe.ID)
	}
	if stdout.Len() == 0 {
		t.Error("stdout carries no changed-files summary")
	}
}

func TestExitCodeRules(t *testing.T) {
	cases := []struct {
		name     string
		findings []Finding
		want     int
	}{
		{"none", nil, 0},
		{"drift only", []Finding{{Kind: "artifact"}}, 3},
		{"error only", []Finding{{Kind: "error"}}, 4},
		{"drift and error", []Finding{{Kind: "artifact"}, {Kind: "error"}}, 3},
	}
	for _, c := range cases {
		if got := exitCode(c.findings); got != c.want {
			t.Errorf("%s: exitCode = %d, want %d", c.name, got, c.want)
		}
	}
}

func readReport(t *testing.T, path string) Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report %s: %v", path, err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode report %s: %v", path, err)
	}
	return report
}

func copyRecipeFixtureInto(t *testing.T, dir, name string) (fullDir, path string, original []byte) {
	t.Helper()
	src := filepath.Join("..", "..", "internal", "recipe", "recipes", name)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture copy: %v", err)
	}
	return dir, path, data
}
