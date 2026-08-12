package docredact

import (
	"regexp"
	"strconv"
)

// ssnPattern matches only the conventional hyphenated form, AAA-GG-SSSS.
// A bare 9-digit run is deliberately not matched: without separators it is
// indistinguishable from any other 9-digit number in a document (an order
// ID, a phone number missing its formatting), and the false-positive rate
// of treating every such number as a candidate SSN was judged worse than
// missing unformatted ones. See docs/decisions/0021.
var ssnPattern = regexp.MustCompile(`\b(\d{3})-(\d{2})-(\d{4})\b`)

type USSSNDetector struct{}

func (USSSNDetector) Name() string { return "us_ssn" }

func (USSSNDetector) Detect(text string) []Match {
	var out []Match
	for _, loc := range ssnPattern.FindAllStringSubmatchIndex(text, -1) {
		area, _ := strconv.Atoi(text[loc[2]:loc[3]])
		group, _ := strconv.Atoi(text[loc[4]:loc[5]])
		serial, _ := strconv.Atoi(text[loc[6]:loc[7]])
		if !validSSN(area, group, serial) {
			continue
		}
		out = append(out, Match{
			Start:    loc[0],
			End:      loc[1],
			Text:     text[loc[0]:loc[1]],
			Category: CategoryUSSSN,
			Source:   Source,
		})
	}
	return out
}

// validSSN applies the SSA's known-invalid-range rules: area 000 and 666
// are never issued, nor is any area from 900-999 (reserved); group 00 and
// serial 0000 are never issued regardless of area/group. These structural
// rules still hold even though the SSA stopped assigning SSNs by
// geographic area in 2011.
func validSSN(area, group, serial int) bool {
	if area == 0 || area == 666 || area >= 900 {
		return false
	}
	if group == 0 {
		return false
	}
	if serial == 0 {
		return false
	}
	return true
}
