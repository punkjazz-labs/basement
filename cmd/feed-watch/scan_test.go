package main

import (
	"net/http"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

const pinnedSHA = "1111111111111111111111111111111111111111"
const newSHA = "2222222222222222222222222222222222222222"

func TestScanArtifactNoDrift(t *testing.T) {
	base := newHFTestServer(t, &hfFixture{
		modelInfo: map[string]hfModelInfo{
			"org/repo": {SHA: pinnedSHA},
		},
	})
	hf := newHFClient(base)
	artifact := recipe.Artifact{Role: "primary", Repository: "org/repo", Revision: pinnedSHA, ExpectedBytes: 100}

	drift, err := scanArtifact(artifact, hf)
	if err != nil {
		t.Fatalf("scanArtifact: %v", err)
	}
	if drift.Drifted {
		t.Fatalf("drift = %+v, want Drifted false when the pinned and current sha match", drift)
	}
}

func TestScanArtifactWholeSnapshotDrift(t *testing.T) {
	base := newHFTestServer(t, &hfFixture{
		modelInfo: map[string]hfModelInfo{
			"org/repo": {SHA: newSHA, CardData: struct {
				License string `json:"license"`
			}{License: "apache-2.0"}},
		},
		revisionInfo: map[string]hfRevisionInfo{
			"org/repo@" + newSHA: {SHA: newSHA, Siblings: []hfSibling{
				{RFilename: "model.safetensors", Size: 900},
				{RFilename: "LICENSE", Size: 50},
			}},
		},
	})
	hf := newHFClient(base)
	artifact := recipe.Artifact{Role: "primary", Repository: "org/repo", Revision: pinnedSHA, ExpectedBytes: 800}

	drift, err := scanArtifact(artifact, hf)
	if err != nil {
		t.Fatalf("scanArtifact: %v", err)
	}
	if !drift.Drifted || drift.CurrentRevision != newSHA {
		t.Fatalf("drift = %+v, want Drifted true with CurrentRevision %s", drift, newSHA)
	}
	if drift.NewTotalBytes != 950 {
		t.Fatalf("NewTotalBytes = %d, want 950", drift.NewTotalBytes)
	}
	if !drift.NewHasLicense {
		t.Fatal("NewHasLicense = false, want true: the fixture's siblings carry LICENSE")
	}
	if drift.LicenceTag != "apache-2.0" {
		t.Fatalf("LicenceTag = %q, want apache-2.0", drift.LicenceTag)
	}
}

func TestScanArtifactPerFileDrift(t *testing.T) {
	base := newHFTestServer(t, &hfFixture{
		modelInfo: map[string]hfModelInfo{
			"org/repo": {SHA: newSHA},
		},
		revisionInfo: map[string]hfRevisionInfo{
			"org/repo@" + newSHA: {SHA: newSHA, Siblings: []hfSibling{
				{RFilename: "weights.gguf", Size: 500}, // unchanged
				// "extra.gguf" is pinned but missing at the new revision.
			}},
		},
	})
	hf := newHFClient(base)
	artifact := recipe.Artifact{
		Role: "primary", Repository: "org/repo", Revision: pinnedSHA, ExpectedBytes: 600,
		Files: []recipe.ArtifactFile{
			{Name: "weights.gguf", ExpectedBytes: 500},
			{Name: "extra.gguf", ExpectedBytes: 100},
		},
	}

	drift, err := scanArtifact(artifact, hf)
	if err != nil {
		t.Fatalf("scanArtifact: %v", err)
	}
	if len(drift.Files) != 2 {
		t.Fatalf("Files = %+v, want 2 entries", drift.Files)
	}
	if !drift.Files[0].StillExists || !drift.Files[0].SameSize {
		t.Errorf("weights.gguf = %+v, want StillExists and SameSize", drift.Files[0])
	}
	if drift.Files[1].StillExists {
		t.Errorf("extra.gguf = %+v, want StillExists false", drift.Files[1])
	}
}

func TestScanRecipeIsolatesNetworkErrorsAcrossArtifacts(t *testing.T) {
	base := newHFTestServer(t, &hfFixture{
		modelInfo: map[string]hfModelInfo{
			"org/good": {SHA: pinnedSHA},
			// "org/bad" carries no fixture entry, so the server answers 404.
		},
	})
	hf := newHFClient(base)
	gh := newGitHubClient(newGitHubTestServer(t, &githubFixture{}))

	r := recipe.Recipe{
		ID:     "two-artifact-recipe",
		Source: recipe.Source{URL: "https://huggingface.co/org/good", Revision: pinnedSHA},
		Artifacts: []recipe.Artifact{
			{Role: "primary", Repository: "org/good", Revision: pinnedSHA, ExpectedBytes: 100},
			{Role: "drafter", Repository: "org/bad", Revision: pinnedSHA, ExpectedBytes: 100},
		},
	}

	scan := scanRecipe(r, hf, gh)
	if len(scan.Artifacts) != 1 || scan.Artifacts[0].Role != "primary" {
		t.Fatalf("Artifacts = %+v, want exactly the primary artifact scanned", scan.Artifacts)
	}
	if len(scan.Errors) != 1 || scan.Errors[0].Kind != "artifact" || scan.Errors[0].Role != "drafter" {
		t.Fatalf("Errors = %+v, want one artifact error for role drafter", scan.Errors)
	}
}

func TestScanSourceDrift(t *testing.T) {
	base := newGitHubTestServer(t, &githubFixture{
		repoInfo: map[string]githubRepoInfo{
			"owner/repo": {DefaultBranch: "main"},
		},
		branchInfo: map[string]githubBranchInfo{
			"owner/repo@main": {Commit: struct {
				SHA string `json:"sha"`
			}{SHA: newSHA}},
		},
	})
	gh := newGitHubClient(base)

	drift, err := scanSource(recipe.Source{URL: "https://github.com/owner/repo", Revision: pinnedSHA}, gh)
	if err != nil {
		t.Fatalf("scanSource: %v", err)
	}
	if !drift.Drifted || drift.CurrentRevision != newSHA {
		t.Fatalf("drift = %+v, want Drifted true with CurrentRevision %s", drift, newSHA)
	}
}

func TestIsGitHubSourceOnlyMatchesGitHubHost(t *testing.T) {
	cases := map[string]bool{
		"https://github.com/owner/repo":      true,
		"https://huggingface.co/owner/model": false,
		"not a url at all":                   false,
	}
	for url, want := range cases {
		if got := isGitHubSource(url); got != want {
			t.Errorf("isGitHubSource(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestScanArtifactPropagatesHTTPErrors(t *testing.T) {
	base := newHFTestServer(t, &hfFixture{
		modelStatus: map[string]int{"org/repo": http.StatusInternalServerError},
	})
	hf := newHFClient(base)
	artifact := recipe.Artifact{Role: "primary", Repository: "org/repo", Revision: pinnedSHA, ExpectedBytes: 100}

	if _, err := scanArtifact(artifact, hf); err == nil {
		t.Fatal("scanArtifact: want an error when the model info endpoint returns 500")
	}
}
