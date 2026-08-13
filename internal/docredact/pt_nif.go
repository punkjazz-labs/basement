package docredact

import "regexp"

// nifPattern matches a plain 9-digit Portuguese NIF. validNIF is the real
// validator: the leading-digit rule and the check digit.
var nifPattern = regexp.MustCompile(`\b\d{9}\b`)

type PTNIFDetector struct{}

func (PTNIFDetector) Name() string { return "pt_nif" }

func (PTNIFDetector) Detect(text string) []Match {
	return matchesFromRegexp(text, nifPattern, CategoryPTNIF, validNIF)
}

// validNIF applies the published NIF rule: the first digit identifies the
// holder-type category (1/2/3 individuals, 5/6/8/9 various entity types
// -- 0, 4 and 7 are never issued), and the ninth digit is a check digit
// computed from the first eight: sum each digit times (9 minus its
// 0-indexed position), reduce mod 11, and the check is 0 when that
// remainder is under 2 or 11 minus the remainder otherwise.
func validNIF(s string) bool {
	switch s[0] {
	case '1', '2', '3', '5', '6', '8', '9':
	default:
		return false
	}
	sum := 0
	for i := 0; i < 8; i++ {
		sum += int(s[i]-'0') * (9 - i)
	}
	r := sum % 11
	check := 11 - r
	if r < 2 {
		check = 0
	}
	return int(s[8]-'0') == check
}
