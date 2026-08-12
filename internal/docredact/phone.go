package docredact

import "regexp"

// phonePattern covers three shapes deliberately, rather than one generic
// digit-run pattern, so that a bare 13-19 digit run (which the card
// detector owns) does not also read as a phone number:
//
//   - international, with a leading "+" and 1-3 digit country code
//   - IT landline/mobile with a leading "0" (landline trunk) or "3"
//     (mobile), optionally prefixed "+39" or "0039"
//   - US/NANP, optionally prefixed "+1", with the conventional
//     3-3-4 grouping and optional parentheses around the area code
//
// All three require at least one separator (space, dot, or dash) between
// digit groups, or a leading "+" -- a bare unbroken digit run never matches
// here, which is what keeps this detector from colliding with card numbers.
// The IT branches tie their optional country-code prefix and the
// separator that follows it into one group, rather than making the
// separator independently optional: a standalone optional separator would
// let \b anchor on the whitespace before the prefix (e.g. the space in
// "landline 02 12345678") and swallow it into the match, off by one
// character from the number itself.
var phonePattern = regexp.MustCompile(
	`\+\d{1,3}[\s.\-]?\(?\d{1,4}\)?(?:[\s.\-]\d{2,4}){1,4}` +
		`|\b(?:(?:0039|\+39)[\s.\-]?)?0\d{1,3}[\s.\-]\d{5,8}\b` +
		`|\b(?:(?:0039|\+39)[\s.\-]?)?3\d{2}[\s.\-]\d{3}[\s.\-]\d{3,4}\b` +
		`|\(\d{3}\)[\s.\-]?\d{3}[\s.\-]?\d{4}\b` +
		`|\b\d{3}[\s.\-]\d{3}[\s.\-]\d{4}\b`,
)

type PhoneDetector struct{}

func (PhoneDetector) Name() string { return "phone" }

func (PhoneDetector) Detect(text string) []Match {
	return matchesFromRegexp(text, phonePattern, CategoryPhone, func(s string) bool {
		digits := 0
		for _, r := range s {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		// E.164 bounds: a real phone number has 7 to 15 digits total.
		return digits >= 7 && digits <= 15
	})
}
