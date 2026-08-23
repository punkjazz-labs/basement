package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Report is the complete JSON document feed-watch writes to -out. Findings
// is check mode's whole output, and bump mode's leftover output: whatever
// bump mode did not or could not fix automatically. Bumped is empty in
// check mode, since check mode never writes a file.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Mode        string    `json:"mode"`
	Findings    []Finding `json:"findings"`
	// Acknowledged holds findings a maintainer ruling in the
	// acknowledgements file covers: still true upstream, already judged,
	// not open work. They never count toward the exit code.
	Acknowledged []Finding    `json:"acknowledged,omitempty"`
	Bumped       []BumpResult `json:"bumped,omitempty"`
}

// Finding is one thing feed-watch found and did not resolve on its own: a
// drifted artifact, a moved source, or a network failure. Kind is one of
// "artifact", "source", or "error". Reason is set only in bump mode, and
// only for a drift bump mode declined to apply; it is always the literal
// string "needs judgment", so a maintainer session can grep for it. Details
// carries the specifics either way.
type Finding struct {
	RecipeID string `json:"recipe_id"`
	Kind     string `json:"kind"`
	Role     string `json:"role,omitempty"`
	Pinned   string `json:"pinned,omitempty"`
	Current  string `json:"current,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Details  string `json:"details,omitempty"`
}

// BumpResult is one recipe file bump mode rewrote in place.
type BumpResult struct {
	RecipeID   string               `json:"recipe_id"`
	File       string               `json:"file"`
	OldVersion int                  `json:"old_version"`
	NewVersion int                  `json:"new_version"`
	Artifacts  []ArtifactBumpDetail `json:"artifacts"`
}

// ArtifactBumpDetail is one artifact's revision move inside a BumpResult.
type ArtifactBumpDetail struct {
	Role        string `json:"role"`
	OldRevision string `json:"old_revision"`
	NewRevision string `json:"new_revision"`
}

// buildCheckFindings turns a set of recipe scans into check mode's report:
// every drifted artifact, every drifted source, and every scan error, with
// no distinction between "would have qualified for auto-bump" and "would
// not have"; check mode only ever reports, so that distinction belongs to
// bump mode alone.
func buildCheckFindings(scans []recipeScan) []Finding {
	var findings []Finding
	for _, scan := range scans {
		for _, drift := range scan.Artifacts {
			if !drift.Drifted {
				continue
			}
			findings = append(findings, Finding{
				RecipeID: scan.Recipe.ID, Kind: "artifact", Role: drift.Role,
				Pinned: drift.PinnedRevision, Current: drift.CurrentRevision,
				Details: describeArtifactDrift(drift),
			})
		}
		if scan.Source != nil && scan.Source.Drifted {
			findings = append(findings, Finding{
				RecipeID: scan.Recipe.ID, Kind: "source",
				Pinned: scan.Source.PinnedRevision, Current: scan.Source.CurrentRevision,
				Details: "the source repository's default branch moved",
			})
		}
		for _, scanErr := range scan.Errors {
			findings = append(findings, Finding{
				RecipeID: scan.Recipe.ID, Kind: "error", Role: scanErr.Role, Details: scanErr.Message,
			})
		}
	}
	return findings
}

func describeArtifactDrift(drift artifactDrift) string {
	var b strings.Builder
	fmt.Fprintf(&b, "snapshot bytes %d -> %d", drift.PinnedTotalBytes, drift.NewTotalBytes)
	if drift.WholeSnapshot {
		if drift.NewHasLicense {
			b.WriteString("; the new revision carries a LICENSE file")
		} else {
			b.WriteString("; the new revision carries no LICENSE or LICENSE.md file")
		}
	}
	for _, file := range drift.Files {
		switch {
		case !file.StillExists:
			fmt.Fprintf(&b, "; file %s is missing at the new revision", file.Name)
		case !file.SameSize:
			fmt.Fprintf(&b, "; file %s changed size %d -> %d bytes", file.Name, file.PinnedBytes, file.NewBytes)
		default:
			fmt.Fprintf(&b, "; file %s unchanged (%d bytes)", file.Name, file.NewBytes)
		}
	}
	return b.String()
}

// exitCode turns a finding set into the exit code a cron job branches on: 0
// when nothing needs attention, 3 when at least one real drift (artifact or
// source) does, and 4 when every finding is a network error and none of them
// is a confirmed drift; that is the one case where "nothing to report"
// might just mean "could not reach anything".
func exitCode(findings []Finding) int {
	hasDrift := false
	hasError := false
	for _, f := range findings {
		if f.Kind == "error" {
			hasError = true
		} else {
			hasDrift = true
		}
	}
	switch {
	case hasDrift:
		return 3
	case hasError:
		return 4
	default:
		return 0
	}
}

// printSummary writes the one-line-per-drift human summary, and in bump
// mode, the list of files bump mode changed. Acknowledged findings print
// one quiet line each, so the daily log still shows the ruling exists.
func printSummary(w io.Writer, mode string, findings, acknowledged []Finding, bumped []BumpResult) {
	for _, f := range acknowledged {
		fmt.Fprintf(w, "%s: %s drift at %s acknowledged\n", f.RecipeID, roleOrKind(f), f.Current)
	}
	for _, f := range findings {
		switch f.Kind {
		case "error":
			fmt.Fprintf(w, "%s: %s check failed: %s\n", f.RecipeID, roleOrKind(f), f.Details)
		default:
			label := f.Kind
			if f.Reason != "" {
				label = f.Reason
			}
			fmt.Fprintf(w, "%s: %s %s drifted %s -> %s (%s): %s\n", f.RecipeID, roleOrKind(f), f.Kind, f.Pinned, f.Current, label, f.Details)
		}
	}
	if mode == "bump" {
		if len(bumped) == 0 {
			fmt.Fprintln(w, "no files changed")
		} else {
			fmt.Fprintln(w, "changed files:")
			for _, b := range bumped {
				fmt.Fprintf(w, "  %s (version %d -> %d)\n", b.File, b.OldVersion, b.NewVersion)
			}
		}
	} else if len(findings) == 0 && len(acknowledged) == 0 {
		fmt.Fprintln(w, "no drift")
	}
}

func roleOrKind(f Finding) string {
	if f.Role != "" {
		return f.Role
	}
	return f.Kind
}

// writeReport marshals report as indented JSON and writes it to path.
func writeReport(path string, report Report) error {
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o640); err != nil {
		return fmt.Errorf("write report %s: %w", path, err)
	}
	return nil
}
