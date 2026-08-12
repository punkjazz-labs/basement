package docredact

import (
	"errors"
	"strings"
	"testing"
)

func TestAddManualCountsEveryOccurrence(t *testing.T) {
	doc := Analyze("Marta Keller signed. Marta Keller was there. Marta Kellerson was not.")
	finding, err := doc.AddManual("Marta Keller", CategoryPhrase)
	if err != nil {
		t.Fatalf("AddManual: %v", err)
	}
	// "Marta Kellerson" contains the literal, so it counts as a third
	// occurrence: exact substring matching is what the owner selected.
	if finding.Occurrences != 3 {
		t.Errorf("occurrences = %d, want 3", finding.Occurrences)
	}
	if finding.Token != "[PHRASE_1]" {
		t.Errorf("token = %q, want [PHRASE_1]", finding.Token)
	}
	if finding.Source != SourceManual {
		t.Errorf("source = %q, want %q", finding.Source, SourceManual)
	}
	if !finding.Enabled {
		t.Error("a manual finding should be enabled the moment it is added")
	}
	if strings.Contains(doc.Redacted(), "Marta Keller") {
		t.Errorf("redacted = %q, want every occurrence replaced", doc.Redacted())
	}
}

func TestAddManualTrimsAndRefusesEmptyOrAbsentText(t *testing.T) {
	doc := Analyze("The Basel laboratory incident stays confidential.")

	if _, err := doc.AddManual("   ", CategoryPhrase); !errors.Is(err, ErrEmptyLiteral) {
		t.Errorf("empty literal error = %v, want %v", err, ErrEmptyLiteral)
	}
	if _, err := doc.AddManual("the Zurich office", CategoryPhrase); !errors.Is(err, ErrLiteralNotFound) {
		t.Errorf("absent literal error = %v, want %v", err, ErrLiteralNotFound)
	}
	// A browser selection routinely carries a trailing space.
	finding, err := doc.AddManual(" Basel laboratory ", CategoryPhrase)
	if err != nil {
		t.Fatalf("AddManual: %v", err)
	}
	if finding.Literal != "Basel laboratory" {
		t.Errorf("literal = %q, want it trimmed", finding.Literal)
	}
}

func TestAddManualRefusesALiteralAlreadyFound(t *testing.T) {
	doc := Analyze("Write to jane.doe@example.com today.")
	if _, err := doc.AddManual("jane.doe@example.com", CategoryPhrase); !errors.Is(err, ErrLiteralKnown) {
		t.Fatalf("duplicate of a detected literal error = %v, want %v", err, ErrLiteralKnown)
	}
	if _, err := doc.AddManual("today", CategoryPhrase); err != nil {
		t.Fatalf("AddManual: %v", err)
	}
	if _, err := doc.AddManual("today", CategoryPhrase); !errors.Is(err, ErrLiteralKnown) {
		t.Fatalf("duplicate of a manual literal error = %v, want %v", err, ErrLiteralKnown)
	}
	if len(doc.Findings) != 2 {
		t.Fatalf("findings = %d, want the email plus the one phrase", len(doc.Findings))
	}
}

func TestAddManualLongerLiteralWinsOverADetectedOne(t *testing.T) {
	doc := Analyze("Write to jane.doe@example.com today.")
	email := doc.Findings[0]
	if email.Literal != "jane.doe@example.com" {
		t.Fatalf("literal = %q, want the detected email", email.Literal)
	}

	// The selection contains the email, so it is the longer literal and
	// wins: the email has no occurrence of its own left and drops off the
	// list rather than sitting there with a toggle that governs nothing.
	phrase, err := doc.AddManual("to jane.doe@example.com today", CategoryPhrase)
	if err != nil {
		t.Fatalf("AddManual: %v", err)
	}
	if len(doc.Findings) != 1 || doc.Findings[0] != phrase {
		t.Fatalf("findings = %+v, want only the phrase", doc.Findings)
	}
	redacted := doc.Redacted()
	if redacted != "Write [PHRASE_1]." {
		t.Fatalf("redacted = %q, want the whole phrase replaced once", redacted)
	}

	// The email's pseudonym number is spent: a new email literal gets the
	// next one, never a number a different literal already wore.
	other := Analyze("Write to jane.doe@example.com today.")
	if other.Findings[0].Token != "[EMAIL_1]" {
		t.Fatalf("a fresh document should still start at [EMAIL_1], got %q", other.Findings[0].Token)
	}
}

func TestAddManualRefusesTextInsideALongerFinding(t *testing.T) {
	doc := Analyze("Write to jane.doe@example.com today.")
	if _, err := doc.AddManual("example.com", CategoryPhrase); !errors.Is(err, ErrLiteralCovered) {
		t.Fatalf("covered literal error = %v, want %v", err, ErrLiteralCovered)
	}
	// The refused attempt must leave the document exactly as it was.
	if len(doc.Findings) != 1 || doc.Findings[0].Literal != "jane.doe@example.com" {
		t.Fatalf("findings = %+v, want the email alone", doc.Findings)
	}
	if !strings.Contains(doc.Redacted(), "[EMAIL_1]") {
		t.Errorf("redacted = %q, want the email still replaced", doc.Redacted())
	}
}

func TestAddManualKeepsToggleStateOfEveryOtherFinding(t *testing.T) {
	doc := Analyze("Write to jane.doe@example.com or call +41 79 337 22 18 tomorrow.")
	email := doc.Findings[0]
	if !doc.Toggle(email.ID, false) {
		t.Fatal("expected to toggle the email finding")
	}
	if _, err := doc.AddManual("tomorrow", CategoryPhrase); err != nil {
		t.Fatalf("AddManual: %v", err)
	}
	if doc.byLiteral["jane.doe@example.com"].Enabled {
		t.Error("adding a phrase re-enabled a finding the owner had switched off")
	}
	if strings.Contains(doc.Redacted(), "[EMAIL_1]") {
		t.Errorf("redacted = %q, want the disabled email left as written", doc.Redacted())
	}
}

func TestParseCategoryFallsBackToPhrase(t *testing.T) {
	for _, name := range []string{"email", "EMAIL", " iban "} {
		category, known := ParseCategory(name)
		if !known {
			t.Errorf("ParseCategory(%q) reported unknown", name)
		}
		if category.Prefix() == "PHRASE" {
			t.Errorf("ParseCategory(%q) = %q, want the named category", name, category)
		}
	}
	for _, name := range []string{"", "role", "company"} {
		category, known := ParseCategory(name)
		if known {
			t.Errorf("ParseCategory(%q) reported known", name)
		}
		if category != CategoryPhrase {
			t.Errorf("ParseCategory(%q) = %q, want phrase", name, category)
		}
	}
}

func TestManualFindingsReachTheMapping(t *testing.T) {
	doc := Analyze("The Basel laboratory incident stays confidential.")
	if _, err := doc.AddManual("Basel laboratory", CategoryPhrase); err != nil {
		t.Fatalf("AddManual: %v", err)
	}
	entries := doc.Mapping()
	if len(entries) != 1 {
		t.Fatalf("mapping entries = %+v, want one", entries)
	}
	if entries[0].Source != SourceManual || entries[0].Category != string(CategoryPhrase) {
		t.Errorf("entry = %+v, want it to say manual/phrase", entries[0])
	}
}
