package main

import (
	"fmt"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

// recipeScan is the complete drift result for one recipe: what changed and
// what stayed the same, plus any network failure encountered along the way.
// Both check mode and bump mode read the same structure. Check mode turns
// it straight into a report, bump mode additionally decides, artifact by
// artifact, whether a drift qualifies for the safe subset.
type recipeScan struct {
	Recipe    recipe.Recipe
	Artifacts []artifactDrift
	Source    *sourceDrift // nil when source.url is not a GitHub repository
	Errors    []scanError
}

// artifactDrift is one artifact's comparison between its pinned revision and
// what the Hugging Face API reports today. Drifted is false, and every
// "New*" field is zero, when the pinned and current revisions already match.
type artifactDrift struct {
	Role             string
	Repository       string
	PinnedRevision   string
	CurrentRevision  string
	Drifted          bool
	WholeSnapshot    bool // true when the recipe artifact pins no files (Files is empty)
	PinnedTotalBytes int64
	NewTotalBytes    int64
	NewHasLicense    bool // whether the new revision carries a LICENSE or LICENSE.md file
	LicenceTag       string
	Files            []fileDrift // populated only for a per-file-pinned artifact
}

// fileDrift is one pinned file's comparison between its pinned size and what
// the new revision's tree reports. StillExists is false when the new
// revision's tree carries no file of this exact name at all.
type fileDrift struct {
	Name        string
	PinnedBytes int64
	StillExists bool
	NewBytes    int64
	SameSize    bool
}

type sourceDrift struct {
	URL             string
	PinnedRevision  string
	CurrentRevision string
	Drifted         bool
}

// scanError isolates one recipe's network failure from every other recipe's
// scan: a Hugging Face or GitHub outage while checking one recipe must never
// stop this tool from reporting the recipes it could reach.
type scanError struct {
	Kind    string // "artifact" or "source"
	Role    string // set for kind "artifact"; empty for "source"
	Message string
}

// scanRecipe checks every artifact and, when applicable, the source, for one
// recipe. It never returns an error itself: every failure it meets becomes a
// scanError entry, so one bad network response never keeps the rest of this
// recipe, or any other recipe, from being scanned.
func scanRecipe(r recipe.Recipe, hf *hfClient, gh *githubClient) recipeScan {
	scan := recipeScan{Recipe: r}
	for _, artifact := range r.Artifacts {
		drift, err := scanArtifact(artifact, hf)
		if err != nil {
			scan.Errors = append(scan.Errors, scanError{Kind: "artifact", Role: artifact.Role, Message: err.Error()})
			continue
		}
		scan.Artifacts = append(scan.Artifacts, drift)
	}
	if isGitHubSource(r.Source.URL) {
		drift, err := scanSource(r.Source, gh)
		if err != nil {
			scan.Errors = append(scan.Errors, scanError{Kind: "source", Message: err.Error()})
		} else {
			scan.Source = &drift
		}
	}
	return scan
}

func scanArtifact(a recipe.Artifact, hf *hfClient) (artifactDrift, error) {
	info, err := hf.ModelInfo(a.Repository)
	if err != nil {
		return artifactDrift{}, err
	}
	drift := artifactDrift{
		Role:             a.Role,
		Repository:       a.Repository,
		PinnedRevision:   a.Revision,
		CurrentRevision:  info.SHA,
		WholeSnapshot:    len(a.Files) == 0,
		PinnedTotalBytes: a.ExpectedBytes,
		LicenceTag:       info.licenceTag(),
	}
	if info.SHA == a.Revision {
		return drift, nil
	}
	drift.Drifted = true
	revision, err := hf.RevisionInfo(a.Repository, info.SHA)
	if err != nil {
		return artifactDrift{}, err
	}
	drift.NewTotalBytes = revision.totalBytes()
	drift.NewHasLicense = revision.hasLicenseFile()
	if !drift.WholeSnapshot {
		for _, file := range a.Files {
			fd := fileDrift{Name: file.Name, PinnedBytes: file.ExpectedBytes}
			if sibling, ok := revision.sibling(file.Name); ok {
				fd.StillExists = true
				fd.NewBytes = sibling.Size
				fd.SameSize = sibling.Size == file.ExpectedBytes
			}
			drift.Files = append(drift.Files, fd)
		}
	}
	return drift, nil
}

func scanSource(s recipe.Source, gh *githubClient) (sourceDrift, error) {
	owner, repo, ok := githubOwnerRepo(s.URL)
	if !ok {
		return sourceDrift{}, fmt.Errorf("source url %s is not a recognizable github.com repository", s.URL)
	}
	repoInfo, err := gh.RepoInfo(owner, repo)
	if err != nil {
		return sourceDrift{}, err
	}
	branch, err := gh.BranchInfo(owner, repo, repoInfo.DefaultBranch)
	if err != nil {
		return sourceDrift{}, err
	}
	return sourceDrift{
		URL:             s.URL,
		PinnedRevision:  s.Revision,
		CurrentRevision: branch.Commit.SHA,
		Drifted:         branch.Commit.SHA != s.Revision,
	}, nil
}
