package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

// evaluateBump decides whether one drifted artifact qualifies for the safe
// subset feed-watch may apply without a maintainer: a whole-snapshot
// artifact whose licence tag has not changed and whose new revision still
// carries a LICENSE file if the old one did, or a per-file-pinned artifact
// whose every pinned file is unchanged in name and size. The caller must
// only pass a drifted artifact; nothing here checks Drifted itself.
func evaluateBump(a recipe.Artifact, drift artifactDrift, hf *hfClient) (edit artifactEdit, reason string, eligible bool) {
	base := artifactEdit{
		Role:             a.Role,
		OldRevision:      a.Revision,
		NewRevision:      drift.CurrentRevision,
		OldExpectedBytes: a.ExpectedBytes,
		OldLicenceURL:    a.LicenceURL,
	}
	if drift.WholeSnapshot {
		if !licenceMatches(a.Licence, drift.LicenceTag) {
			return artifactEdit{}, fmt.Sprintf(
				"licence changed: the recipe pins %s but the repository now reports %s",
				a.Licence, orNA(drift.LicenceTag),
			), false
		}
		oldRevisionInfo, err := hf.RevisionInfo(a.Repository, a.Revision)
		if err != nil {
			return artifactEdit{}, fmt.Sprintf("could not confirm the old revision's LICENSE file: %v", err), false
		}
		if oldRevisionInfo.hasLicenseFile() && !drift.NewHasLicense {
			return artifactEdit{}, "the new revision no longer carries a LICENSE or LICENSE.md file", false
		}
		base.UpdateExpectedBytes = true
		base.NewExpectedBytes = drift.NewTotalBytes
		return base, "", true
	}
	for _, file := range drift.Files {
		if !file.StillExists {
			return artifactEdit{}, fmt.Sprintf("pinned file %s no longer exists at the new revision", file.Name), false
		}
		if !file.SameSize {
			return artifactEdit{}, fmt.Sprintf(
				"pinned file %s changed size: was %d bytes, now %d bytes",
				file.Name, file.PinnedBytes, file.NewBytes,
			), false
		}
	}
	return base, "", true
}

// licenceMatches is the case-insensitive comparison the whole-snapshot rule
// requires (apache-2.0 == Apache-2.0). An empty API tag can never match: a
// licence this tool cannot read is a licence it must not wave through.
func licenceMatches(recipeLicence, apiLicenceTag string) bool {
	a := strings.TrimSpace(recipeLicence)
	b := strings.TrimSpace(apiLicenceTag)
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(a, b)
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}

// applyBumps runs the safe subset over every recipe's scan result: eligible
// artifact drifts are written to that recipe's on-disk YAML file as one
// version bump; everything else becomes a Finding tagged "needs judgment"
// for a maintainer session to read. It returns a fatal error only when the
// recipes directory itself cannot be read at all, a failure that touches
// every recipe alike, unlike a single recipe's own file or network trouble,
// which is isolated into a Finding the same way scanning already isolates
// network failures.
func applyBumps(scans []recipeScan, recipesDir string, hf *hfClient) ([]Finding, []BumpResult, error) {
	fileIndex, err := indexRecipeFiles(recipesDir)
	if err != nil {
		return nil, nil, err
	}

	var findings []Finding
	var bumped []BumpResult
	for _, scan := range scans {
		for _, scanErr := range scan.Errors {
			findings = append(findings, Finding{
				RecipeID: scan.Recipe.ID, Kind: "error", Role: scanErr.Role, Details: scanErr.Message,
			})
		}
		if scan.Source != nil && scan.Source.Drifted {
			findings = append(findings, Finding{
				RecipeID: scan.Recipe.ID, Kind: "source",
				Pinned: scan.Source.PinnedRevision, Current: scan.Source.CurrentRevision,
				Reason:  "needs judgment",
				Details: "the source repository's default branch moved; review and bump by hand",
			})
		}

		var edits []artifactEdit
		var editedRoles []string
		var bumpedArtifacts []ArtifactBumpDetail
		for _, drift := range scan.Artifacts {
			if !drift.Drifted {
				continue
			}
			index, ok := scan.Recipe.ArtifactIndex(drift.Role)
			if !ok {
				continue // cannot happen: the scan only ever names roles the recipe itself declares
			}
			edit, reason, eligible := evaluateBump(scan.Recipe.Artifacts[index], drift, hf)
			if !eligible {
				findings = append(findings, Finding{
					RecipeID: scan.Recipe.ID, Kind: "artifact", Role: drift.Role,
					Pinned: drift.PinnedRevision, Current: drift.CurrentRevision,
					Reason: "needs judgment", Details: reason,
				})
				continue
			}
			edits = append(edits, edit)
			editedRoles = append(editedRoles, drift.Role)
			bumpedArtifacts = append(bumpedArtifacts, ArtifactBumpDetail{
				Role: drift.Role, OldRevision: drift.PinnedRevision, NewRevision: drift.CurrentRevision,
			})
		}
		if len(edits) == 0 {
			continue
		}

		path, ok := fileIndex[scan.Recipe.ID]
		if !ok {
			findings = append(findings, Finding{
				RecipeID: scan.Recipe.ID, Kind: "error",
				Details: fmt.Sprintf("no file under %s decodes to this recipe's id; cannot bump", recipesDir),
			})
			continue
		}
		original, err := os.ReadFile(path)
		if err != nil {
			findings = append(findings, Finding{RecipeID: scan.Recipe.ID, Kind: "error", Details: fmt.Sprintf("read %s: %v", path, err)})
			continue
		}

		content := string(original)
		editFailed := false
		for _, edit := range edits {
			content, err = applyArtifactEdit(content, edit)
			if err != nil {
				editFailed = true
				break
			}
		}
		newVersion := scan.Recipe.Version + 1
		if !editFailed {
			content, err = replaceIndentedLine(content, "version", fmt.Sprintf("%d", scan.Recipe.Version), fmt.Sprintf("%d", newVersion), 0)
			if err != nil {
				editFailed = true
			}
		}
		if editFailed {
			findings = append(findings, Finding{
				RecipeID: scan.Recipe.ID, Kind: "artifact", Role: strings.Join(editedRoles, ","),
				Reason:  "needs judgment",
				Details: "automatic bump could not edit the recipe file: " + err.Error(),
			})
			continue
		}

		decoded, decodeErr := recipe.DecodeStrict([]byte(content))
		if decodeErr == nil {
			decodeErr = recipe.Validate(decoded)
		}
		if decodeErr != nil {
			// The edit was never written; content only ever lived in memory,
			// so the file on disk is already exactly what it was.
			findings = append(findings, Finding{
				RecipeID: scan.Recipe.ID, Kind: "artifact", Role: strings.Join(editedRoles, ","),
				Reason:  "needs judgment",
				Details: "automatic bump produced an invalid recipe and was discarded: " + decodeErr.Error(),
			})
			continue
		}

		perm := os.FileMode(0o644)
		if info, statErr := os.Stat(path); statErr == nil {
			perm = info.Mode().Perm()
		}
		if err := os.WriteFile(path, []byte(content), perm); err != nil {
			findings = append(findings, Finding{RecipeID: scan.Recipe.ID, Kind: "error", Details: fmt.Sprintf("write %s: %v", path, err)})
			continue
		}
		bumped = append(bumped, BumpResult{
			RecipeID:   scan.Recipe.ID,
			File:       path,
			OldVersion: scan.Recipe.Version,
			NewVersion: newVersion,
			Artifacts:  bumpedArtifacts,
		})
	}
	return findings, bumped, nil
}

// indexRecipeFiles maps every recipe id under dir to its file path, by
// decoding each *.yaml file the same way recipe.Builtin resolves the
// embedded set. Matching by decoded id rather than by filename means a bump
// always edits the exact file the embedded copy came from, never a filename
// guess.
func indexRecipeFiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read recipes directory %s: %w", dir, err)
	}
	index := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		decoded, err := recipe.DecodeStrict(data)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		index[decoded.ID] = path
	}
	return index, nil
}
