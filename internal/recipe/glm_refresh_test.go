package recipe

import (
	"reflect"
	"testing"
)

// An already-serving install must keep resolving its exact container recipe
// after a manager refresh, while a requested install picks the new image.
func TestGLMRefreshRetainsInstalledVersionsAndSelectsNewRuntime(t *testing.T) {
	all, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	current := Merge(all, nil, nil)
	for _, tc := range []struct {
		id       string
		previous int
	}{
		{"glm53-flash-exl3-2s", 2},
		{"glm53-flash-exl3-ablit-2s", 1},
	} {
		t.Run(tc.id, func(t *testing.T) {
			old, ok := FindVersion(all, tc.id, tc.previous)
			if !ok {
				t.Fatal("installed recipe lost; rollback cannot resolve it")
			}
			next, ok := Find(current, tc.id)
			if !ok || next.Version != tc.previous+1 {
				t.Fatal("catalog does not select refreshed recipe")
			}
			if old.Runtime.Digest != "sha256:5c7a0f538f7aa05647ae0c97bc5333330c6441043f7e00defa14fc2cff6ee343" || next.Runtime.Digest == old.Runtime.Digest {
				t.Fatal("old and new runtime identities are not distinct")
			}
			if next.Source.Revision != "6c228969a81d48372173bee55e5ad1b4753e656e" {
				t.Fatal("refresh is not pinned to the tested Mia source")
			}
			// This update changes runtime code only. Artifact identities (including
			// the donor ranges), ablation preference, serving settings and topology
			// must survive the version change unchanged.
			normalized := next
			normalized.Version = old.Version
			normalized.Source = old.Source
			normalized.Runtime.Digest = old.Runtime.Digest
			normalized.Runtime.ImageBytes = old.Runtime.ImageBytes
			normalized.Runtime.ImageDiskBytes = old.Runtime.ImageDiskBytes
			if !reflect.DeepEqual(normalized, old) {
				t.Fatal("refresh unexpectedly changes settings or artifacts")
			}
		})
	}
}
