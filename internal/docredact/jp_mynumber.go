package docredact

import (
	"regexp"
	"strings"
)

// jpSpaceGroupRun and jpHyphenGroupRun each match a maximal run of 4-digit
// groups joined by one single, consistent separator -- either all spaces
// or all hyphens, never mixed within one run. A run stops the instant the
// separator changes or repeats (a double space, or a hyphen appearing
// where the run has so far used spaces), because the trailing `*` can only
// keep matching its own literal separator.
var jpSpaceGroupRun = regexp.MustCompile(`\b\d{4}(?: \d{4})*\b`)
var jpHyphenGroupRun = regexp.MustCompile(`\b\d{4}(?:-\d{4})*\b`)

// JPMyNumberDetector finds Japanese My Numbers written only in their
// grouped form, NNNN NNNN NNNN with a consistent single space or hyphen
// separator. A bare 12-digit run is deliberately not matched: without
// separators it is indistinguishable from any other 12-digit number in a
// document, the same reasoning as the US SSN's hyphenated-only decision --
// see ssnPattern's comment in us_ssn.go.
//
// A My Number directly adjacent to another grouped run of 4-digit groups
// is genuinely ambiguous, and this package refuses to guess at a boundary
// it cannot anchor with a checksum: see jpCandidatesFromRuns for the exact
// rule. Two consequences worth stating plainly:
//   - Two My Numbers written back-to-back with nothing between them (six
//     groups total) are both found, each validated on its own -- the run
//     splits cleanly into two aligned triples.
//   - A real My Number with one unrelated 4-digit group glued onto it (five
//     groups total) is not found at all. There is no checksum-anchored way
//     to tell which three of those five groups are the real number without
//     guessing, so the whole run is left alone rather than risk either a
//     false positive or a false negative on the wrong half.
//
// A 4x4-formatted 16-digit number (a card number, for instance) never
// splits into a whole number of triples and so is never mistaken for a My
// Number, whatever its own digits happen to be.
type JPMyNumberDetector struct{}

func (JPMyNumberDetector) Name() string { return "jp_my_number" }

func (JPMyNumberDetector) Detect(text string) []Match {
	var out []Match
	out = append(out, jpCandidatesFromRuns(text, jpSpaceGroupRun, " ")...)
	out = append(out, jpCandidatesFromRuns(text, jpHyphenGroupRun, "-")...)
	return out
}

// jpCandidatesFromRuns finds every maximal run matched by runPattern (a
// run of 4-digit groups joined by sep) and, for a run whose group count is
// divisible by 3, emits one Match per aligned 12-digit triple of groups
// (groups 0-2, 3-5, 6-8, ...) whose check digit (jpCheckDigit) validates.
// A run whose group count is not a multiple of 3 -- including runs shorter
// than three groups -- yields no candidates at all: there is no
// principled way to pick out a real My Number from an ambiguous run, so
// none of it is matched.
func jpCandidatesFromRuns(text string, runPattern *regexp.Regexp, sep string) []Match {
	var out []Match
	for _, loc := range runPattern.FindAllStringIndex(text, -1) {
		start := loc[0]
		groups := strings.Split(text[start:loc[1]], sep)
		if len(groups) < 3 || len(groups)%3 != 0 {
			continue
		}
		pos := start
		for i := 0; i < len(groups); i += 3 {
			triple := groups[i] + sep + groups[i+1] + sep + groups[i+2]
			tripleEnd := pos + len(triple)
			first11 := groups[i] + groups[i+1] + groups[i+2][:3]
			checkDigit := int(groups[i+2][3] - '0')
			if jpCheckDigit(first11) == checkDigit {
				out = append(out, Match{
					Start:    pos,
					End:      tripleEnd,
					Text:     text[pos:tripleEnd],
					Category: CategoryJPMyNumber,
					Source:   Source,
				})
			}
			pos = tripleEnd
			if i+3 < len(groups) {
				pos += len(sep) // skip the separator joining this triple to the next
			}
		}
	}
	return out
}

// jpCheckDigit applies the Digital Agency's published My Number
// check-digit formula to the first 11 payload digits: reading those 11
// digits right to left as positions n = 1..11, the weight is q = n+1 for
// n <= 6 and q = n-5 for n >= 7; the remainder r is the sum of digit*weight
// mod 11; the check digit is 0 when r <= 1, otherwise 11-r.
func jpCheckDigit(first11 string) int {
	sum := 0
	for i := 0; i < 11; i++ {
		n := 11 - i // position counted from the rightmost payload digit
		var q int
		if n <= 6 {
			q = n + 1
		} else {
			q = n - 5
		}
		d := int(first11[i] - '0')
		sum += d * q
	}
	r := sum % 11
	if r <= 1 {
		return 0
	}
	return 11 - r
}
