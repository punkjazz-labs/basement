package docredact

import "strings"

// Score measures outcomes, not span bookkeeping: a gold literal still visible
// in the redacted output is a leak, whatever the passes did internally.
type Score struct {
	Gold         int              `json:"gold"`
	Leaked       int              `json:"leaked"`
	LeakedByCat  map[Category]int `json:"leaked_by_category"`
	GoldByCat    map[Category]int `json:"gold_by_category"`
	OverRedacted int              `json:"over_redacted"` // enabled findings overlapping no gold occurrence
	Hallucinated int              `json:"hallucinated"`  // from ModelPassResult, 0 for pattern-only
}

// ScoreDocument compares doc's redacted output against gold: every gold
// literal that a passing look at Redacted() would still turn up is a leak,
// and every enabled finding whose spans never touch a gold occurrence is an
// over-redaction. pass only supplies Hallucinated, copied through unchanged
// -- the pattern-only arm passes a zero ModelPassResult, so Hallucinated is
// honestly 0 rather than an estimate.
func ScoreDocument(doc *Document, gold []GoldItem, pass ModelPassResult) Score {
	score := Score{
		Gold:         len(gold),
		LeakedByCat:  make(map[Category]int),
		GoldByCat:    make(map[Category]int),
		Hallucinated: pass.Hallucinated,
	}

	redacted := doc.Redacted()
	var goldSpans []Match
	for _, g := range gold {
		score.GoldByCat[g.Category]++
		if strings.Contains(redacted, g.Literal) {
			score.Leaked++
			score.LeakedByCat[g.Category]++
		}
		// Same exact-search loop as manualMatches, so a gold literal's
		// occurrence spans line up with how AddManual would have located it.
		goldSpans = append(goldSpans, literalMatches(doc.Text, g.Literal, g.Category, "gold")...)
	}

	for _, f := range doc.Findings {
		if !f.Enabled {
			continue
		}
		overlapsGold := false
		for _, span := range doc.spans {
			if span.Text != f.Literal {
				continue
			}
			for _, g := range goldSpans {
				if span.Start < g.End && g.Start < span.End {
					overlapsGold = true
					break
				}
			}
			if overlapsGold {
				break
			}
		}
		if !overlapsGold {
			score.OverRedacted++
		}
	}

	return score
}
