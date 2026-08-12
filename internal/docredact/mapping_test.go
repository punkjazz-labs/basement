package docredact

import (
	"bytes"
	"strings"
	"testing"
)

func TestMappingRoundTrip(t *testing.T) {
	doc := Analyze(corpus)

	data, err := doc.MappingBytes()
	if err != nil {
		t.Fatalf("MappingBytes: %v", err)
	}

	warning, entries, err := ParseMapping(data)
	if err != nil {
		t.Fatalf("ParseMapping: %v", err)
	}
	if warning != MappingWarning {
		t.Errorf("warning = %q, want %q", warning, MappingWarning)
	}

	want := doc.Mapping()
	if len(entries) != len(want) {
		t.Fatalf("round-tripped %d entries, want %d", len(entries), len(want))
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

// TestMappingWarningIsTheFirstLine pins the spec's exact requirement: the
// first line of the mapping bytes is a plain warning sentence, not a JSON
// field a reader could skim past.
func TestMappingWarningIsTheFirstLine(t *testing.T) {
	doc := Analyze(corpus)
	data, err := doc.MappingBytes()
	if err != nil {
		t.Fatal(err)
	}
	firstLine, _, found := bytes.Cut(data, []byte("\n"))
	if !found {
		t.Fatal("mapping bytes have no newline at all")
	}
	if string(firstLine) != MappingWarning {
		t.Errorf("first line = %q, want the plain warning %q", firstLine, MappingWarning)
	}
}

// TestOnlyEnabledFindingsAppearInMapping proves a disabled finding's
// literal is never written to the mapping: it was never replaced, so
// there is no pseudonym for it to be mapped from.
func TestOnlyEnabledFindingsAppearInMapping(t *testing.T) {
	doc := Analyze(corpus)
	var target *Finding
	for _, f := range doc.Findings {
		if f.Literal == "support@example.org" {
			target = f
		}
	}
	if target == nil {
		t.Fatal("expected to find support@example.org")
	}
	doc.Toggle(target.ID, false)

	for _, entry := range doc.Mapping() {
		if entry.Literal == "support@example.org" {
			t.Error("a disabled finding's literal must not appear in the mapping")
		}
	}
	data, err := doc.MappingBytes()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("support@example.org")) {
		t.Error("a disabled finding's literal must not appear in the mapping bytes")
	}
}

// TestMappingNeverWrittenIntoRedactedBytes pins that the two outputs are
// built from entirely separate data: nothing distinguishing about the
// mapping (its warning line, or the literal text of any enabled finding)
// ever ends up inside the redacted text.
func TestMappingNeverWrittenIntoRedactedBytes(t *testing.T) {
	doc := Analyze(corpus)
	redacted := []byte(doc.Redacted())
	mapping, err := doc.MappingBytes()
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(redacted, []byte(MappingWarning)) {
		t.Error("redacted output contains the mapping's warning line")
	}
	for _, entry := range doc.Mapping() {
		if strings.Contains(string(redacted), entry.Literal) {
			t.Errorf("redacted output contains literal %q from the mapping", entry.Literal)
		}
	}
	// Byte-for-byte: the redacted document is never a substring of the
	// mapping payload or vice versa, since they are written from two
	// independent calls (Redacted and MappingBytes) that share no buffer.
	if bytes.Contains(mapping, redacted) || bytes.Contains(redacted, mapping) {
		t.Error("the redacted output and the mapping bytes overlap, and they must never share a buffer")
	}
}
