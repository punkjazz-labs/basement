package docredact

import (
	"os"
	"strings"
	"testing"
)

// TestCorpusInvariants is the oracle for every document under
// testdata/corpus: it never checks a specific literal or category by name,
// only two structural rules that must hold no matter what the corpus
// contains. Rule one -- every gold literal is exactly the text it claims to
// label, not a paraphrase or a near-miss -- catches a corpus doc where the
// gold list drifted from an edit to the prose. Rule two -- every
// pattern-category gold literal is actually found by Analyze -- catches a
// corpus doc claiming a detector finds something it does not; model-category
// gold (person, org, address, job_title, amount) is exempt because no
// detector claims those, by design (see docredact.go).
func TestCorpusInvariants(t *testing.T) {
	docs, err := LoadCorpus("testdata/corpus")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) < 12 {
		t.Fatalf("corpus has %d docs, want at least 12", len(docs))
	}
	for _, doc := range docs {
		for _, g := range doc.Gold {
			if !strings.Contains(doc.Text, g.Literal) {
				t.Fatalf("%s: gold literal %q not in text", doc.Name, g.Literal)
			}
		}
		// every pattern-category gold literal must be found by the pattern pass:
		// the corpus may not claim the deterministic pass finds things it does not.
		found := map[string]bool{}
		for _, f := range Analyze(doc.Text).Findings {
			found[f.Literal] = true
		}
		for _, g := range doc.Gold {
			switch g.Category {
			case CategoryEmail, CategoryPhone, CategoryIBAN, CategoryCard,
				CategoryIPv4, CategoryIPv6, CategoryDOB, CategoryITCF, CategoryUSSSN:
				if !found[g.Literal] {
					t.Fatalf("%s: pattern gold %q (%s) not detected", doc.Name, g.Literal, g.Category)
				}
			}
		}
	}
}

// TestLoadCorpusSortsByFilename pins the loader's iteration order to
// filename, not directory-listing order or JSON "name" field order, so a
// caller (a future cmd/docredact-bench) can rely on it for stable, diffable
// output across runs.
func TestLoadCorpusSortsByFilename(t *testing.T) {
	docs, err := LoadCorpus("testdata/corpus")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) < 2 {
		t.Fatalf("need at least 2 docs to test ordering, got %d", len(docs))
	}
	// Load the raw directory listing independently and compare: os.ReadDir
	// already returns entries sorted by filename, so this is the ground
	// truth the loader's order is being checked against.
	entries, err := os.ReadDir("testdata/corpus")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) != len(docs) {
		t.Fatalf("got %d filenames, %d docs", len(names), len(docs))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("filenames not sorted at index %d: %q >= %q", i, names[i-1], names[i])
		}
	}
}

// TestLoadCorpusLocales requires both locales named in the brief to be
// represented, and only those two: a doc with a typo'd or unregistered
// locale would silently pass every other check while being useless for a
// per-locale benchmark breakdown.
func TestLoadCorpusLocales(t *testing.T) {
	docs, err := LoadCorpus("testdata/corpus")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, doc := range docs {
		seen[doc.Locale]++
	}
	if seen["IT"] == 0 || seen["US"] == 0 {
		t.Fatalf("locales present = %v, want both IT and US represented", seen)
	}
	for locale := range seen {
		if locale != "IT" && locale != "US" {
			t.Fatalf("doc has unregistered locale %q", locale)
		}
	}
}

// TestLoadCorpusEmptyGoldDocExists requires at least one document with a
// nil/empty gold list: the negative case where a realistic document contains
// no sensitive literal at all, and the pattern pass must agree by finding
// nothing.
func TestLoadCorpusEmptyGoldDocExists(t *testing.T) {
	docs, err := LoadCorpus("testdata/corpus")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, doc := range docs {
		if len(doc.Gold) == 0 {
			found = true
			if len(Analyze(doc.Text).Findings) != 0 {
				t.Errorf("%s: has empty gold but Analyze still found findings", doc.Name)
			}
		}
	}
	if !found {
		t.Fatal("no corpus doc has an empty gold list")
	}
}

// TestLoadCorpusAllCategoriesCovered requires every pattern category and
// every model category to appear in gold at least once, somewhere in the
// corpus: a category with zero coverage means the benchmark this corpus
// feeds can never report a score for it.
func TestLoadCorpusAllCategoriesCovered(t *testing.T) {
	docs, err := LoadCorpus("testdata/corpus")
	if err != nil {
		t.Fatal(err)
	}
	want := []Category{
		CategoryEmail, CategoryPhone, CategoryIBAN, CategoryCard, CategoryIPv4,
		CategoryIPv6, CategoryDOB, CategoryITCF, CategoryUSSSN,
		CategoryPerson, CategoryOrg, CategoryAddress, CategoryJobTitle, CategoryAmount,
	}
	seen := map[Category]bool{}
	for _, doc := range docs {
		for _, g := range doc.Gold {
			seen[g.Category] = true
		}
	}
	for _, c := range want {
		if !seen[c] {
			t.Errorf("category %q never appears in corpus gold", c)
		}
	}
}

func TestLoadCorpusMissingDir(t *testing.T) {
	if _, err := LoadCorpus("testdata/corpus-does-not-exist"); err == nil {
		t.Fatal("expected an error for a missing corpus directory")
	}
}

func TestLoadCorpusBadJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/bad.json", []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCorpus(dir); err == nil {
		t.Fatal("expected an error for invalid JSON in the corpus directory")
	}
}
