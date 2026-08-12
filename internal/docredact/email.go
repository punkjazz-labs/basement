package docredact

import "regexp"

// emailPattern is deliberately not RFC 5322: it matches the shape a human
// would call an email address in a document, which is the shape worth
// redacting. Exotic-but-legal addresses that fall outside it are a false
// negative, not a false positive, and that is the safer direction to err in
// for a detector whose matches get flagged, not silently trusted.
var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

type EmailDetector struct{}

func (EmailDetector) Name() string { return "email" }

func (EmailDetector) Detect(text string) []Match {
	return matchesFromRegexp(text, emailPattern, CategoryEmail, nil)
}

// matchesFromRegexp is the shared plumbing every simple regexp-only detector
// uses: find every non-overlapping match, and optionally reject it with a
// validator before it becomes a Match. Detectors that need to inspect
// capture groups (dates, national identifiers) do not use this helper.
func matchesFromRegexp(text string, re *regexp.Regexp, category Category, valid func(string) bool) []Match {
	var out []Match
	for _, loc := range re.FindAllStringIndex(text, -1) {
		literal := text[loc[0]:loc[1]]
		if valid != nil && !valid(literal) {
			continue
		}
		out = append(out, Match{
			Start:    loc[0],
			End:      loc[1],
			Text:     literal,
			Category: category,
			Source:   Source,
		})
	}
	return out
}
