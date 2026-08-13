package docredact

import "regexp"

// bsnPattern matches a plain 9-digit Dutch BSN. A leading zero is a valid
// BSN (unlike, say, the German Steuer-ID's first digit), so the pattern
// places no constraint on the first digit. validBSN is the real
// validator.
var bsnPattern = regexp.MustCompile(`\b\d{9}\b`)

type NLBSNDetector struct{}

func (NLBSNDetector) Name() string { return "nl_bsn" }

func (NLBSNDetector) Detect(text string) []Match {
	return matchesFromRegexp(text, bsnPattern, CategoryNLBSN, validBSN)
}

// validBSN applies the Dutch elfproef (eleven test) with its BSN-specific
// negative last weight: 9*d1 + 8*d2 + ... + 2*d8 - 1*d9 must be a
// multiple of 11. A run of nine digits that is all zeros trivially
// satisfies that arithmetic (the sum is 0) but is not a real BSN, so it
// is rejected separately.
func validBSN(s string) bool {
	allZero := true
	sum := 0
	weight := 9
	for i := 0; i < 8; i++ {
		d := int(s[i] - '0')
		if d != 0 {
			allZero = false
		}
		sum += weight * d
		weight--
	}
	last := int(s[8] - '0')
	if last != 0 {
		allZero = false
	}
	sum -= last
	if allZero {
		return false
	}
	return sum%11 == 0
}
