package main

import (
	"fmt"
	"regexp"
	"strings"
)

// This file holds the surgical raw-text YAML editing feed-watch's bump mode
// uses instead of a decode-edit-re-marshal round trip. A re-marshal would
// reformat the whole file and lose every comment; a recipe's comments record
// why a pin was chosen, and losing them on every bump would be worse than
// not bumping at all. Every function here therefore edits one exact,
// full-line match and fails loudly rather than guess when that line is not
// found; a caller that gets an error must leave the file untouched.

// anyArtifactRoleLine matches the start of any artifact list entry
// ("  - role: <name>"), which is how a recipe's artifacts: list is written
// throughout this pack (two-space indent, one entry per role).
var anyArtifactRoleLine = regexp.MustCompile(`(?m)^  - role: `)

// topLevelKeyLine matches the start of a top-level (zero-indent) mapping
// key. Every recipe field outside artifacts: sits at this indent, so it
// marks where the artifacts: list, and any one entry inside it, ends.
var topLevelKeyLine = regexp.MustCompile(`(?m)^[A-Za-z]`)

// artifactBlock returns the byte range of one artifact's list entry inside
// content, from its "  - role: <role>" line up to (but not including)
// whichever comes first: the next artifact entry, or the next top-level key.
func artifactBlock(content, role string) (start, end int, err error) {
	roleLine := regexp.MustCompile(`(?m)^  - role: ` + regexp.QuoteMeta(role) + `\s*$`)
	loc := roleLine.FindStringIndex(content)
	if loc == nil {
		return 0, 0, fmt.Errorf("could not find an artifact block for role %q", role)
	}
	start = loc[0]
	rest := content[loc[1]:]
	end = len(content)
	if m := anyArtifactRoleLine.FindStringIndex(rest); m != nil {
		end = loc[1] + m[0]
	}
	if m := topLevelKeyLine.FindStringIndex(rest); m != nil && loc[1]+m[0] < end {
		end = loc[1] + m[0]
	}
	return start, end, nil
}

// replaceIndentedLine rewrites the single line "<indent spaces><key>:
// <oldValue>" to carry newValue instead, and fails if that exact line does
// not appear in content exactly once. Content outside the matched line is
// returned byte for byte.
func replaceIndentedLine(content, key, oldValue, newValue string, indent int) (string, error) {
	prefix := strings.Repeat(" ", indent)
	oldLine := prefix + key + ": " + oldValue
	newLine := prefix + key + ": " + newValue
	lines := strings.SplitAfter(content, "\n")
	matches := 0
	for i, line := range lines {
		hasNewline := strings.HasSuffix(line, "\n")
		body := strings.TrimSuffix(line, "\n")
		if body != oldLine {
			continue
		}
		matches++
		if hasNewline {
			lines[i] = newLine + "\n"
		} else {
			lines[i] = newLine
		}
	}
	if matches != 1 {
		return "", fmt.Errorf("expected exactly one line %q, found %d", oldLine, matches)
	}
	return strings.Join(lines, ""), nil
}

// artifactEdit is one artifact's safe-subset bump, already decided by
// evaluateBump: which lines change and what they become. UpdateExpectedBytes
// is set only for a whole-snapshot artifact, whose total moves with the
// revision; a per-file-pinned artifact's expected_bytes is already correct
// because every pinned file's own size is unchanged.
type artifactEdit struct {
	Role                string
	OldRevision         string
	NewRevision         string
	UpdateExpectedBytes bool
	OldExpectedBytes    int64
	NewExpectedBytes    int64
	OldLicenceURL       string
}

// applyArtifactEdit rewrites one artifact's revision, and where the edit
// calls for it, its expected_bytes and the revision segment of its
// licence_url, entirely within that artifact's own block so a same-named
// field on a different artifact is never touched.
func applyArtifactEdit(content string, edit artifactEdit) (string, error) {
	start, end, err := artifactBlock(content, edit.Role)
	if err != nil {
		return "", err
	}
	block := content[start:end]

	block, err = replaceIndentedLine(block, "revision", edit.OldRevision, edit.NewRevision, 4)
	if err != nil {
		return "", fmt.Errorf("artifact %s: %w", edit.Role, err)
	}
	if edit.UpdateExpectedBytes {
		block, err = replaceIndentedLine(block, "expected_bytes", formatInt(edit.OldExpectedBytes), formatInt(edit.NewExpectedBytes), 4)
		if err != nil {
			return "", fmt.Errorf("artifact %s: %w", edit.Role, err)
		}
	}
	if edit.OldLicenceURL != "" && strings.Contains(edit.OldLicenceURL, edit.OldRevision) {
		newLicenceURL := strings.ReplaceAll(edit.OldLicenceURL, edit.OldRevision, edit.NewRevision)
		block, err = replaceIndentedLine(block, "licence_url", edit.OldLicenceURL, newLicenceURL, 4)
		if err != nil {
			return "", fmt.Errorf("artifact %s: %w", edit.Role, err)
		}
	}
	return content[:start] + block + content[end:], nil
}

func formatInt(n int64) string {
	return fmt.Sprintf("%d", n)
}
