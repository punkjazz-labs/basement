package docredact

import (
	"strings"
	"testing"
)

// jpTestCheckDigit independently reimplements the Digital Agency My Number
// check-digit formula (see jpCheckDigit in jp_mynumber.go) so tests that
// build additional literals are not simply asserting the production
// algorithm against itself.
func jpTestCheckDigit(t *testing.T, first11 string) int {
	t.Helper()
	sum := 0
	for i := 0; i < 11; i++ {
		n := 11 - i
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

// TestJPMyNumberGroupedSpacedValidMatch uses a literal independently
// verified against the published Digital Agency check-digit formula with a
// throwaway script before this test was committed: payload 12345678903
// produces check digit 4.
func TestJPMyNumberGroupedSpacedValidMatch(t *testing.T) {
	const literal = "1234 5678 9034"
	text := "My Number: " + literal + " on the form."
	matches := JPMyNumberDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryJPMyNumber {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryJPMyNumber)
	}
	if matches[0].Category.Prefix() != "MYNUM" {
		t.Errorf("Prefix = %q, want MYNUM", matches[0].Category.Prefix())
	}
	start := strings.Index(text, literal)
	if matches[0].Start != start || matches[0].End != start+len(literal) {
		t.Errorf("span = [%d,%d), want [%d,%d)", matches[0].Start, matches[0].End, start, start+len(literal))
	}
}

// TestJPMyNumberGroupedHyphenatedValidMatch covers the hyphen-grouped
// written form of the same canonical payload.
func TestJPMyNumberGroupedHyphenatedValidMatch(t *testing.T) {
	const literal = "1234-5678-9034"
	text := "My Number: " + literal + " noted."
	matches := JPMyNumberDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
}

// TestJPMyNumberBareTwelveDigitsNotMatched pins the honesty rule from the
// file comment in jp_mynumber.go: a bare 12-digit run, even one whose
// digits satisfy the check-digit formula, is indistinguishable from any
// other 12-digit number and must not be matched -- only the grouped
// written form is.
func TestJPMyNumberBareTwelveDigitsNotMatched(t *testing.T) {
	text := "Reference number 123456789034 is not a My Number here."
	matches := JPMyNumberDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for bare 12-digit run, got %+v", matches)
	}
}

// TestJPMyNumberFlippedCheckDigitRejected is the canonical literal with
// the last (check) digit changed, which no longer satisfies the
// check-digit formula.
func TestJPMyNumberFlippedCheckDigitRejected(t *testing.T) {
	text := "My Number: 1234 5678 9035 on the form."
	matches := JPMyNumberDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for wrong check digit, got %+v", matches)
	}
}

// TestJPMyNumberAllZeroValid pins a boundary of the check-digit formula
// independently verified via jpTestCheckDigit: payload "00000000000"
// produces remainder 0, which the r<=1 rule maps to check digit 0.
func TestJPMyNumberAllZeroValid(t *testing.T) {
	payload := "00000000000"
	check := jpTestCheckDigit(t, payload)
	if check != 0 {
		t.Fatalf("test setup: expected check digit 0, got %d", check)
	}
	literal := "0000 0000 000" + itoaT(check)
	text := "My Number " + literal + " on file."
	matches := JPMyNumberDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
}

// TestJPMyNumberAdjacentFourthGroupNotMatched checks grouped-run boundary
// discipline: a 3-group window that is really a prefix of a longer
// 4-group run (a 16-digit card number formatted NNNN NNNN NNNN NNNN) must
// not be matched, even when its own 11-digit payload happens to satisfy
// the check-digit formula by coincidence -- jpTestCheckDigit confirms
// "411111111111"[:11] = "41111111111" does check out to 1.
func TestJPMyNumberAdjacentFourthGroupNotMatched(t *testing.T) {
	first11 := "41111111111"
	check := jpTestCheckDigit(t, first11)
	if check != 1 {
		t.Fatalf("test setup: expected check digit 1, got %d", check)
	}
	text := "Card on file: 4111 1111 1111 1111, four groups of four."
	matches := JPMyNumberDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for a 3-group window inside a 4-group run, got %+v", matches)
	}
}

// TestJPMyNumberBackToBackPairBothMatch is the reviewer's exact scenario:
// two independently valid My Numbers written back-to-back with a single
// space between them (six groups total, no other text separating them)
// must both be found, each validated on its own -- the run splits cleanly
// into two aligned triples rather than the two candidates suppressing each
// other. First triple is the canonical payload (check digit 4); second is
// the all-zero payload (check digit 0, per TestJPMyNumberAllZeroValid).
func TestJPMyNumberBackToBackPairBothMatch(t *testing.T) {
	const first = "1234 5678 9034"
	const second = "0000 0000 0000"
	text := "IDs on file: " + first + " " + second + " for the two applicants."
	matches := JPMyNumberDetector{}.Detect(text)
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2: %+v", len(matches), matches)
	}
	got := map[string]bool{matches[0].Text: true, matches[1].Text: true}
	if !got[first] || !got[second] {
		t.Errorf("matches = %+v, want texts %q and %q", matches, first, second)
	}
	for _, m := range matches {
		if m.Category != CategoryJPMyNumber {
			t.Errorf("Category = %q, want %q", m.Category, CategoryJPMyNumber)
		}
	}
}

// TestJPMyNumberAdjacentLoneGroupNotMatched pins the documented residual
// from the file comment in jp_mynumber.go: a genuinely valid My Number
// (the canonical payload, check digit 4) with one unrelated 4-digit group
// glued onto it by the same single-space separator (four groups total) is
// not matched at all. Four is not a multiple of three, so there is no
// checksum-anchored way to tell which three of the four groups are the
// real number -- matching either half would be a guess, and this package
// refuses to guess.
func TestJPMyNumberAdjacentLoneGroupNotMatched(t *testing.T) {
	text := "IDs on file: 1234 5678 9034 4321 noted."
	matches := JPMyNumberDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for a valid My Number with one adjacent lone group, got %+v", matches)
	}
}

// TestJPMyNumberLongerDigitRunNotMatched checks word-boundary discipline:
// the valid grouped literal preceded by an extra digit with no separator
// must not be matched.
func TestJPMyNumberLongerDigitRunNotMatched(t *testing.T) {
	text := "Reference 51234 5678 9034 is not a My Number."
	matches := JPMyNumberDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match inside a longer digit run, got %+v", matches)
	}
}
