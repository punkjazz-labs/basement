package docredact

import "regexp"

// steuerIDPattern matches an 11-digit German Steuer-ID (Steuerliche
// Identifikationsnummer), either in its contiguous form or the
// conventional space-grouped 2-3-3-3 written form, with an optional
// single space between each group so both written forms share one
// pattern (same convention as nirPattern in fr_nir.go). The first digit
// is constrained to 1-9 in the pattern itself, since a leading zero is
// never issued. steuerIDDigitStructureOK and steuerIDCheckDigit are the
// real validators.
var steuerIDPattern = regexp.MustCompile(`\b([1-9]\d)[ ]?(\d{3})[ ]?(\d{3})[ ]?(\d{3})\b`)

type DESteuerIDDetector struct{}

func (DESteuerIDDetector) Name() string { return "de_steuer_id" }

func (DESteuerIDDetector) Detect(text string) []Match {
	var out []Match
	for _, loc := range steuerIDPattern.FindAllStringSubmatchIndex(text, -1) {
		g1 := text[loc[2]:loc[3]]
		g2 := text[loc[4]:loc[5]]
		g3 := text[loc[6]:loc[7]]
		g4 := text[loc[8]:loc[9]]
		full := g1 + g2 + g3 + g4
		first10 := full[:10]
		if !steuerIDDigitStructureOK(first10) {
			continue
		}
		want := steuerIDCheckDigit(first10)
		if int(full[10]-'0') != want {
			continue
		}
		out = append(out, Match{
			Start:    loc[0],
			End:      loc[1],
			Text:     text[loc[0]:loc[1]],
			Category: CategoryDESteuerID,
			Source:   Source,
		})
	}
	return out
}

// steuerIDDigitStructureOK applies the published digit-structure rule:
// among the first 10 digits, exactly one digit value occurs two or three
// times, and at least one digit value (0-9) is absent. Both the 2x and
// 3x variants are legal; a Steuer-ID with all 10 digits distinct (no
// repeat, nothing absent) fails this rule regardless of its check digit.
func steuerIDDigitStructureOK(first10 string) bool {
	var counts [10]int
	for i := 0; i < len(first10); i++ {
		counts[first10[i]-'0']++
	}
	repeated := 0
	absent := false
	for _, c := range counts {
		if c == 2 || c == 3 {
			repeated++
		}
		if c == 0 {
			absent = true
		}
	}
	return repeated == 1 && absent
}

// steuerIDCheckDigit computes the 11th digit from the first 10 using the
// published ISO 7064 mod 11,10 formula: starting from product=10, for
// each digit d compute sum = (d+product) mod 10 (treating a result of 0
// as 10), then product = (2*sum) mod 11; the check digit is 11-product,
// with the edge case 11 mapped to 0.
func steuerIDCheckDigit(first10 string) int {
	product := 10
	for i := 0; i < len(first10); i++ {
		d := int(first10[i] - '0')
		sum := (d + product) % 10
		if sum == 0 {
			sum = 10
		}
		product = (2 * sum) % 11
	}
	check := 11 - product
	if check == 11 {
		check = 0
	}
	return check
}
