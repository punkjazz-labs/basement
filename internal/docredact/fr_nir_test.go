package docredact

import (
	"strconv"
	"strings"
	"testing"
)

// nirTestKey independently reimplements the FR NIR check-key formula
// (97 - (first 13 digits mod 97), see nirCheckKey in fr_nir.go) so tests
// that build additional literals are not simply asserting the production
// algorithm against itself.
func nirTestKey(t *testing.T, body string) string {
	t.Helper()
	n, err := strconv.ParseInt(body, 10, 64)
	if err != nil {
		t.Fatalf("bad body %q: %v", body, err)
	}
	key := int(97 - n%97)
	return strconv.Itoa(key)
}

// TestFRNIRValidContiguousMatch uses a literal independently verified
// against the published INSEE key formula with a throwaway script before
// this test was committed: sex=1 (male, born in France), year 85, month
// 03, department 75 (Paris), commune 116, order 001 -> key 27.
func TestFRNIRValidContiguousMatch(t *testing.T) {
	const literal = "185037511600127"
	text := "His NIR is " + literal + " on the form."
	matches := FRNIRDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryFRNIR {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryFRNIR)
	}
	if matches[0].Category.Prefix() != "NIR" {
		t.Errorf("Prefix = %q, want NIR", matches[0].Category.Prefix())
	}
	start := strings.Index(text, literal)
	if matches[0].Start != start || matches[0].End != start+len(literal) {
		t.Errorf("span = [%d,%d), want [%d,%d)", matches[0].Start, matches[0].End, start, start+len(literal))
	}
}

// TestFRNIRFlippedKeyDigitRejected is the same literal as above with the
// last key digit changed (27 -> 28), which no longer satisfies the
// check-key formula.
func TestFRNIRFlippedKeyDigitRejected(t *testing.T) {
	text := "His NIR is 185037511600128 on the form."
	matches := FRNIRDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match, got %+v", matches)
	}
}

// TestFRNIRLongerDigitRunNotMatched checks word-boundary discipline: the
// valid 15-digit literal above with an extra trailing digit must not be
// matched as a 15-digit prefix of a longer run.
func TestFRNIRLongerDigitRunNotMatched(t *testing.T) {
	text := "Reference 1850375116001278 is not a NIR."
	matches := FRNIRDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match inside a longer digit run, got %+v", matches)
	}
}

// TestFRNIRGroupedForm2A covers the space-grouped written form with a
// Corsican department code. body/key computed independently via
// nirTestKey: sex=2, year 90, month 07, department 2A (substitutes to 19
// for the check), commune 004, order 017.
func TestFRNIRGroupedForm2A(t *testing.T) {
	body := "2" + "90" + "07" + "19" + "004" + "017"
	key := nirTestKey(t, body)
	literal := "2 90 07 2A 004 017 " + key
	text := "NIR: " + literal + " (grouped form)."
	matches := FRNIRDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryFRNIR {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryFRNIR)
	}
}

// TestFRNIRContiguousForm2B covers the contiguous written form with a
// Corsican department code and a provisional (foreign-born) sex digit.
// body/key computed independently via nirTestKey: sex=7, year 77, month
// 12, department 2B (substitutes to 18 for the check), commune 001,
// order 123.
func TestFRNIRContiguousForm2B(t *testing.T) {
	body := "7" + "77" + "12" + "18" + "001" + "123"
	key := nirTestKey(t, body)
	literal := "7" + "77" + "12" + "2B" + "001" + "123" + key
	text := "NIR " + literal + " on file."
	matches := FRNIRDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
}

// TestFRNIRInvalidSexDigitRejected checks the structural rule
// independently of the check-key formula: sex digit 3 is never issued
// (only 1, 2, 7, 8 are), even though the key below is computed correctly
// for this body.
func TestFRNIRInvalidSexDigitRejected(t *testing.T) {
	body := "3" + "85" + "03" + "75" + "116" + "001"
	key := nirTestKey(t, body)
	text := "NIR " + body + key + " on file."
	matches := FRNIRDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for sex digit 3, got %+v", matches)
	}
}
