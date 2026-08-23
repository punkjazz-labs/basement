package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"gopkg.in/yaml.v3"
)

// Acknowledgement is one maintainer ruling on a drift feed-watch keeps
// finding: this exact upstream state was reviewed, a decision exists, and
// the daily report must not raise it as open work again. It matches one
// finding by recipe, kind, role and the upstream revision the ruling
// covered, so the moment upstream moves again the finding comes back as
// open. An acknowledgement never changes what bump mode may write; it only
// reclassifies a finding in the report and the exit code.
type Acknowledgement struct {
	Recipe   string `yaml:"recipe"`
	Kind     string `yaml:"kind"`
	Role     string `yaml:"role,omitempty"`
	Revision string `yaml:"revision"`
	Reason   string `yaml:"reason"`
}

// loadAcknowledgements reads the ruling list at path. A missing file means
// no rulings, which is the normal state for most checkouts; anything else
// that stops the file from loading is an error, because silently dropping
// rulings would re-open judged findings without anyone deciding that.
func loadAcknowledgements(path string) ([]Acknowledgement, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read acknowledgements %s: %w", path, err)
	}
	var acks []Acknowledgement
	if err := yaml.Unmarshal(data, &acks); err != nil {
		return nil, fmt.Errorf("decode acknowledgements %s: %w", path, err)
	}
	for index, ack := range acks {
		if ack.Recipe == "" || ack.Kind == "" || ack.Revision == "" || ack.Reason == "" {
			return nil, fmt.Errorf("acknowledgement %d in %s: recipe, kind, revision and reason are all required", index+1, path)
		}
	}
	return acks, nil
}

// partitionAcknowledged splits findings into the ones still open and the
// ones a ruling covers. Error findings are never acknowledgeable: a network
// failure today says nothing about the drift someone judged last month.
func partitionAcknowledged(findings []Finding, acks []Acknowledgement) (open, acknowledged []Finding) {
	for _, finding := range findings {
		if finding.Kind != "error" && ruled(finding, acks) {
			acknowledged = append(acknowledged, finding)
			continue
		}
		open = append(open, finding)
	}
	return open, acknowledged
}

func ruled(f Finding, acks []Acknowledgement) bool {
	for _, ack := range acks {
		if ack.Recipe == f.RecipeID && ack.Kind == f.Kind && ack.Role == f.Role && ack.Revision == f.Current {
			return true
		}
	}
	return false
}
