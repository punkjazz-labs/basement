package docredact

import (
	"strings"
	"testing"
)

// TestResolveOverlapsLongerLiteralWins pins the exact rule the spec
// requires: when two matches overlap, the longer literal survives and the
// shorter one is dropped, regardless of which one starts first or which
// detector produced it.
func TestResolveOverlapsLongerLiteralWins(t *testing.T) {
	// "AAAAAAAAAA" (0-10) is fully covered by an unrelated shorter match
	// "AAAAA" (3-8) nested inside it. The nested, shorter match must lose.
	nested := []Match{
		{Start: 0, End: 10, Text: "AAAAAAAAAA", Category: CategoryEmail},
		{Start: 3, End: 8, Text: "AAAAA", Category: CategoryPhone},
	}
	got := ResolveOverlaps(nested)
	if len(got) != 1 || got[0].Text != "AAAAAAAAAA" {
		t.Fatalf("nested case: got %+v, want only the longer match", got)
	}

	// A later-starting but longer match must still beat an earlier,
	// shorter one it partially overlaps -- proving the rule is "longer
	// wins", not "first wins" or "earliest start wins".
	partial := []Match{
		{Start: 0, End: 5, Text: "short", Category: CategoryEmail},
		{Start: 3, End: 12, Text: "overlapslong", Category: CategoryPhone},
	}
	got = ResolveOverlaps(partial)
	if len(got) != 1 || got[0].Text != "overlapslong" {
		t.Fatalf("partial-overlap case: got %+v, want only the longer, later-starting match", got)
	}

	// Non-overlapping matches are both kept, in document order.
	separate := []Match{
		{Start: 20, End: 25, Text: "later", Category: CategoryEmail},
		{Start: 0, End: 6, Text: "earlie", Category: CategoryPhone},
	}
	got = ResolveOverlaps(separate)
	if len(got) != 2 || got[0].Text != "earlie" || got[1].Text != "later" {
		t.Fatalf("non-overlapping case: got %+v, want both, in document order", got)
	}
}

// TestAnalyzeAppliesOverlapResolution proves the same rule holds end to
// end through Analyze and Redacted, not just inside ResolveOverlaps
// itself, using two real detectors that genuinely overlap on the same
// text: the email detector matches "user@192.168.1.1.com" whole, and the
// IPv4 detector independently matches "192.168.1.1" nested inside it. The
// email is longer and must be the only finding and the only thing
// replaced; the nested IPv4 candidate must not appear as a finding at all.
func TestAnalyzeAppliesOverlapResolution(t *testing.T) {
	text := "Reach user@192.168.1.1.com for support."
	doc := Analyze(text)

	if len(doc.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one (the longer, email match)", doc.Findings)
	}
	found := doc.Findings[0]
	if found.Literal != "user@192.168.1.1.com" || found.Category != CategoryEmail {
		t.Fatalf("finding = %+v, want the whole email address, not the nested IPv4", found)
	}

	redacted := doc.Redacted()
	if strings.Contains(redacted, "192.168.1.1") {
		t.Error("redacted output still contains the nested IPv4 literal that lost the overlap")
	}
	if strings.Contains(redacted, "user@192.168.1.1.com") {
		t.Error("redacted output still contains the winning email literal")
	}
}
