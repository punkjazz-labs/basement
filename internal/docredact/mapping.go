package docredact

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MappingWarning is written as the literal first line of MappingBytes,
// ahead of the JSON payload -- not folded into a JSON field -- so that a
// human, or a program, cannot open the file, glance at it, and miss the
// warning inside a wrapper. It is also why the mapping is not valid JSON on
// its own: parsing it back (ParseMapping) skips this line deliberately.
// See docs/decisions/0021.
const MappingWarning = "WARNING: this file maps redaction pseudonyms back to the original sensitive text. Do not upload it anywhere."

// MappingEntry is one row of the pseudonym-to-literal table. Only enabled
// findings appear here: a disabled finding was never replaced in the
// redacted copy, so it has no pseudonym to map from, and recording its
// literal in this file would put sensitive text into a file whose only job
// is to enumerate what already left the machine in disguised form.
type MappingEntry struct {
	Token       string `json:"token"`
	Literal     string `json:"literal"`
	Category    string `json:"category"`
	Source      string `json:"source"`
	Occurrences int    `json:"occurrences"`
}

// Mapping returns the pseudonym-to-literal table for every enabled
// finding, in the same first-appearance order as Findings.
func (d *Document) Mapping() []MappingEntry {
	var out []MappingEntry
	for _, f := range d.Findings {
		if !f.Enabled {
			continue
		}
		out = append(out, MappingEntry{
			Token:       f.Token,
			Literal:     f.Literal,
			Category:    string(f.Category),
			Source:      f.Source,
			Occurrences: f.Occurrences,
		})
	}
	return out
}

type mappingPayload struct {
	Entries []MappingEntry `json:"entries"`
}

// MappingBytes renders the mapping as the bytes a download response body
// should be: MappingWarning, a newline, then the JSON payload. Nothing
// here touches disk -- the caller (internal/httpapi) writes these bytes
// straight into an HTTP response as a browser download, per the "runs from
// basement directly" design: the manager never writes a redacted copy or a
// mapping file to its own filesystem.
func (d *Document) MappingBytes() ([]byte, error) {
	payload, err := json.MarshalIndent(mappingPayload{Entries: d.Mapping()}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode mapping: %w", err)
	}
	var buf bytes.Buffer
	buf.WriteString(MappingWarning)
	buf.WriteByte('\n')
	buf.Write(payload)
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// ParseMapping parses bytes produced by MappingBytes back into the warning
// line and entries. It exists for the round-trip test and for any future
// "restore the reply" feature (spec's open question, not built here) that
// needs to read a downloaded mapping file back in.
func ParseMapping(data []byte) (warning string, entries []MappingEntry, err error) {
	idx := bytes.IndexByte(data, '\n')
	if idx < 0 {
		return "", nil, fmt.Errorf("mapping has no warning line")
	}
	warning = string(data[:idx])
	var payload mappingPayload
	if err := json.Unmarshal(data[idx+1:], &payload); err != nil {
		return "", nil, fmt.Errorf("parse mapping json: %w", err)
	}
	return warning, payload.Entries, nil
}
