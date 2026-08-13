package docredact

import (
	"strings"
	"testing"
)

// bsnTestElfproef independently reimplements the Dutch elfproef with a
// negative last weight (see validBSN in nl_bsn.go) so tests that build
// additional literals are not simply asserting the production algorithm
// against itself.
func bsnTestElfproef(t *testing.T, s string) bool {
	t.Helper()
	weights := [8]int{9, 8, 7, 6, 5, 4, 3, 2}
	sum := 0
	for i, w := range weights {
		sum += w * int(s[i]-'0')
	}
	sum -= int(s[8] - '0')
	allZero := true
	for i := 0; i < 9; i++ {
		if s[i] != '0' {
			allZero = false
		}
	}
	return sum%11 == 0 && !allZero
}

// TestNLBSNCanonicalValidMatch uses the canonical BSN value named in the
// task brief, verified independently against the published elfproef
// formula with a throwaway script before this test was committed:
// 9*1+8*1+7*1+6*2+5*2+4*2+3*3+2*3-1*3 = 66, 66 mod 11 = 0.
func TestNLBSNCanonicalValidMatch(t *testing.T) {
	const literal = "111222333"
	if !bsnTestElfproef(t, literal) {
		t.Fatalf("test setup: %q does not satisfy the independent elfproef check", literal)
	}
	text := "His BSN is " + literal + " on the form."
	matches := NLBSNDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
	if matches[0].Category != CategoryNLBSN {
		t.Errorf("Category = %q, want %q", matches[0].Category, CategoryNLBSN)
	}
	if matches[0].Category.Prefix() != "BSN" {
		t.Errorf("Prefix = %q, want BSN", matches[0].Category.Prefix())
	}
	start := strings.Index(text, literal)
	if matches[0].Start != start || matches[0].End != start+len(literal) {
		t.Errorf("span = [%d,%d), want [%d,%d)", matches[0].Start, matches[0].End, start, start+len(literal))
	}
}

// TestNLBSNFlippedDigitRejected is the canonical literal with the last
// digit changed, which no longer satisfies the elfproef.
func TestNLBSNFlippedDigitRejected(t *testing.T) {
	text := "His BSN is 111222334 on the form."
	matches := NLBSNDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for wrong check digit, got %+v", matches)
	}
}

// TestNLBSNLongerDigitRunNotMatched checks word-boundary discipline: the
// canonical literal with an extra leading digit and no separator must not
// be matched as a 9-digit substring of a longer run.
func TestNLBSNLongerDigitRunNotMatched(t *testing.T) {
	text := "Reference 9111222333 is not a BSN."
	matches := NLBSNDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match inside a longer digit run, got %+v", matches)
	}
}

// TestNLBSNLeadingZeroValidMatch covers the case a BSN starts with a
// leading zero -- still 9 digits, still valid, per the brief. Verified
// independently: 9*0+8*1+7*2+6*3+5*4+4*5+3*6+2*1-1*1 = 99, 99 mod 11 = 0.
func TestNLBSNLeadingZeroValidMatch(t *testing.T) {
	const literal = "012345611"
	if !bsnTestElfproef(t, literal) {
		t.Fatalf("test setup: %q does not satisfy the independent elfproef check", literal)
	}
	text := "BSN " + literal + " noted."
	matches := NLBSNDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != literal {
		t.Errorf("Text = %q, want %q", matches[0].Text, literal)
	}
}

// TestNLBSNAllZerosRejected checks the not-all-zeros rule independently
// of the elfproef arithmetic: "000000000" sums to zero, which trivially
// satisfies mod 11 == 0, but must still be rejected.
func TestNLBSNAllZerosRejected(t *testing.T) {
	text := "BSN 000000000 should not match."
	matches := NLBSNDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for all-zeros BSN, got %+v", matches)
	}
}
