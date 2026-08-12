package docredact

import "sort"

// ResolveOverlaps reduces a set of detector matches, which may come from
// several detectors and may overlap in the text, to a non-overlapping set
// where the longer literal always wins at any point of overlap.
//
// The algorithm: sort candidates longest-first (ties broken by their
// original order, for determinism when two matches share a length), then
// walk that order accepting a candidate only if its span does not
// intersect any span already accepted. Because longer candidates are
// always considered before shorter ones, a shorter match that overlaps an
// already-accepted longer match is always the one dropped -- never the
// reverse. The result is returned in document order (sorted by Start).
func ResolveOverlaps(matches []Match) []Match {
	ordered := make([]Match, len(matches))
	copy(ordered, matches)
	sort.SliceStable(ordered, func(i, j int) bool {
		li := ordered[i].End - ordered[i].Start
		lj := ordered[j].End - ordered[j].Start
		return li > lj
	})

	type span struct{ start, end int }
	var accepted []span
	var kept []Match
	for _, m := range ordered {
		overlaps := false
		for _, a := range accepted {
			if m.Start < a.end && a.start < m.End {
				overlaps = true
				break
			}
		}
		if overlaps {
			continue
		}
		accepted = append(accepted, span{m.Start, m.End})
		kept = append(kept, m)
	}

	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Start < kept[j].Start })
	return kept
}
