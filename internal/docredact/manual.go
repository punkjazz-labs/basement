package docredact

import (
	"errors"
	"strings"
)

// The three ways adding a literal by hand can fail, each one a fact the
// console can put in front of the owner as it is.
var (
	// ErrEmptyLiteral: nothing was selected, or only whitespace was.
	ErrEmptyLiteral = errors.New("literal is required")
	// ErrLiteralNotFound: the text does not appear in the document, so
	// hiding it would replace nothing.
	ErrLiteralNotFound = errors.New("that text is not in the document")
	// ErrLiteralKnown: an identical literal is already a finding, whichever
	// pass found it, so it already has a pseudonym and a toggle.
	ErrLiteralKnown = errors.New("that text is already a finding")
	// ErrLiteralCovered: every occurrence sits inside a longer literal that
	// is already being replaced, so a second finding would never fire.
	ErrLiteralCovered = errors.New("that text is already inside a longer finding")
)

// manualMatches finds every non-overlapping occurrence of an exact literal,
// left to right. Matching is exact and case sensitive: the owner selected
// these characters, and replacing something they did not select would be a
// different promise than the one the button makes.
func manualMatches(text, literal string, category Category) []Match {
	if literal == "" {
		return nil
	}
	var out []Match
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], literal)
		if index < 0 {
			break
		}
		start := offset + index
		end := start + len(literal)
		out = append(out, Match{Start: start, End: end, Text: literal, Category: category, Source: SourceManual})
		offset = end
	}
	return out
}

// AddManual hides one exact literal the owner picked out of the preview. The
// literal joins the same overlap resolution as every detector match, so a
// manual phrase that contains a detected literal wins over it (longer wins)
// and one that sits inside a longer detected literal is refused rather than
// silently doing nothing.
//
// Surrounding whitespace is trimmed, because a browser selection routinely
// carries a trailing space that is not part of what the owner meant. Nothing
// else about the text is changed.
func (d *Document) AddManual(literal string, category Category) (*Finding, error) {
	literal = strings.TrimSpace(literal)
	if literal == "" {
		return nil, ErrEmptyLiteral
	}
	if _, exists := d.byLiteral[literal]; exists {
		return nil, ErrLiteralKnown
	}
	for _, existing := range d.manual {
		if existing.literal == literal {
			return nil, ErrLiteralKnown
		}
	}
	if !strings.Contains(d.Text, literal) {
		return nil, ErrLiteralNotFound
	}
	if category == "" {
		category = CategoryPhrase
	}

	d.manual = append(d.manual, manualLiteral{literal: literal, category: category})
	d.recompute()
	finding, ok := d.byLiteral[literal]
	if !ok {
		// Every occurrence lost the overlap contest. Put the document back
		// the way it was rather than leave a literal on the list that
		// produces no finding.
		d.manual = d.manual[:len(d.manual)-1]
		d.recompute()
		return nil, ErrLiteralCovered
	}
	return finding, nil
}
