package docredact

import (
	"strings"
	"testing"
)

// TestUKNINOValidMatch uses HMRC's own published example format (the two
// prefix letters A and B are both structurally allowed: neither is in the
// forbidden-first or forbidden-second sets, and "AB" is not a forbidden
// pair) as the plain contiguous form.
func TestUKNINOValidMatch(t *testing.T) {
	const literal = "AB123456C"
	text := "His NINO is " + literal + " on the form."
	matches := UKNINODetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryUKNINO {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryUKNINO)
	}
	if matches[0].Category.Prefix() != "NINO" {
		t.Errorf("Prefix = %q, want NINO", matches[0].Category.Prefix())
	}
	start := strings.Index(text, literal)
	if matches[0].Start != start || matches[0].End != start+len(literal) {
		t.Errorf("span = [%d,%d), want [%d,%d)", matches[0].Start, matches[0].End, start, start+len(literal))
	}
}

// TestUKNINOGroupedForm covers the conventional space-grouped written
// form "AA 12 34 56 C".
func TestUKNINOGroupedForm(t *testing.T) {
	const literal = "AB 12 34 56 C"
	text := "NINO: " + literal + " (grouped form)."
	matches := UKNINODetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryUKNINO {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryUKNINO)
	}
}

// TestUKNINOForbiddenFirstLetterRejected uses the task brief's own
// example: Q is never issued as the first letter.
func TestUKNINOForbiddenFirstLetterRejected(t *testing.T) {
	text := "NINO QQ 12 34 56 A is not valid."
	matches := UKNINODetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for forbidden first letter, got %+v", matches)
	}
}

// TestUKNINOForbiddenSecondLetterRejected isolates the second-letter-only
// rule: O is never issued as the second letter, but (unlike D/F/I/Q/U/V)
// it is not forbidden as the first letter.
func TestUKNINOForbiddenSecondLetterRejected(t *testing.T) {
	text := "NINO AO123456B is not valid."
	matches := UKNINODetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for forbidden second letter, got %+v", matches)
	}
}

// TestUKNINOForbiddenPairRejected covers the prefix-pair rule
// independently of the per-letter rules: none of B, G, N, K, T, or Z is
// individually forbidden in its position, but these pairs are never
// issued per HMRC's published allocation rules (NIM39110).
func TestUKNINOForbiddenPairRejected(t *testing.T) {
	cases := []string{"BG123456C", "GB123456C", "NK123456C", "ZZ123456C"}
	for _, literal := range cases {
		text := "NINO " + literal + " is not valid."
		matches := UKNINODetector{}.Detect(text)
		if len(matches) != 0 {
			t.Errorf("%s: expected no match for forbidden pair, got %+v", literal, matches)
		}
	}
}

// TestUKNINOBTPrefixValid pins the fix for a brief/reality mismatch: the
// task brief listed BT as a forbidden pair, but HMRC's published rules
// (NIM39110) do not exclude it -- BG is the excluded B-prefix pair, not
// BT. An otherwise-valid NINO starting "BT" must match.
func TestUKNINOBTPrefixValid(t *testing.T) {
	const literal = "BT123456C"
	text := "NINO " + literal + " is valid."
	matches := UKNINODetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
}

// TestUKNINOSuffixLetterEnforced checks that only A-D is accepted as the
// suffix letter: E is one past the allowed range.
func TestUKNINOSuffixLetterEnforced(t *testing.T) {
	text := "NINO AB123456E is not valid."
	matches := UKNINODetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for out-of-range suffix letter, got %+v", matches)
	}
}

// TestUKNINOLowercaseNotMatched: uppercase only, same reasoning as the
// ES DNI control letter (see dniPattern's comment in es_dni.go).
func TestUKNINOLowercaseNotMatched(t *testing.T) {
	text := "nino ab123456c is not valid."
	matches := UKNINODetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for lowercase NINO, got %+v", matches)
	}
}

// TestUKNINOLongerRunNotMatched checks word-boundary discipline: an
// otherwise-valid literal with an extra trailing digit (both digit and
// letter are word characters, so \b does not fire between them) must not
// be matched as a substring of a longer run.
func TestUKNINOLongerRunNotMatched(t *testing.T) {
	text := "Reference AB123456C1 is not a NINO."
	matches := UKNINODetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match inside a longer run, got %+v", matches)
	}
}
