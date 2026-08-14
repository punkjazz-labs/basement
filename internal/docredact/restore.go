package docredact

import "regexp"

// tokenShape matches anything shaped like a pseudonym this package mints:
// an uppercase prefix, an underscore, a number, in brackets. Its console
// twin is the TOKEN regexp in webui/ui/src/docredact.ts, which the preview
// uses for the same purpose; the two must stay in lockstep with every
// Category.Prefix() this package declares, which
// TestTokenShapeMatchesEveryCategoryPrefix pins. A match is only restored
// when the mapping actually names it; everything else is left exactly where
// it stood and reported as unknown, because a pseudonym the mapping never
// minted is the cloud model inventing one.
var tokenShape = regexp.MustCompile(`\[[A-Z][A-Z0-9]*_\d+\]`)

// RestoreResult tallies what one Restore call did: how many pseudonym
// occurrences were swapped back, how many distinct pseudonyms that covered,
// and which pseudonym-shaped strings had no mapping entry, distinct and in
// first-appearance order.
type RestoreResult struct {
	Replaced int      `json:"replaced"`
	Tokens   int      `json:"tokens"`
	Unknown  []string `json:"unknown"`
}

// Restore swaps every mapped pseudonym in text back to its literal. It is
// a single pass: replacement output is never re-scanned, so a literal that
// itself contains pseudonym-shaped text survives verbatim, and restoration
// can never cascade. Neither text nor entries is mutated.
func Restore(text string, entries []MappingEntry) (string, RestoreResult) {
	byToken := make(map[string]string, len(entries))
	for _, entry := range entries {
		byToken[entry.Token] = entry.Literal
	}

	result := RestoreResult{Unknown: []string{}}
	seenTokens := make(map[string]bool)
	seenUnknown := make(map[string]bool)

	restored := tokenShape.ReplaceAllStringFunc(text, func(token string) string {
		literal, ok := byToken[token]
		if !ok {
			if !seenUnknown[token] {
				seenUnknown[token] = true
				result.Unknown = append(result.Unknown, token)
			}
			return token
		}
		result.Replaced++
		if !seenTokens[token] {
			seenTokens[token] = true
			result.Tokens++
		}
		return literal
	})
	return restored, result
}
