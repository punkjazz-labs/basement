package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcknowledgementsCoverExactRevisionsOnly(t *testing.T) {
	acks := []Acknowledgement{
		{Recipe: "r1", Kind: "source", Revision: "aaa", Reason: "judged"},
		{Recipe: "r2", Kind: "artifact", Role: "drafter", Revision: "bbb", Reason: "judged"},
	}
	findings := []Finding{
		{RecipeID: "r1", Kind: "source", Current: "aaa"},
		{RecipeID: "r1", Kind: "source", Current: "moved-again"},
		{RecipeID: "r2", Kind: "artifact", Role: "drafter", Current: "bbb"},
		{RecipeID: "r2", Kind: "artifact", Role: "primary", Current: "bbb"},
		{RecipeID: "r1", Kind: "error", Current: "aaa", Details: "network"},
	}
	open, acknowledged := partitionAcknowledged(findings, acks)
	if len(acknowledged) != 2 {
		t.Fatalf("acknowledged=%#v, want the two exact matches", acknowledged)
	}
	if len(open) != 3 {
		t.Fatalf("open=%#v, want the moved-again drift, the other role, and the error", open)
	}
	if exitCode(open) != 3 {
		t.Fatalf("open findings must still drive exit 3")
	}
	if exitCode(nil) != 0 {
		t.Fatalf("no open findings must exit 0 even when rulings exist")
	}
}

func TestLoadAcknowledgementsMissingFileMeansNone(t *testing.T) {
	acks, err := loadAcknowledgements(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil || acks != nil {
		t.Fatalf("got %#v, %v; a missing file is the normal empty state", acks, err)
	}
}

func TestLoadAcknowledgementsRejectsIncompleteRulings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acks.yaml")
	if err := os.WriteFile(path, []byte("- recipe: r1\n  kind: source\n  revision: aaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAcknowledgements(path); err == nil {
		t.Fatal("a ruling without a reason must not load")
	}
}

func TestShippedAcknowledgementsFileLoads(t *testing.T) {
	acks, err := loadAcknowledgements("../../docs/feed-acknowledged.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) == 0 {
		t.Fatal("the shipped rulings file must decode to at least one entry")
	}
}
