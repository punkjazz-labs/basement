package docredact

import (
	"strconv"
	"strings"
	"testing"
)

// nifTestCheck independently reimplements the PT NIF check-digit formula
// (see validNIF in pt_nif.go) so tests that build additional literals are
// not simply asserting the production algorithm against itself.
func nifTestCheck(first8 string) string {
	sum := 0
	for i := 0; i < 8; i++ {
		sum += int(first8[i]-'0') * (9 - i)
	}
	r := sum % 11
	check := 11 - r
	if r < 2 {
		check = 0
	}
	return strconv.Itoa(check)
}

// TestPTNIFValidMatch uses a literal verified against the published
// check-digit formula with a throwaway script before this test was
// committed: first8 = 12345678 -> check digit 9.
func TestPTNIFValidMatch(t *testing.T) {
	const literal = "123456789"
	text := "NIF " + literal + " is on the invoice."
	matches := PTNIFDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryPTNIF {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryPTNIF)
	}
	if matches[0].Category.Prefix() != "NIF" {
		t.Errorf("Prefix = %q, want NIF", matches[0].Category.Prefix())
	}
	start := strings.Index(text, literal)
	if matches[0].Start != start || matches[0].End != start+len(literal) {
		t.Errorf("span = [%d,%d), want [%d,%d)", matches[0].Start, matches[0].End, start, start+len(literal))
	}
}

func TestPTNIFFlippedCheckDigitRejected(t *testing.T) {
	text := "NIF 123456780 is on the invoice."
	matches := PTNIFDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for wrong check digit, got %+v", matches)
	}
}

// TestPTNIFLongerDigitRunNotMatched checks word-boundary discipline: the
// valid 9-digit literal above with an extra trailing digit must not be
// matched as a 9-digit prefix of a longer run.
func TestPTNIFLongerDigitRunNotMatched(t *testing.T) {
	text := "Order 1234567891 should not be read as a NIF."
	matches := PTNIFDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match inside a longer digit run, got %+v", matches)
	}
}

// TestPTNIFAdditionalValidPrefixes covers the plain 9-digit form with
// different valid leading digits, check digits computed independently
// via nifTestCheck.
func TestPTNIFAdditionalValidPrefixes(t *testing.T) {
	for _, first8 := range []string{"50000000", "20000000"} {
		literal := first8 + nifTestCheck(first8)
		text := "NIF: " + literal + " noted."
		matches := PTNIFDetector{}.Detect(text)
		if len(matches) != 1 {
			t.Fatalf("%s: got %d matches, want 1: %+v", literal, len(matches), matches)
		}
		if matches[0].Text != literal {
			t.Errorf("%s: Text = %q, want %q", literal, matches[0].Text, literal)
		}
	}
}

// TestPTNIFInvalidFirstDigitRejected: 0, 4 and 7 are never issued as the
// first digit, even though the check digit below is computed correctly
// for this body.
func TestPTNIFInvalidFirstDigitRejected(t *testing.T) {
	first8 := "40000000"
	text := "NIF " + first8 + nifTestCheck(first8) + " noted."
	matches := PTNIFDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for leading digit 4, got %+v", matches)
	}
}
