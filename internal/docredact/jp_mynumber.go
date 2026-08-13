package docredact

import "regexp"

// myNumberPattern matches only the grouped written form of a Japanese My
// Number, NNNN NNNN NNNN with a single space or hyphen between each group
// (the two separators need not match each other). A bare 12-digit run is
// deliberately not matched: without separators it is indistinguishable
// from any other 12-digit number in a document, the same reasoning as the
// US SSN's hyphenated-only decision -- see ssnPattern's comment in
// us_ssn.go. jpCheckDigit and jpHasAdjacentGroup are the real validators.
var myNumberPattern = regexp.MustCompile(`\b(\d{4})[ -](\d{4})[ -](\d{4})\b`)

type JPMyNumberDetector struct{}

func (JPMyNumberDetector) Name() string { return "jp_my_number" }

func (JPMyNumberDetector) Detect(text string) []Match {
	var out []Match
	for _, loc := range myNumberPattern.FindAllStringSubmatchIndex(text, -1) {
		if jpHasAdjacentGroup(text, loc[0], loc[1]) {
			continue
		}
		g1 := text[loc[2]:loc[3]]
		g2 := text[loc[4]:loc[5]]
		g3 := text[loc[6]:loc[7]]
		digits := g1 + g2 + g3
		first11 := digits[:11]
		checkDigit := int(digits[11] - '0')
		if jpCheckDigit(first11) != checkDigit {
			continue
		}
		out = append(out, Match{
			Start:    loc[0],
			End:      loc[1],
			Text:     text[loc[0]:loc[1]],
			Category: CategoryJPMyNumber,
			Source:   Source,
		})
	}
	return out
}

// jpHasAdjacentGroup reports whether the match at [start,end) sits inside a
// longer run of separator-joined 4-digit groups -- a fourth group glued on
// either side with the same kind of separator the pattern itself accepts.
// A real My Number is always exactly three groups; a candidate that is
// really a prefix or suffix window of some longer grouped number (a
// 4x4-formatted 16-digit card number, for instance) is a false positive
// the check-digit formula alone cannot rule out, since it only has an
// 11-digit payload to work with and any given 11 digits satisfy it about
// one time in eleven. This is the grouped-run analog of the plain
// word-boundary discipline every other detector in this package already
// applies to bare digit runs.
func jpHasAdjacentGroup(text string, start, end int) bool {
	if start >= 5 {
		sep := text[start-1]
		if (sep == ' ' || sep == '-') && jpAllDigits(text[start-5:start-1]) {
			return true
		}
	}
	if end+5 <= len(text) {
		sep := text[end]
		if (sep == ' ' || sep == '-') && jpAllDigits(text[end+1:end+5]) {
			return true
		}
	}
	return false
}

func jpAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
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
