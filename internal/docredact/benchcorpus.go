package docredact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// CorpusDoc is one labeled benchmark document: realistic prose plus the
// ground truth of every sensitive literal it contains. Gold covers both
// pattern-category literals (what Analyze is expected to find) and
// model-category literals (person, org, address, job_title, amount --
// ground truth for the future model pass, spec step 3, which Analyze is not
// expected to find on its own).
type CorpusDoc struct {
	Name   string     `json:"name"`
	Locale string     `json:"locale"` // "IT" or "US"
	Text   string     `json:"text"`
	Gold   []GoldItem `json:"gold"`
}

// GoldItem is one labeled literal within a CorpusDoc: the exact substring of
// Text it labels, and the category a correct redactor should assign it.
type GoldItem struct {
	Literal  string   `json:"literal"`
	Category Category `json:"category"`
}

// LoadCorpus reads every *.json file directly inside dir as a CorpusDoc, in
// filename order -- not directory-listing order on platforms where that
// differs, and not JSON field order -- so a caller (a benchmark command)
// gets the same document sequence on every run regardless of OS or how the
// files were last touched.
func LoadCorpus(dir string) ([]CorpusDoc, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read corpus dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	docs := make([]CorpusDoc, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read corpus doc %s: %w", path, err)
		}
		var doc CorpusDoc
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse corpus doc %s: %w", path, err)
		}
		docs = append(docs, doc)
	}
	return docs, nil
}
