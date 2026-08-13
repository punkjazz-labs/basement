package docredact

import "regexp"

// cpfPattern matches a Brazilian CPF (Cadastro de Pessoas Físicas) in its
// conventional formatted written form 000.000.000-00 or as a bare
// 11-digit run: each separator (both dots and the hyphen) is
// independently optional, the same relaxed-grouping convention as
// steuerIDPattern in de_steuerid.go. cpfDigitsAllSame and
// cpfCheckDigitsValid are the real validators.
var cpfPattern = regexp.MustCompile(`\b(\d{3})\.?(\d{3})\.?(\d{3})-?(\d{2})\b`)

type BRCPFDetector struct{}

func (BRCPFDetector) Name() string { return "br_cpf" }

func (BRCPFDetector) Detect(text string) []Match {
	var out []Match
	for _, loc := range cpfPattern.FindAllStringSubmatchIndex(text, -1) {
		g1 := text[loc[2]:loc[3]]
		g2 := text[loc[4]:loc[5]]
		g3 := text[loc[6]:loc[7]]
		g4 := text[loc[8]:loc[9]]
		digits := g1 + g2 + g3 + g4
		if cpfDigitsAllSame(digits) {
			continue
		}
		if !cpfCheckDigitsValid(digits) {
			continue
		}
		out = append(out, Match{
			Start:    loc[0],
			End:      loc[1],
			Text:     text[loc[0]:loc[1]],
			Category: CategoryBRCPF,
			Source:   Source,
		})
	}
	return out
}

// cpfDigitsAllSame rejects the eleven known-invalid CPFs made of a single
// repeated digit (000.000.000-00, 111.111.111-11, ... 999.999.999-99):
// several of these -- including 111.111.111-11 -- still satisfy the
// check-digit formula below arithmetically, so the Receita Federal's
// published rule excludes the whole family by this separate, explicit
// check rather than relying on the checksum to catch them.
func cpfDigitsAllSame(digits string) bool {
	for i := 1; i < len(digits); i++ {
		if digits[i] != digits[0] {
			return false
		}
	}
	return true
}

// cpfCheckDigitsValid applies the published CPF check-digit formula to all
// 11 digits: the first check digit (index 9) is derived from the first 9
// digits, the second (index 10) from the first 10 digits (including the
// first check digit). cpfCheckDigit computes each one.
func cpfCheckDigitsValid(digits string) bool {
	d := make([]int, len(digits))
	for i := 0; i < len(digits); i++ {
		d[i] = int(digits[i] - '0')
	}
	if cpfCheckDigit(d[:9]) != d[9] {
		return false
	}
	return cpfCheckDigit(d[:10]) == d[10]
}

// cpfCheckDigit computes one CPF check digit from a prefix of digits: sum
// each digit times a descending weight starting at len(prefix)+1 (10 for
// the first check digit's 9-digit prefix, 11 for the second's 10-digit
// prefix), multiply the sum by 10, reduce mod 11, and map a remainder of
// 10 to 0.
func cpfCheckDigit(prefix []int) int {
	sum := 0
	weight := len(prefix) + 1
	for _, d := range prefix {
		sum += d * weight
		weight--
	}
	r := (sum * 10) % 11
	if r == 10 {
		r = 0
	}
	return r
}
