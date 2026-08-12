package docredact

import "regexp"

// cardPattern matches 13-19 digits, optionally grouped by single spaces or
// dashes -- the two conventional ways a card number is written. luhnValid
// is the real validator.
var cardPattern = regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)

type CardDetector struct{}

func (CardDetector) Name() string { return "card" }

func (CardDetector) Detect(text string) []Match {
	return matchesFromRegexp(text, cardPattern, CategoryCard, func(s string) bool {
		digits := digitsOnly(s)
		if len(digits) < 13 || len(digits) > 19 {
			return false
		}
		return luhnValid(digits)
	})
}

func digitsOnly(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// luhnValid checks the Luhn (mod 10) checksum used by every major card
// network: from the rightmost digit, double every second digit, subtract 9
// from any result over 9, and the total must be a multiple of 10.
func luhnValid(digits string) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}
