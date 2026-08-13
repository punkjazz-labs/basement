package docredact

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeCompleter scripts a sequence of replies for a Completer, popping one
// per call, so a test can drive ApplyModelPass through a specific sequence
// of transport and parse outcomes without a real model. "STRUCTURED-
// UNSUPPORTED" and "ERROR" are sentinels rather than literal replies.
type fakeCompleter struct {
	replies []string // popped per call; "STRUCTURED-UNSUPPORTED" and "ERROR" are sentinels
	calls   []string // records user prompts for assertions
}

func (f *fakeCompleter) Complete(_ context.Context, _, user string, structured bool) (string, error) {
	f.calls = append(f.calls, user)
	if len(f.replies) == 0 {
		return "", errors.New("no scripted reply")
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	switch r {
	case "STRUCTURED-UNSUPPORTED":
		return "", ErrStructuredUnsupported
	case "ERROR":
		return "", errors.New("connection refused")
	}
	return r, nil
}

func TestApplyModelPassHappyPath(t *testing.T) {
	doc := Analyze("Mario Rossi called. Mario Rossi confirmed by mario.rossi@example.com.")
	fc := &fakeCompleter{replies: []string{`[{"literal":"Mario Rossi","category":"person"}]`}}

	result, err := doc.ApplyModelPass(context.Background(), fc)
	if err != nil {
		t.Fatalf("ApplyModelPass: %v", err)
	}
	if result.Accepted != 1 {
		t.Errorf("accepted = %d, want 1", result.Accepted)
	}

	person, ok := doc.byLiteral["Mario Rossi"]
	if !ok {
		t.Fatal("expected a finding for Mario Rossi")
	}
	if person.Source != SourceModel {
		t.Errorf("source = %q, want %q", person.Source, SourceModel)
	}
	if person.Token != "[PERSON_1]" {
		t.Errorf("token = %q, want [PERSON_1]", person.Token)
	}
	if person.Occurrences != 2 {
		t.Errorf("occurrences = %d, want 2", person.Occurrences)
	}

	email, ok := doc.byLiteral["mario.rossi@example.com"]
	if !ok || email.Source != Source {
		t.Errorf("email finding = %+v, want it to stay source %q", email, Source)
	}
}

func TestApplyModelPassHallucinatedLiteralIsCountedAndDropped(t *testing.T) {
	doc := Analyze("Nothing sensitive is written here at all.")
	fc := &fakeCompleter{replies: []string{`[{"literal":"Someone Nobody","category":"person"}]`}}

	result, err := doc.ApplyModelPass(context.Background(), fc)
	if err != nil {
		t.Fatalf("ApplyModelPass: %v", err)
	}
	if result.Hallucinated != 1 {
		t.Errorf("hallucinated = %d, want 1", result.Hallucinated)
	}
	if len(doc.Findings) != 0 {
		t.Errorf("findings = %+v, want none", doc.Findings)
	}
}

func TestApplyModelPassDuplicateOfAnExistingFindingIsCounted(t *testing.T) {
	doc := Analyze("Reach out to mario.rossi@example.com for details.")
	fc := &fakeCompleter{replies: []string{`[{"literal":"mario.rossi@example.com","category":"person"}]`}}

	result, err := doc.ApplyModelPass(context.Background(), fc)
	if err != nil {
		t.Fatalf("ApplyModelPass: %v", err)
	}
	if result.Duplicates != 1 {
		t.Errorf("duplicates = %d, want 1", result.Duplicates)
	}
	email := doc.byLiteral["mario.rossi@example.com"]
	if email == nil || email.Source != Source {
		t.Errorf("finding = %+v, want it to keep source %q", email, Source)
	}
}

func TestApplyModelPassRepairsAGarbledReply(t *testing.T) {
	doc := Analyze("Anna Bianchi signed the form yesterday.")
	fc := &fakeCompleter{replies: []string{
		"sorry, I cannot help with that",
		`[{"literal":"Anna Bianchi","category":"person"}]`,
	}}

	result, err := doc.ApplyModelPass(context.Background(), fc)
	if err != nil {
		t.Fatalf("ApplyModelPass: %v", err)
	}
	if result.Accepted != 1 {
		t.Errorf("accepted = %d, want 1", result.Accepted)
	}
	if len(fc.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(fc.calls))
	}
	if !strings.Contains(fc.calls[1], "sorry, I cannot help with that") {
		t.Errorf("repair prompt = %q, want it to contain the previous reply", fc.calls[1])
	}
}

func TestApplyModelPassGivesUpAfterOneFailedRepair(t *testing.T) {
	doc := Analyze("Plain text with nothing to find.")
	fc := &fakeCompleter{replies: []string{"still not json", "still not json either"}}

	result, err := doc.ApplyModelPass(context.Background(), fc)
	if err != nil {
		t.Fatalf("ApplyModelPass: %v", err)
	}
	if result.ChunksFailed != 1 {
		t.Errorf("chunks failed = %d, want 1", result.ChunksFailed)
	}
	if !result.Degraded {
		t.Error("expected degraded when the only chunk failed")
	}
	if len(doc.Findings) != 0 {
		t.Errorf("findings = %+v, want unchanged", doc.Findings)
	}
}

func TestApplyModelPassFallsBackToUnstructuredForLaterChunks(t *testing.T) {
	padding := strings.Repeat("filler ", 1150) // > ModelChunkSize on its own
	text := padding + "Giulia Conti works here."
	doc := Analyze(text)

	chunks := ChunkText(doc.Text, ModelChunkSize, ModelChunkOverlap)
	if len(chunks) < 2 {
		t.Fatalf("expected at least two chunks, got %d", len(chunks))
	}

	replies := []string{"STRUCTURED-UNSUPPORTED", "[]"}
	for i := 1; i < len(chunks); i++ {
		if i == len(chunks)-1 {
			replies = append(replies, `[{"literal":"Giulia Conti","category":"person"}]`)
		} else {
			replies = append(replies, "[]")
		}
	}
	fc := &fakeCompleter{replies: replies}

	result, err := doc.ApplyModelPass(context.Background(), fc)
	if err != nil {
		t.Fatalf("ApplyModelPass: %v", err)
	}
	if result.ChunksFailed != 0 {
		t.Errorf("chunks failed = %d, want 0", result.ChunksFailed)
	}
	if result.Accepted != 1 {
		t.Errorf("accepted = %d, want 1", result.Accepted)
	}
	// One retry for the first chunk's structured attempt, then one call per
	// remaining chunk -- a chunk that already knows structured output is
	// unsupported must not pay for a doomed structured attempt again.
	wantCalls := len(chunks) + 1
	if len(fc.calls) != wantCalls {
		t.Errorf("calls = %d, want %d", len(fc.calls), wantCalls)
	}
}

func TestApplyModelPassLongerModelLiteralWinsOverAPatternMatch(t *testing.T) {
	doc := Analyze("Sig. Mario Rossi <mario@example.com> confirmed the order.")
	fc := &fakeCompleter{replies: []string{
		`[{"literal":"Sig. Mario Rossi <mario@example.com>","category":"person"}]`,
	}}

	result, err := doc.ApplyModelPass(context.Background(), fc)
	if err != nil {
		t.Fatalf("ApplyModelPass: %v", err)
	}
	if result.Accepted != 1 {
		t.Errorf("accepted = %d, want 1", result.Accepted)
	}
	if _, ok := doc.byLiteral["mario@example.com"]; ok {
		t.Error("the email finding should have lost the overlap to the longer model literal")
	}
	phrase, ok := doc.byLiteral["Sig. Mario Rossi <mario@example.com>"]
	if !ok || phrase.Source != SourceModel {
		t.Errorf("finding = %+v, want the longer model literal to win", phrase)
	}
}

func TestApplyModelPassUnknownCategoryBecomesPhrase(t *testing.T) {
	doc := Analyze("The consultant Priya Shah reviewed the contract.")
	fc := &fakeCompleter{replies: []string{`[{"literal":"Priya Shah","category":"consultant"}]`}}

	result, err := doc.ApplyModelPass(context.Background(), fc)
	if err != nil {
		t.Fatalf("ApplyModelPass: %v", err)
	}
	if result.Accepted != 1 {
		t.Errorf("accepted = %d, want 1", result.Accepted)
	}
	finding := doc.byLiteral["Priya Shah"]
	if finding == nil || finding.Category != CategoryPhrase {
		t.Errorf("finding = %+v, want category phrase", finding)
	}
}

func TestApplyModelPassCancelledContextLeavesDocumentUntouched(t *testing.T) {
	doc := Analyze("Mario Rossi signed the contract.")
	before := len(doc.Findings)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fc := &fakeCompleter{replies: []string{`[{"literal":"Mario Rossi","category":"person"}]`}}

	_, err := doc.ApplyModelPass(ctx, fc)
	if err == nil {
		t.Fatal("expected an error for an already-cancelled context")
	}
	if len(doc.Findings) != before {
		t.Errorf("findings changed after a cancelled context: %+v", doc.Findings)
	}
	if len(fc.calls) != 0 {
		t.Errorf("calls = %d, want 0 for an already-cancelled context", len(fc.calls))
	}
}
