package docredact

import (
	"fmt"
	"strings"
)

// Finding is one group: one exact literal, seen possibly many times across
// the document, with a single toggle that governs every occurrence.
type Finding struct {
	// ID is stable for the lifetime of one analyzed Document and doubles
	// as the text inside the replacement token, e.g. "EMAIL_1" for the
	// token "[EMAIL_1]". Numbered per category in order of first
	// appearance in the document.
	ID          string   `json:"id"`
	Token       string   `json:"token"`
	Literal     string   `json:"literal"`
	Category    Category `json:"category"`
	Source      string   `json:"source"`
	Occurrences int      `json:"occurrences"`
	Enabled     bool     `json:"enabled"`
}

// Document is one analyzed text: the original bytes, the findings grouped
// from it, and the resolved (non-overlapping) spans needed to produce a
// preview or an export. It is not safe for concurrent use without an
// external lock -- internal/httpapi, which serves it over HTTP, owns that.
type Document struct {
	Text     string
	Findings []*Finding // first-appearance order; also the pseudonym numbering order

	spans     []Match // resolved, sorted by Start, non-overlapping
	byLiteral map[string]*Finding
}

// Analyze runs every registered pattern detector over text, resolves
// overlapping spans (longer literal wins), and groups the survivors by
// exact literal text into findings, enabled by default.
//
// Grouping keys on literal text alone, not (category, literal): the
// detector set here never produces two different categories for the same
// exact literal in practice (an email is never also a valid IBAN), so this
// is simpler than a compound key without losing anything. If a literal is
// somehow claimed by two categories, whichever occurrence comes first in
// document order decides the finding's category -- deterministic, if not
// necessarily the "right" one for that edge case.
func Analyze(text string) *Document {
	matches := DetectAll(text)
	resolved := ResolveOverlaps(matches)

	doc := &Document{
		Text:      text,
		spans:     resolved,
		byLiteral: make(map[string]*Finding),
	}

	counters := make(map[Category]int)
	for _, m := range resolved {
		f, ok := doc.byLiteral[m.Text]
		if !ok {
			counters[m.Category]++
			n := counters[m.Category]
			f = &Finding{
				ID:       fmt.Sprintf("%s_%d", m.Category.Prefix(), n),
				Token:    fmt.Sprintf("[%s_%d]", m.Category.Prefix(), n),
				Literal:  m.Text,
				Category: m.Category,
				Source:   m.Source,
				Enabled:  true,
			}
			doc.byLiteral[m.Text] = f
			doc.Findings = append(doc.Findings, f)
		}
		f.Occurrences++
	}

	return doc
}

// Toggle sets a finding's enabled state by ID. It reports whether a
// finding with that ID exists.
func (d *Document) Toggle(id string, enabled bool) bool {
	for _, f := range d.Findings {
		if f.ID == id {
			f.Enabled = enabled
			return true
		}
	}
	return false
}

// Redacted renders the document with every enabled finding's occurrences
// replaced by its pseudonym token. Disabled findings are left exactly as
// written -- that is what "disabled" means, not a bug to fix later.
func (d *Document) Redacted() string {
	var b strings.Builder
	last := 0
	for _, m := range d.spans {
		f := d.byLiteral[m.Text]
		if f == nil || !f.Enabled {
			continue
		}
		b.WriteString(d.Text[last:m.Start])
		b.WriteString(f.Token)
		last = m.End
	}
	b.WriteString(d.Text[last:])
	return b.String()
}
