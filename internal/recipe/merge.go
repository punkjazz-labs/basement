package recipe

// Merge computes the effective catalog: embedded is the permanent offline
// floor, cache overlays it, and a fresh fetch overlays cache. This is the
// one function that owns that rule (spec 04) — nothing else in the program
// decides which of two same-ID recipes wins. At each overlay step, a
// recipe replaces the one already recorded for its ID only when its Version
// is greater than or equal to the recorded one, so a same-version resend is
// harmless and an older or malformed remote entry can never shadow a newer
// embedded or cached one. Recipe IDs absent from earlier layers are simply
// added, keeping their first-seen order; ordering elsewhere is not
// meaningful and callers must not depend on it beyond this stability.
func Merge(embedded, cached, fresh []Recipe) []Recipe {
	order := make([]string, 0, len(embedded))
	byID := make(map[string]Recipe, len(embedded))
	apply := func(layer []Recipe) {
		for _, r := range layer {
			existing, ok := byID[r.ID]
			if !ok {
				order = append(order, r.ID)
				byID[r.ID] = r
				continue
			}
			if r.Version >= existing.Version {
				byID[r.ID] = r
			}
		}
	}
	apply(embedded)
	apply(cached)
	apply(fresh)
	result := make([]Recipe, 0, len(order))
	for _, id := range order {
		result = append(result, byID[id])
	}
	return result
}

// FindVersion looks up a recipe by the exact (id, version) pair an
// InstalledModel row was recorded with. Effective/merged catalogs hold only
// the newest version per ID, which is right for deciding what a fresh
// install or an update targets, but wrong for operating on a model that is
// already installed: its container name, port, and config were built from
// the recipe as it existed at install time, not whatever the catalog has
// moved on to since. Callers operating on an existing InstalledModel must
// use this instead of Find, against a recipe set that retains history
// (see recipefeed.Fetcher.All), so a background recipe update can never
// change what an already-running model resolves to underneath it.
func FindVersion(recipes []Recipe, id string, version int) (Recipe, bool) {
	for _, r := range recipes {
		if r.ID == id && r.Version == version {
			return r, true
		}
	}
	return Recipe{}, false
}
