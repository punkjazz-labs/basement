package docredact

import (
	"strings"
	"testing"
)

// cpfTestCheckDigit independently reimplements the CPF check-digit formula
// (see cpfCheckDigit in br_cpf.go) so tests that build additional literals
// are not simply asserting the production algorithm against itself.
func cpfTestCheckDigit(t *testing.T, prefix []int) int {
	t.Helper()
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

// TestBRCPFCanonicalValidMatch uses the canonical CPF value named in the
// task brief, verified against the published check-digit formula with a
// throwaway script before this test was committed: digits 5 2 9 9 8 2 2 4
// 7 produce first check digit 2 and second check digit 5.
func TestBRCPFCanonicalValidMatch(t *testing.T) {
	const literal = "529.982.247-25"
	text := "Her CPF is " + literal + " on the form."
	matches := BRCPFDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryBRCPF {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryBRCPF)
	}
	if matches[0].Category.Prefix() != "CPF" {
		t.Errorf("Prefix = %q, want CPF", matches[0].Category.Prefix())
	}
	start := strings.Index(text, literal)
	if matches[0].Start != start || matches[0].End != start+len(literal) {
		t.Errorf("span = [%d,%d), want [%d,%d)", matches[0].Start, matches[0].End, start, start+len(literal))
	}
}

// TestBRCPFBareDigitsMatch covers the bare 11-digit written form of the
// same canonical literal.
func TestBRCPFBareDigitsMatch(t *testing.T) {
	const literal = "52998224725"
	text := "CPF: " + literal + " noted."
	matches := BRCPFDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryBRCPF {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryBRCPF)
	}
}

// TestBRCPFFlippedDigitRejected is the canonical literal with the last
// check digit changed, which no longer satisfies the check-digit formula.
func TestBRCPFFlippedDigitRejected(t *testing.T) {
	text := "Her CPF is 529.982.247-26 on the form."
	matches := BRCPFDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for wrong check digit, got %+v", matches)
	}
}

// TestBRCPFLongerDigitRunNotMatched checks word-boundary discipline: the
// valid bare literal preceded by an extra digit with no separator must not
// be matched as an 11-digit substring of a longer run.
func TestBRCPFLongerDigitRunNotMatched(t *testing.T) {
	text := "Reference 152998224725 is not a CPF."
	matches := BRCPFDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match inside a longer digit run, got %+v", matches)
	}
}

// TestBRCPFAllSameDigitRejected pins the Receita Federal's explicit rule
// that a CPF made of eleven identical digits is never valid, even though
// (verified independently via cpfTestCheckDigit) "11111111111" satisfies
// the check-digit arithmetic on its own: check1=1, check2=1.
func TestBRCPFAllSameDigitRejected(t *testing.T) {
	same := make([]int, 11)
	for i := range same {
		same[i] = 1
	}
	c1 := cpfTestCheckDigit(t, same[:9])
	c2 := cpfTestCheckDigit(t, same[:10])
	if c1 != 1 || c2 != 1 {
		t.Fatalf("test setup: expected check digits 1,1, got %d,%d", c1, c2)
	}
	text := "CPF 11111111111 should not match."
	matches := BRCPFDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for all-same-digit CPF, got %+v", matches)
	}
}
