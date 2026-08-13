package docredact

import (
	"context"
	"testing"
)

// TestScoreDocument builds one hand-labeled document covering every rule
// ScoreDocument has to get right in a single pass: a pattern-caught literal,
// a model-caught literal, a literal nobody catches (the leak that matters),
// and a manual over-redaction that touches no gold text at all.
func TestScoreDocument(t *testing.T) {
	doc := Analyze("Jane Doe, reachable at jane@example.com. John Smith stopped by. The office park was quiet today.")

	// The fake model claims Jane Doe (real, verified against the text) and
	// Someone Fake (not in the text, so ApplyModelPass counts it
	// Hallucinated and drops it) -- the same shape a real model reply takes.
	fc := &fakeCompleter{replies: []string{`[{"literal":"Jane Doe","category":"person"},{"literal":"Someone Fake","category":"person"}]`}}
	pass, err := doc.ApplyModelPass(context.Background(), fc)
	if err != nil {
		t.Fatalf("ApplyModelPass: %v", err)
	}
	if pass.Hallucinated != 1 {
		t.Fatalf("pass.Hallucinated = %d, want 1", pass.Hallucinated)
	}

	// An innocuous manual addition that overlaps no gold literal at all --
	// this is the over-redaction ScoreDocument must count.
	if _, err := doc.AddManual("office park", CategoryPhrase); err != nil {
		t.Fatalf("AddManual: %v", err)
	}

	gold := []GoldItem{
		{Literal: "jane@example.com", Category: CategoryEmail}, // pattern catches this
		{Literal: "Jane Doe", Category: CategoryPerson},        // model catches this
		{Literal: "John Smith", Category: CategoryPerson},      // nobody catches this: a leak
	}

	score := ScoreDocument(doc, gold, pass)

	if score.Gold != 3 {
		t.Errorf("Gold = %d, want 3", score.Gold)
	}
	if score.Leaked != 1 {
		t.Errorf("Leaked = %d, want 1", score.Leaked)
	}
	if got := score.LeakedByCat[CategoryPerson]; got != 1 {
		t.Errorf("LeakedByCat[person] = %d, want 1", got)
	}
	if got := score.LeakedByCat[CategoryEmail]; got != 0 {
		t.Errorf("LeakedByCat[email] = %d, want 0", got)
	}
	if len(score.LeakedByCat) != 1 {
		t.Errorf("LeakedByCat = %+v, want only person", score.LeakedByCat)
	}
	if got := score.GoldByCat[CategoryPerson]; got != 2 {
		t.Errorf("GoldByCat[person] = %d, want 2", got)
	}
	if got := score.GoldByCat[CategoryEmail]; got != 1 {
		t.Errorf("GoldByCat[email] = %d, want 1", got)
	}
	if len(score.GoldByCat) != 2 {
		t.Errorf("GoldByCat = %+v, want person and email only", score.GoldByCat)
	}
	if score.OverRedacted != 1 {
		t.Errorf("OverRedacted = %d, want 1", score.OverRedacted)
	}
	if score.Hallucinated != 1 {
		t.Errorf("Hallucinated = %d, want 1 (copied from the ModelPassResult)", score.Hallucinated)
	}
}

// TestScoreDocumentPatternOnlyLeaksEveryModelCategoryLiteral is the
// pattern-only shape the bench command's baseline arm actually runs: no
// ApplyModelPass call, a zero ModelPassResult, so every gold literal a
// pattern detector cannot find is a leak and Hallucinated stays 0.
func TestScoreDocumentPatternOnlyLeaksEveryModelCategoryLiteral(t *testing.T) {
	doc := Analyze("Mario Rossi works at Acme Corp and can be reached at mario@example.com.")
	gold := []GoldItem{
		{Literal: "mario@example.com", Category: CategoryEmail},
		{Literal: "Mario Rossi", Category: CategoryPerson},
		{Literal: "Acme Corp", Category: CategoryOrg},
	}

	score := ScoreDocument(doc, gold, ModelPassResult{})

	if score.Gold != 3 {
		t.Errorf("Gold = %d, want 3", score.Gold)
	}
	if score.Leaked != 2 {
		t.Errorf("Leaked = %d, want 2 (person and org, neither has a pattern detector)", score.Leaked)
	}
	if got := score.LeakedByCat[CategoryEmail]; got != 0 {
		t.Errorf("LeakedByCat[email] = %d, want 0: the pattern detector must catch this", got)
	}
	if got := score.LeakedByCat[CategoryPerson]; got != 1 {
		t.Errorf("LeakedByCat[person] = %d, want 1", got)
	}
	if got := score.LeakedByCat[CategoryOrg]; got != 1 {
		t.Errorf("LeakedByCat[org] = %d, want 1", got)
	}
	if score.OverRedacted != 0 {
		t.Errorf("OverRedacted = %d, want 0: only a pattern detector ran, nothing was added by hand", score.OverRedacted)
	}
	if score.Hallucinated != 0 {
		t.Errorf("Hallucinated = %d, want 0 for a zero ModelPassResult", score.Hallucinated)
	}
}

// TestScoreDocumentLongerFindingCoveringGoldSpanIsNotOverRedacted checks that
// a gold literal whose every occurrence sat inside a longer accepted finding
// is not a leak (the span check in ScoreDocument reads Redacted(), and the
// token replaces the whole span), and that the longer finding itself is not
// an over-redaction because its span does intersect the gold occurrence.
func TestScoreDocumentLongerFindingCoveringGoldSpanIsNotOverRedacted(t *testing.T) {
	doc := Analyze("Reach out to mario.rossi@example.com for details.")
	if _, err := doc.AddManual("to mario.rossi@example.com for", CategoryPhrase); err != nil {
		t.Fatalf("AddManual: %v", err)
	}

	gold := []GoldItem{
		{Literal: "mario.rossi@example.com", Category: CategoryEmail},
	}
	score := ScoreDocument(doc, gold, ModelPassResult{})

	if score.Leaked != 0 {
		t.Errorf("Leaked = %d, want 0: the email is inside the longer accepted phrase", score.Leaked)
	}
	if score.OverRedacted != 0 {
		t.Errorf("OverRedacted = %d, want 0: the phrase's span intersects the gold email's span", score.OverRedacted)
	}
}
