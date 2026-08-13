package docredact

import (
	"regexp"
	"strings"
)

// ninoPattern matches a UK National Insurance Number: two letters, six
// digits, and a suffix letter restricted to A-D, either contiguous or in
// the conventional space-grouped written form "AA 12 34 56 C" (same
// optional-single-space convention as nirPattern in fr_nir.go). Uppercase
// only, for the same reason dniPattern in es_dni.go is uppercase only:
// identity documents print it that way, and matching lowercase too would
// just widen the false-positive net. ninoStructureOK applies the letter
// rules the pattern itself cannot express.
//
// Unlike every other national-identifier detector in this package, a
// NINO has no check digit at all -- HMRC never defined one. This
// detector is therefore structure-only: the pattern's letter classes plus
// ninoStructureOK's forbidden-letter and forbidden-pair rules are the
// only validation the format supports. That is a real limitation (a
// random-looking but structurally valid string will match even if it was
// never actually issued), the same honesty as the US SSN's decision not
// to match unhyphenated digit runs -- see ssnPattern's comment in
// us_ssn.go.
var ninoPattern = regexp.MustCompile(`\b([A-Z])([A-Z])[ ]?(\d{2})[ ]?(\d{2})[ ]?(\d{2})[ ]?([A-D])\b`)

type UKNINODetector struct{}

func (UKNINODetector) Name() string { return "uk_nino" }

func (UKNINODetector) Detect(text string) []Match {
	var out []Match
	for _, loc := range ninoPattern.FindAllStringSubmatchIndex(text, -1) {
		first := text[loc[2]:loc[3]]
		second := text[loc[4]:loc[5]]
		if !ninoStructureOK(first[0], second[0]) {
			continue
		}
		out = append(out, Match{
			Start:    loc[0],
			End:      loc[1],
			Text:     text[loc[0]:loc[1]],
			Category: CategoryUKNINO,
			Source:   Source,
		})
	}
	return out
}

// ninoForbiddenFirst and ninoForbiddenSecond are HMRC's published
// letters that are never issued in the first or second prefix position
// respectively (the sets differ: O is excluded only from the second
// position). ninoForbiddenPairs is HMRC's published list (NIM39110) of
// two-letter prefixes excluded from allocation regardless of the
// individual letters. No checksum exists for a NINO, so this prefix
// legality is the only validation available.
const ninoForbiddenFirst = "DFIQUV"
const ninoForbiddenSecond = "DFIOQUV"

var ninoForbiddenPairs = map[string]bool{
	"BG": true,
	"GB": true,
	"NK": true,
	"KN": true,
	"TN": true,
	"NT": true,
	"ZZ": true,
}

// ninoStructureOK applies the three published prefix rules: the first
// letter must not be in ninoForbiddenFirst, the second must not be in
// ninoForbiddenSecond, and the pair together must not be in
// ninoForbiddenPairs.
func ninoStructureOK(first, second byte) bool {
	if strings.IndexByte(ninoForbiddenFirst, first) >= 0 {
		return false
	}
	if strings.IndexByte(ninoForbiddenSecond, second) >= 0 {
		return false
	}
	pair := string(first) + string(second)
	return !ninoForbiddenPairs[pair]
}
