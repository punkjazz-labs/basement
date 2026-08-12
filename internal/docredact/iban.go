package docredact

import (
	"regexp"
	"strconv"
	"strings"
)

// ibanPattern matches the shape of an IBAN written either solid or in the
// conventional space-separated groups of four: two letters, two check
// digits, then 11-30 more letters/digits (with optional single spaces
// between groups). ibanMod97 is the real validator.
var ibanPattern = regexp.MustCompile(`\b[A-Z]{2}\d{2}(?:[ ]?[A-Z0-9]{2,4}){3,8}\b`)

// ibanLength is the known total character length (letters+digits, no
// spaces) for countries this build has bothered to pin down. It is a
// tightening check on top of ibanMod97, not a replacement for it: a country
// not in this map still gets the mod-97 check and the generic 15-34 length
// bound, just not the extra precision. Extend this map as more locales
// matter; it is not the locale list (see Locales), just an accuracy aid.
var ibanLength = map[string]int{
	"IT": 27, "DE": 22, "FR": 27, "ES": 24, "GB": 22, "NL": 18, "CH": 21,
	"BE": 16, "AT": 20, "PT": 25, "IE": 22, "PL": 28,
}

type IBANDetector struct{}

func (IBANDetector) Name() string { return "iban" }

func (IBANDetector) Detect(text string) []Match {
	return matchesFromRegexp(text, ibanPattern, CategoryIBAN, isValidIBAN)
}

func isValidIBAN(literal string) bool {
	compact := strings.ToUpper(strings.ReplaceAll(literal, " ", ""))
	if len(compact) < 15 || len(compact) > 34 {
		return false
	}
	country := compact[0:2]
	if want, ok := ibanLength[country]; ok && len(compact) != want {
		return false
	}
	for _, r := range compact {
		if !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return ibanMod97(compact) == 1
}

// ibanMod97 implements ISO 7064 mod-97-10 as IBAN uses it: move the first
// four characters to the end, expand every letter to two digits (A=10 ...
// Z=35), then reduce the resulting decimal digit string modulo 97 by
// processing it in chunks small enough to fit an int, since the full
// number is far too large for one.
func ibanMod97(compact string) int {
	rearranged := compact[4:] + compact[0:4]
	var digits strings.Builder
	for _, r := range rearranged {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		} else {
			digits.WriteString(strconv.Itoa(int(r-'A') + 10))
		}
	}
	remainder := 0
	s := digits.String()
	for i := 0; i < len(s); i++ {
		remainder = (remainder*10 + int(s[i]-'0')) % 97
	}
	return remainder
}
