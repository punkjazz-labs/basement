package recipe

import "testing"

func recipeAt(id string, version int) Recipe { return Recipe{ID: id, Version: version} }

func TestMergeEmbeddedIsTheFloor(t *testing.T) {
	embedded := []Recipe{recipeAt("a", 1), recipeAt("b", 1)}
	got := Merge(embedded, nil, nil)
	if len(got) != 2 || got[0].Version != 1 || got[1].Version != 1 {
		t.Fatalf("embedded-only merge changed: %#v", got)
	}
}

func TestMergeCacheOverlaysEmbeddedWhenNewer(t *testing.T) {
	embedded := []Recipe{recipeAt("a", 1)}
	cached := []Recipe{recipeAt("a", 2)}
	got := Merge(embedded, cached, nil)
	if len(got) != 1 || got[0].Version != 2 {
		t.Fatalf("cache did not overlay an older embedded recipe: %#v", got)
	}
}

func TestMergeFreshOverlaysCacheWhenNewer(t *testing.T) {
	embedded := []Recipe{recipeAt("a", 1)}
	cached := []Recipe{recipeAt("a", 2)}
	fresh := []Recipe{recipeAt("a", 3)}
	got := Merge(embedded, cached, fresh)
	if len(got) != 1 || got[0].Version != 3 {
		t.Fatalf("fresh did not overlay a newer cache: %#v", got)
	}
}

func TestMergeNeverDowngradesPerRecipe(t *testing.T) {
	// A stale or malformed fresh/cache entry reporting an OLDER version than
	// what is already recorded for that ID must never win — this is the
	// per-recipe half of downgrade protection (the other half lives in
	// recipefeed, comparing the whole index's generated_at).
	embedded := []Recipe{recipeAt("a", 5)}
	cached := []Recipe{recipeAt("a", 1)} // e.g. a cache file from long ago
	fresh := []Recipe{recipeAt("a", 3)}  // still older than embedded
	got := Merge(embedded, cached, fresh)
	if len(got) != 1 || got[0].Version != 5 {
		t.Fatalf("an older recipe version was allowed to shadow a newer one: %#v", got)
	}
}

func TestMergeEqualVersionIsAccepted(t *testing.T) {
	embedded := []Recipe{recipeAt("a", 1)}
	fresh := []Recipe{{ID: "a", Version: 1, DisplayName: "fresh copy"}}
	got := Merge(embedded, nil, fresh)
	if len(got) != 1 || got[0].DisplayName != "fresh copy" {
		t.Fatalf("equal-version fresh recipe should still be accepted (>=): %#v", got)
	}
}

func TestMergeAddsNewRecipeIDs(t *testing.T) {
	embedded := []Recipe{recipeAt("a", 1)}
	fresh := []Recipe{recipeAt("b", 1)}
	got := Merge(embedded, nil, fresh)
	if len(got) != 2 {
		t.Fatalf("new recipe id from a fresh fetch did not appear in the catalog: %#v", got)
	}
	if _, ok := Find(got, "a"); !ok {
		t.Fatal("embedded recipe a missing")
	}
	if _, ok := Find(got, "b"); !ok {
		t.Fatal("new remote recipe b missing")
	}
}

func TestMergeIsOrderStableAcrossLayers(t *testing.T) {
	embedded := []Recipe{recipeAt("a", 1), recipeAt("b", 1)}
	cached := []Recipe{recipeAt("c", 1)}
	fresh := []Recipe{recipeAt("b", 2), recipeAt("d", 1)}
	got := Merge(embedded, cached, fresh)
	ids := make([]string, len(got))
	for i, r := range got {
		ids[i] = r.ID
	}
	want := []string{"a", "b", "c", "d"}
	if len(ids) != len(want) {
		t.Fatalf("got ids %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("got ids %v, want %v", ids, want)
		}
	}
}

func TestFindVersionExactMatch(t *testing.T) {
	recipes := []Recipe{recipeAt("a", 1), recipeAt("a", 2), recipeAt("b", 1)}
	got, ok := FindVersion(recipes, "a", 1)
	if !ok || got.Version != 1 {
		t.Fatalf("FindVersion(a,1) = %#v, %v", got, ok)
	}
	got, ok = FindVersion(recipes, "a", 2)
	if !ok || got.Version != 2 {
		t.Fatalf("FindVersion(a,2) = %#v, %v", got, ok)
	}
	if _, ok := FindVersion(recipes, "a", 3); ok {
		t.Fatal("FindVersion matched a version that was not present")
	}
	if _, ok := FindVersion(recipes, "missing", 1); ok {
		t.Fatal("FindVersion matched an id that was not present")
	}
}
