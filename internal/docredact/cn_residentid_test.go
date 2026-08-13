package docredact

import (
	"strings"
	"testing"
)

// cnTestCheckChar independently reimplements the GB 11643 ISO 7064 mod
// 11-2 check-character formula (see cnIDCheckChar in cn_residentid.go) so
// tests that build additional literals are not simply asserting the
// production algorithm against itself.
func cnTestCheckChar(t *testing.T, first17 string) string {
	t.Helper()
	weights := [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	const chars = "10X98765432"
	sum := 0
	for i := 0; i < 17; i++ {
		sum += int(first17[i]-'0') * weights[i]
	}
	return string(chars[sum%11])
}

// TestCNResidentIDValidMatch uses a literal independently verified against
// the published GB 11643 formula with a throwaway script before this test
// was committed: area 110105, birth date 1990-01-01, order 001, check
// digit 0.
func TestCNResidentIDValidMatch(t *testing.T) {
	const literal = "110105199001010010"
	text := "Her ID number is " + literal + " on the form."
	matches := CNResidentIDDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryCNResidentID {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryCNResidentID)
	}
	if matches[0].Category.Prefix() != "CNID" {
		t.Errorf("Prefix = %q, want CNID", matches[0].Category.Prefix())
	}
	start := strings.Index(text, literal)
	if matches[0].Start != start || matches[0].End != start+len(literal) {
		t.Errorf("span = [%d,%d), want [%d,%d)", matches[0].Start, matches[0].End, start, start+len(literal))
	}
}

// TestCNResidentIDCheckCharX covers the check character X: area 110105,
// birth date 1990-01-01, order 007, check char verified independently via
// cnTestCheckChar.
func TestCNResidentIDCheckCharX(t *testing.T) {
	first17 := "110105" + "19900101" + "007"
	check := cnTestCheckChar(t, first17)
	if check != "X" {
		t.Fatalf("test setup: expected check char X, got %q", check)
	}
	literal := first17 + check
	text := "ID: " + literal + " noted."
	matches := CNResidentIDDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
}

// TestCNResidentIDLowercaseXAccepted covers the same literal as above with
// a lowercase x check character: accepted on input, but the match text
// preserves the literal exactly as written (lowercase), never normalized.
func TestCNResidentIDLowercaseXAccepted(t *testing.T) {
	const literal = "11010519900101007x"
	text := "ID: " + literal + " noted."
	matches := CNResidentIDDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
}

// TestCNResidentIDFlippedCheckCharRejected is the canonical literal with
// the check character changed, which no longer satisfies the ISO 7064
// mod 11-2 formula.
func TestCNResidentIDFlippedCheckCharRejected(t *testing.T) {
	text := "ID 110105199001010019 should not match."
	matches := CNResidentIDDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for wrong check char, got %+v", matches)
	}
}

// TestCNResidentIDInvalidCalendarDateRejected checks the calendar-date
// rule independently of the checksum: birth date 1990-02-30 (February has
// at most 29 days) is not a real date, so this must be rejected even
// though its check char (verified independently via cnTestCheckChar) is
// correct.
func TestCNResidentIDInvalidCalendarDateRejected(t *testing.T) {
	first17 := "110105" + "19900230" + "001"
	check := cnTestCheckChar(t, first17)
	literal := first17 + check
	text := "ID " + literal + " should not match."
	matches := CNResidentIDDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for impossible calendar date, got %+v", matches)
	}
}

// TestCNResidentIDYearOutOfRangeRejected checks the year-range rule
// (1900..current) independently of the checksum: birth year 1899 is a
// real calendar date but predates the accepted range.
func TestCNResidentIDYearOutOfRangeRejected(t *testing.T) {
	first17 := "110105" + "18990101" + "001"
	check := cnTestCheckChar(t, first17)
	literal := first17 + check
	text := "ID " + literal + " should not match."
	matches := CNResidentIDDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for out-of-range birth year, got %+v", matches)
	}
}

// TestCNResidentIDLongerDigitRunNotMatched checks word-boundary discipline:
// the valid literal preceded by an extra digit with no separator must not
// be matched as an 18-character substring of a longer run.
func TestCNResidentIDLongerDigitRunNotMatched(t *testing.T) {
	text := "Reference 5110105199001010010 is not an ID."
	matches := CNResidentIDDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match inside a longer digit run, got %+v", matches)
	}
}
