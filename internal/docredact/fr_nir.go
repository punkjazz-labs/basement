package docredact

import (
	"regexp"
	"strconv"
)

// nirPattern matches a French NIR (INSEE social-security number) in
// either its 15-character contiguous form or the conventional
// space-grouped form "S YY MM DD CCC OOO KK" (1-2-2-2-3-3-2 characters),
// with an optional single space between each group so both written forms
// share one pattern. The department group (DD) accepts the two Corsican
// codes 2A/2B in place of two digits, uppercase only: identity documents
// print them that way, and matching lowercase too would just widen the
// false-positive net for no real document ever written that way.
// nirCheckKey is the real validator.
var nirPattern = regexp.MustCompile(`\b(\d)[ ]?(\d{2})[ ]?(\d{2})[ ]?(\d{2}|2[AB])[ ]?(\d{3})[ ]?(\d{3})[ ]?(\d{2})\b`)

type FRNIRDetector struct{}

func (FRNIRDetector) Name() string { return "fr_nir" }

func (FRNIRDetector) Detect(text string) []Match {
	var out []Match
	for _, loc := range nirPattern.FindAllStringSubmatchIndex(text, -1) {
		sex := text[loc[2]:loc[3]]
		year := text[loc[4]:loc[5]]
		month := text[loc[6]:loc[7]]
		dept := text[loc[8]:loc[9]]
		commune := text[loc[10]:loc[11]]
		order := text[loc[12]:loc[13]]
		key := text[loc[14]:loc[15]]

		// The sex digit is 1 (male) or 2 (female) for a person born in
		// France; 7/8 are the provisional codes issued to a foreign-born
		// person pending their permanent number. No other value is ever
		// issued.
		if c := sex[0]; c != '1' && c != '2' && c != '7' && c != '8' {
			continue
		}
		monthNum, err := strconv.Atoi(month)
		if err != nil || monthNum < 1 || monthNum > 12 {
			continue
		}
		deptDigits, ok := nirDeptDigits(dept)
		if !ok {
			continue
		}
		body := sex + year + month + deptDigits + commune + order
		want, ok := nirCheckKey(body)
		if !ok {
			continue
		}
		keyNum, err := strconv.Atoi(key)
		if err != nil || keyNum != want {
			continue
		}
		out = append(out, Match{
			Start:    loc[0],
			End:      loc[1],
			Text:     text[loc[0]:loc[1]],
			Category: CategoryFRNIR,
			Source:   Source,
		})
	}
	return out
}

// nirDeptDigits turns a written department code into the two digits the
// check-key formula uses. 2A and 2B (the two Corsican departments, which
// have no numeric INSEE code of their own) substitute to 19 and 18
// respectively -- a fixed rule, not derived from anything. Any other
// value must fall in an INSEE department range: 01-95 (metropolitan
// France) or 97-98 (overseas).
func nirDeptDigits(dept string) (string, bool) {
	switch dept {
	case "2A":
		return "19", true
	case "2B":
		return "18", true
	}
	n, err := strconv.Atoi(dept)
	if err != nil {
		return "", false
	}
	if n >= 1 && n <= 95 {
		return dept, true
	}
	if n >= 97 && n <= 98 {
		return dept, true
	}
	return "", false
}

// nirCheckKey computes the expected 2-digit key from the first 13
// characters of a NIR (sex+year+month+department+commune+order, with any
// Corsican department letters already substituted to digits): the
// published INSEE formula is 97 minus that 13-digit number modulo 97.
func nirCheckKey(body string) (int, bool) {
	n, err := strconv.ParseInt(body, 10, 64)
	if err != nil {
		return 0, false
	}
	return int(97 - n%97), true
}
