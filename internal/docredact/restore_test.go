package docredact

import (
	"fmt"
	"reflect"
	"testing"
)

func restoreEntries() []MappingEntry {
	return []MappingEntry{
		{Token: "[PERSON_1]", Literal: "Marta Ferretti"},
		{Token: "[PERSON_11]", Literal: "Jonas Weber"},
		{Token: "[ORG_1]", Literal: "Nordwind Logistik GmbH"},
	}
}

func TestRestoreReplacesEveryOccurrence(t *testing.T) {
	text := "Ask [PERSON_1] at [ORG_1]. [PERSON_1] answers on Fridays."
	restored, result := Restore(text, restoreEntries())
	want := "Ask Marta Ferretti at Nordwind Logistik GmbH. Marta Ferretti answers on Fridays."
	if restored != want {
		t.Fatalf("restored = %q, want %q", restored, want)
	}
	if result.Replaced != 3 || result.Tokens != 2 {
		t.Fatalf("result = %+v, want Replaced 3 Tokens 2", result)
	}
	if len(result.Unknown) != 0 {
		t.Fatalf("unknown = %v, want none", result.Unknown)
	}
}

func TestRestoreDoesNotConfuseTokenPrefixes(t *testing.T) {
	// [PERSON_1] must never match inside [PERSON_11]: the closing bracket
	// makes each pseudonym self-delimiting, and this pins it.
	restored, result := Restore("[PERSON_11] then [PERSON_1]", restoreEntries())
	if want := "Jonas Weber then Marta Ferretti"; restored != want {
		t.Fatalf("restored = %q, want %q", restored, want)
	}
	if result.Replaced != 2 || result.Tokens != 2 {
		t.Fatalf("result = %+v, want Replaced 2 Tokens 2", result)
	}
}

func TestRestoreCountsUnknownTokensAndLeavesThemIntact(t *testing.T) {
	text := "[PERSON_1] met [PERSON_9] and [X_2]; [PERSON_9] again."
	restored, result := Restore(text, restoreEntries())
	want := "Marta Ferretti met [PERSON_9] and [X_2]; [PERSON_9] again."
	if restored != want {
		t.Fatalf("restored = %q, want %q", restored, want)
	}
	// Distinct, first-appearance order.
	if wantUnknown := []string{"[PERSON_9]", "[X_2]"}; !reflect.DeepEqual(result.Unknown, wantUnknown) {
		t.Fatalf("unknown = %v, want %v", result.Unknown, wantUnknown)
	}
	if result.Replaced != 1 || result.Tokens != 1 {
		t.Fatalf("result = %+v, want Replaced 1 Tokens 1", result)
	}
}

func TestRestoreIsSinglePass(t *testing.T) {
	// A literal that contains pseudonym-shaped text must arrive verbatim
	// and never be re-scanned: restoration reads the input once.
	entries := []MappingEntry{
		{Token: "[PHRASE_1]", Literal: "see [ORG_1] file"},
		{Token: "[ORG_1]", Literal: "Nordwind"},
	}
	restored, result := Restore("[PHRASE_1] and [ORG_1]", entries)
	if want := "see [ORG_1] file and Nordwind"; restored != want {
		t.Fatalf("restored = %q, want %q", restored, want)
	}
	if result.Replaced != 2 || result.Tokens != 2 || len(result.Unknown) != 0 {
		t.Fatalf("result = %+v, want Replaced 2 Tokens 2 Unknown none", result)
	}
}

func TestRestoreEmptyCases(t *testing.T) {
	if restored, result := Restore("", restoreEntries()); restored != "" || result.Replaced != 0 || len(result.Unknown) != 0 {
		t.Fatalf("empty text: %q %+v", restored, result)
	}
	if restored, result := Restore("no tokens here", nil); restored != "no tokens here" || result.Replaced != 0 {
		t.Fatalf("nil entries: %q %+v", restored, result)
	}
	if result := func() RestoreResult { _, r := Restore("plain", restoreEntries()); return r }(); result.Unknown == nil {
		t.Fatalf("Unknown must be an empty slice, not nil, so the JSON reads [] rather than null")
	}
}

// TestTokenShapeMatchesEveryCategoryPrefix pins tokenShape against every
// Category this package declares, including the default/fallback prefix an
// unrecognized category falls back to. tokenShape is hand-maintained rather
// than derived from Category.Prefix(), so nothing stops a future category
// from picking a prefix tokenShape does not match -- an underscore in the
// prefix, say. Restore would then neither restore that token nor report it
// as unknown: it would simply not look like a token at all, and vanish from
// both counts while sitting untouched in the output. This test is the
// tripwire for that silently wrong case.
func TestTokenShapeMatchesEveryCategoryPrefix(t *testing.T) {
	categories := []Category{
		CategoryEmail, CategoryPhone, CategoryIBAN, CategoryCard, CategoryIPv4, CategoryIPv6,
		CategoryDOB, CategoryITCF, CategoryUSSSN, CategoryFRNIR, CategoryESDNI, CategoryPTNIF,
		CategoryDESteuerID, CategoryNLBSN, CategoryUKNINO, CategoryBRCPF, CategoryCNResidentID,
		CategoryJPMyNumber, CategoryPhrase, CategoryPerson, CategoryOrg, CategoryAddress,
		CategoryJobTitle, CategoryAmount,
		// The fallback Category.Prefix() returns for a name none of the
		// above match ("MATCH"), so the default case is pinned too, not
		// just the named ones.
		Category("unrecognized-category"),
	}
	for _, c := range categories {
		token := fmt.Sprintf("[%s_1]", c.Prefix())
		if !tokenShape.MatchString(token) {
			t.Errorf("category %q: prefix %q does not fit tokenShape (token %q)", c, c.Prefix(), token)
		}
	}
}

func TestRestoreRoundTripsTheCorpus(t *testing.T) {
	// The product invariant: redact, then restore with the mapping, and the
	// original text comes back byte for byte, for every corpus document.
	docs, err := LoadCorpus("testdata/corpus")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	for _, cd := range docs {
		doc := Analyze(cd.Text)
		restored, _ := Restore(doc.Redacted(), doc.Mapping())
		if restored != cd.Text {
			t.Fatalf("%s: restore(redacted) != original", cd.Name)
		}
	}
}
