package docredact

import (
	"strings"
	"testing"
)

// corpus is the fixture document the exhaustive tests below run against. It
// mixes real, findable literals (positives) with values that look like a
// category but fail its checksum or plausibility rule (near misses), plus
// some literals repeated to exercise whole-document occurrence counting.
//
// Every literal referenced by corpusPositives or corpusNearMisses below
// must appear in this text verbatim, or the test that checks for it is
// checking nothing.
const corpus = `Contact Jane at jane.doe@example.com for details. You can also
reach her at jane.doe@example.com after 5pm, or email jane.doe@example.com
directly. Copy support@example.org on every reply.

Call me at +1 415-555-0132 or (415) 555-0199. My Italian mobile is
+39 345 123 4567 and the office landline is 02 12345678. An order
reference 82-11 is not a phone number, and the time 12:30 is not an
address either.

Wire to GB29 NWBK 6016 1331 9268 19 please. Do not use the old account
GB29 NWBK 6016 1331 9268 18, which fails validation and must be ignored.

Card on file: 4111 1111 1111 1111. The typo version 4111 1111 1111 1112
was rejected by the processor and should never be treated as real.

Server addresses: 192.168.1.10 and 2001:db8::1. The bogus address
999.999.999.999 was never valid and was never assigned to anything.

She was born on 12/03/1985 in Rome. The delivery date on the form,
32/13/2020, is not a real calendar date, and the future date 14/03/2099
cannot be a birth date either, since it has not happened yet. Her ISO
record shows 1985-03-12, and the file also says March 12, 1985 and
12 March 1985.

Codice fiscale RSSMRA85M01H501Q is on file. It appears again here:
RSSMRA85M01H501Q, and a third time here: RSSMRA85M01H501Q. A near-miss
value, RSSMRA85M01H501X, was rejected as invalid by the same check.

SSN 219-09-9999 is on file. The near misses 666-12-3456, 000-12-3456 and
900-12-3456 must never be treated as valid Social Security numbers.
`

type corpusExpectation struct {
	literal     string
	category    Category
	occurrences int
}

// corpusPositives is every literal the pattern pass must find, grouped
// exactly once per exact literal string, with the occurrence count that
// literal actually has in corpus.
var corpusPositives = []corpusExpectation{
	{"jane.doe@example.com", CategoryEmail, 3},
	{"support@example.org", CategoryEmail, 1},
	{"+1 415-555-0132", CategoryPhone, 1},
	{"(415) 555-0199", CategoryPhone, 1},
	{"+39 345 123 4567", CategoryPhone, 1},
	{"02 12345678", CategoryPhone, 1},
	{"GB29 NWBK 6016 1331 9268 19", CategoryIBAN, 1},
	{"4111 1111 1111 1111", CategoryCard, 1},
	{"192.168.1.10", CategoryIPv4, 1},
	{"2001:db8::1", CategoryIPv6, 1},
	{"12/03/1985", CategoryDOB, 1},
	{"1985-03-12", CategoryDOB, 1},
	{"March 12, 1985", CategoryDOB, 1},
	{"12 March 1985", CategoryDOB, 1},
	{"RSSMRA85M01H501Q", CategoryITCF, 3},
	{"219-09-9999", CategoryUSSSN, 1},
}

// corpusNearMisses are values that look like a category on the surface but
// must never be found: a Luhn failure, an IBAN checksum failure, calendar
// dates that cannot be a birth date (an invalid date and a future one), an
// invalid codice fiscale check character, and SSNs in the SSA's known-
// invalid ranges (666, 000, 900-999).
var corpusNearMisses = []string{
	"GB29 NWBK 6016 1331 9268 18",
	"4111 1111 1111 1112",
	"999.999.999.999",
	"32/13/2020",
	"14/03/2099",
	"RSSMRA85M01H501X",
	"666-12-3456",
	"000-12-3456",
	"900-12-3456",
}

func TestCorpusPositivesFoundAndGrouped(t *testing.T) {
	doc := Analyze(corpus)
	byLiteral := make(map[string]*Finding, len(doc.Findings))
	for _, f := range doc.Findings {
		byLiteral[f.Literal] = f
	}
	for _, want := range corpusPositives {
		got, ok := byLiteral[want.literal]
		if !ok {
			t.Errorf("literal %q (%s) was not found", want.literal, want.category)
			continue
		}
		if got.Category != want.category {
			t.Errorf("literal %q: category = %s, want %s", want.literal, got.Category, want.category)
		}
		if got.Occurrences != want.occurrences {
			t.Errorf("literal %q: occurrences = %d, want %d", want.literal, got.Occurrences, want.occurrences)
		}
		if got.Source != Source {
			t.Errorf("literal %q: source = %q, want %q", want.literal, got.Source, Source)
		}
		if !got.Enabled {
			t.Errorf("literal %q: expected enabled by default", want.literal)
		}
		if !strings.HasPrefix(got.Token, "["+want.category.Prefix()+"_") {
			t.Errorf("literal %q: token %q does not start with [%s_", want.literal, got.Token, want.category.Prefix())
		}
	}
}

func TestCorpusNearMissesNeverFound(t *testing.T) {
	doc := Analyze(corpus)
	for _, literal := range corpusNearMisses {
		if !strings.Contains(corpus, literal) {
			t.Fatalf("test fixture bug: near-miss %q is not actually in the corpus", literal)
		}
		for _, f := range doc.Findings {
			if f.Literal == literal {
				t.Errorf("near-miss %q was incorrectly found as a %s finding", literal, f.Category)
			}
		}
	}
}

// TestRedactedOutputContainsNoEnabledLiteral is the assertion the whole
// product rests on: with every finding enabled (the default), none of the
// positive literals may appear anywhere in the redacted output, byte for
// byte, over the whole corpus.
func TestRedactedOutputContainsNoEnabledLiteral(t *testing.T) {
	doc := Analyze(corpus)
	redacted := doc.Redacted()
	for _, want := range corpusPositives {
		if strings.Contains(redacted, want.literal) {
			t.Errorf("redacted output still contains enabled literal %q", want.literal)
		}
	}
	// A near miss was never a finding, so it must be left exactly as
	// written -- proof that the redaction pass only ever touches what it
	// found, never anything that merely looks similar.
	for _, literal := range corpusNearMisses {
		if !strings.Contains(redacted, literal) {
			t.Errorf("redacted output no longer contains near-miss %q, which should have been left alone", literal)
		}
	}
}

// TestDisabledFindingLeavesLiteralInPlace proves the inverse of the
// exhaustive test above: turning a finding off is what keeps its literal in
// the output, not a bug.
func TestDisabledFindingLeavesLiteralInPlace(t *testing.T) {
	doc := Analyze(corpus)
	var email *Finding
	for _, f := range doc.Findings {
		if f.Literal == "jane.doe@example.com" {
			email = f
		}
	}
	if email == nil {
		t.Fatal("expected to find jane.doe@example.com")
	}
	if !doc.Toggle(email.ID, false) {
		t.Fatalf("toggle of %q failed", email.ID)
	}
	redacted := doc.Redacted()
	if !strings.Contains(redacted, "jane.doe@example.com") {
		t.Error("disabling a finding should leave its literal in the redacted output")
	}
	if strings.Contains(redacted, email.Token) {
		t.Error("a disabled finding must not appear as a replacement token either")
	}
}
