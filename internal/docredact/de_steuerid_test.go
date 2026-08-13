package docredact

import (
	"strings"
	"testing"
)

// steuerIDTestCheckDigit independently reimplements the ISO 7064 mod
// 11,10 check-digit formula (see steuerIDCheckDigit in de_steuerid.go)
// so tests that build additional literals are not simply asserting the
// production algorithm against itself.
func steuerIDTestCheckDigit(t *testing.T, first10 string) int {
	t.Helper()
	product := 10
	for i := 0; i < 10; i++ {
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

// TestDESteuerIDCanonicalValidMatch uses a literal independently verified
// against the published ISO 7064 mod 11,10 formula with a throwaway
// script before this test was committed: first10 "1234567801" has digit
// '1' occurring twice (the 2x variant of the digit-structure rule) and
// digit '9' absent, check digit 8.
func TestDESteuerIDCanonicalValidMatch(t *testing.T) {
	const literal = "12345678018"
	text := "Steuer-ID: " + literal + " on the form."
	matches := DESteuerIDDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryDESteuerID {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryDESteuerID)
	}
	if matches[0].Category.Prefix() != "IDNR" {
		t.Errorf("Prefix = %q, want IDNR", matches[0].Category.Prefix())
	}
	start := strings.Index(text, literal)
	if matches[0].Start != start || matches[0].End != start+len(literal) {
		t.Errorf("span = [%d,%d), want [%d,%d)", matches[0].Start, matches[0].End, start, start+len(literal))
	}
}

// TestDESteuerIDThreeTimesVariantValidMatch covers the 3x variant of the
// digit-structure rule: first10 "1234567000" has digit '0' occurring
// three times and digits '8' and '9' absent. Verified independently via
// throwaway script.
func TestDESteuerIDThreeTimesVariantValidMatch(t *testing.T) {
	const literal = "12345670005"
	text := "Steuer-ID " + literal + " noted."
	matches := DESteuerIDDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
}

// TestDESteuerIDFlippedCheckDigitRejected is the canonical literal with
// the last (check) digit changed, which no longer satisfies the ISO 7064
// mod 11,10 formula.
func TestDESteuerIDFlippedCheckDigitRejected(t *testing.T) {
	text := "Steuer-ID: 12345678019 on the form."
	matches := DESteuerIDDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for wrong check digit, got %+v", matches)
	}
}

// TestDESteuerIDLongerDigitRunNotMatched checks word-boundary discipline:
// the valid literal preceded by an extra digit with no separator must not
// be matched as an 11-digit substring of a longer run.
func TestDESteuerIDLongerDigitRunNotMatched(t *testing.T) {
	text := "Reference 912345678018 is not a Steuer-ID."
	matches := DESteuerIDDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match inside a longer digit run, got %+v", matches)
	}
}

// TestDESteuerIDGroupedForm covers the conventional 2-3-3-3 space-grouped
// written form of the canonical valid literal.
func TestDESteuerIDGroupedForm(t *testing.T) {
	const literal = "12 345 678 018"
	text := "Steuer-ID: " + literal + " (grouped form)."
	matches := DESteuerIDDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryDESteuerID {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryDESteuerID)
	}
}

// TestDESteuerIDLeadingZeroRejected checks the structural rule that the
// first digit must not be zero, independently of the check digit: a
// literal starting with 0 must never match even if a valid check digit
// happened to follow, so the pattern itself excludes it.
func TestDESteuerIDLeadingZeroRejected(t *testing.T) {
	text := "ID 02345678018 should not match."
	matches := DESteuerIDDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for leading zero, got %+v", matches)
	}
}

// TestDESteuerIDAllDistinctDigitsRejected checks the digit-structure rule
// independently of the check digit: first10 "1023456789" uses every
// digit value 0-9 exactly once, so no digit occurs two or three times and
// none is absent -- this must be rejected even though its check digit
// (computed independently via steuerIDTestCheckDigit) is correct.
func TestDESteuerIDAllDistinctDigitsRejected(t *testing.T) {
	first10 := "1023456789"
	check := steuerIDTestCheckDigit(t, first10)
	literal := first10 + itoaT(check)
	text := "ID " + literal + " should not match."
	matches := DESteuerIDDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for all-distinct first 10 digits, got %+v", matches)
	}
}

// TestDESteuerIDOtherDigitFourTimesAccepted pins a disclosed judgment
// call rather than testing an intended rule: first10 "1100002345" has
// digit '1' occurring exactly twice (the only value at frequency 2 or 3)
// but digit '0' occurring four times. steuerIDDigitStructureOK implements
// the brief's literal wording -- "exactly one digit value occurs two or
// three times and at least one digit value is absent" -- which only
// counts values at frequency 2 or 3, so a value at frequency 4 does not
// disqualify this number and it is accepted. The fuller real-world
// Steuer-ID rule ("every other digit that occurs, occurs at most once")
// would reject it. This test pins the current (literal-brief) behavior
// so a future refactor cannot silently drift either way without a
// visible test change.
func TestDESteuerIDOtherDigitFourTimesAccepted(t *testing.T) {
	first10 := "1100002345"
	check := steuerIDTestCheckDigit(t, first10)
	literal := first10 + itoaT(check)
	text := "ID " + literal + " on file."
	matches := DESteuerIDDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1 (pinning literal-brief acceptance): %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
}

func itoaT(n int) string {
	return string(rune('0' + n))
}
