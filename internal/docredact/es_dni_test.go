package docredact

import (
	"strings"
	"testing"
)

// dniTestLetter independently reimplements the Spanish control-letter
// formula (see dniControlLetters in es_dni.go) so tests that build
// additional literals are not simply asserting the production algorithm
// against itself.
func dniTestLetter(n int) string {
	const letters = "TRWAGMYFPDXBNJZSQVHLCKE"
	return string(letters[n%23])
}

// TestESDNICanonicalValidMatch uses the canonical DNI value named in the
// task brief, verified against the published control-letter table
// (12345678 mod 23 = 14, letters[14] = 'Z') with a throwaway script
// before this test was committed.
func TestESDNICanonicalValidMatch(t *testing.T) {
	const literal = "12345678Z"
	text := "Her DNI is " + literal + " on the contract."
	matches := ESDNIDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryESDNI {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryESDNI)
	}
	if matches[0].Category.Prefix() != "DNI" {
		t.Errorf("Prefix = %q, want DNI", matches[0].Category.Prefix())
	}
	start := strings.Index(text, literal)
	if matches[0].Start != start || matches[0].End != start+len(literal) {
		t.Errorf("span = [%d,%d), want [%d,%d)", matches[0].Start, matches[0].End, start, start+len(literal))
	}
}

func TestESDNIFlippedLetterRejected(t *testing.T) {
	text := "Her DNI is 12345678A on the contract."
	matches := ESDNIDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for wrong control letter, got %+v", matches)
	}
}

// TestESDNILongerDigitRunNotMatched checks word-boundary discipline: a
// valid 8-digit+letter literal preceded by an extra digit with no
// separator must not be matched.
func TestESDNILongerDigitRunNotMatched(t *testing.T) {
	text := "Order number 112345678Z should not be read as a DNI."
	matches := ESDNIDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match inside a longer digit run, got %+v", matches)
	}
}

// TestESDNINIEWithXAndZ covers both the X and Z leading letters of the
// NIE form. n computed independently (X/Y/Z -> 0/1/2 prepended to the 7
// digits) and the control letter via dniTestLetter.
func TestESDNINIEWithXAndZ(t *testing.T) {
	cases := []struct {
		prefix string
		digits string
		lead   int
	}{
		{"X", "1234567", 0},
		{"Z", "9876543", 2},
	}
	for _, c := range cases {
		n := c.lead*10000000 + atoiT(t, c.digits)
		letter := dniTestLetter(n)
		literal := c.prefix + c.digits + letter
		text := "NIE on file: " + literal + "."
		matches := ESDNIDetector{}.Detect(text)
		if len(matches) != 1 {
			t.Fatalf("%s: got %d matches, want 1: %+v", literal, len(matches), matches)
		}
		if matches[0].Text != literal {
			t.Errorf("%s: Text = %q, want %q", literal, matches[0].Text, literal)
		}
		if matches[0].Category != CategoryESDNI {
			t.Errorf("%s: Category = %q, want %q", literal, matches[0].Category, CategoryESDNI)
		}
	}
}

// TestESDNIPlainWithSeparator covers the optional single space or hyphen
// before the control letter.
func TestESDNIPlainWithSeparator(t *testing.T) {
	for _, literal := range []string{"12345678 Z", "12345678-Z"} {
		text := "DNI: " + literal + " noted."
		matches := ESDNIDetector{}.Detect(text)
		if len(matches) != 1 {
			t.Fatalf("%s: got %d matches, want 1: %+v", literal, len(matches), matches)
		}
		if matches[0].Text != literal {
			t.Errorf("%s: Text = %q, want %q", literal, matches[0].Text, literal)
		}
	}
}

// TestESDNILowercaseLetterNotMatched: uppercase only, see the comment on
// dniPattern in es_dni.go for why.
func TestESDNILowercaseLetterNotMatched(t *testing.T) {
	text := "dni 12345678z should not match."
	matches := ESDNIDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for lowercase control letter, got %+v", matches)
	}
}

func atoiT(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}
