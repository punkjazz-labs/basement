package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecordedRevocationIsReadableByExactVersion(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	revokedAt := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	if err := s.RecordRevocation(ctx, "recipe-a", 2, "the published weights were the wrong quantisation", revokedAt); err != nil {
		t.Fatal(err)
	}
	entry, revoked, err := s.Revocation(ctx, "recipe-a", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("the recorded version is not reported as revoked")
	}
	if entry.Reason != "the published weights were the wrong quantisation" {
		t.Fatalf("reason was not stored verbatim: %q", entry.Reason)
	}
	if entry.RevokedAt != "2026-08-12T09:00:00Z" {
		t.Fatalf("revoked_at=%q, want RFC3339 as issued", entry.RevokedAt)
	}
	if entry.AcceptedAt == "" {
		t.Fatal("accepted_at must record when this machine learned of the revocation")
	}
	// Revocation names one version. Another version of the same recipe, and
	// another recipe entirely, are untouched questions with their own answers.
	if _, revoked, err := s.Revocation(ctx, "recipe-a", 3); err != nil || revoked {
		t.Fatalf("a different version of the same recipe was reported revoked (err=%v)", err)
	}
	if _, revoked, err := s.Revocation(ctx, "recipe-b", 2); err != nil || revoked {
		t.Fatalf("an unrelated recipe was reported revoked (err=%v)", err)
	}
}

func TestRevocationSurvivesRestartAndLaterSilence(t *testing.T) {
	// Permanence: once accepted, a revocation stays on this machine even
	// though every later index says nothing about it, and even across a
	// restart. There is no un-revoke path to exercise, because there is none
	// to write.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.RecordRevocation(ctx, "recipe-a", 2, "the runtime image was compromised", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	entry, revoked, err := reopened.Revocation(ctx, "recipe-a", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked || entry.Reason != "the runtime image was compromised" {
		t.Fatalf("a revocation did not survive a restart: revoked=%v entry=%#v", revoked, entry)
	}
}

func TestRerecordingARevocationNeverRewritesTheOwnersExplanation(t *testing.T) {
	// A later index repeating the entry cannot change what the owner was
	// already told, any more than it can withdraw the revocation.
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.RecordRevocation(ctx, "recipe-a", 2, "the licence was withdrawn", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRevocation(ctx, "recipe-a", 2, "never mind, it is fine", time.Now()); err != nil {
		t.Fatal(err)
	}
	entry, revoked, err := s.Revocation(ctx, "recipe-a", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked || entry.Reason != "the licence was withdrawn" {
		t.Fatalf("the first accepted reason must stand, got: %#v", entry)
	}
	entries, err := s.Revocations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("re-recording duplicated the revocation: %#v", entries)
	}
}

func TestRevocationRefusesEntriesItCannotHonour(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	cases := []struct {
		name      string
		id        string
		version   int
		reason    string
		revokedAt time.Time
	}{
		{"no id", "", 1, "licence problem", time.Now()},
		{"no version", "recipe-a", 0, "licence problem", time.Now()},
		{"no reason", "recipe-a", 1, "  ", time.Now()},
		{"no revoked_at", "recipe-a", 1, "licence problem", time.Time{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := s.RecordRevocation(ctx, testCase.id, testCase.version, testCase.reason, testCase.revokedAt); err == nil {
				t.Fatal("an unhonourable revocation was recorded")
			}
		})
	}
	entries, err := s.Revocations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected nothing recorded, got: %#v", entries)
	}
}

func TestRevocationsTableIsCreatedByItsOwnMigration(t *testing.T) {
	// The fleet_upgrade_nodes episode (2026-08-12) was an amended migration a
	// shipped database skipped. This table arrives with its own schema
	// version instead, so a database that stopped at the previous one still
	// gains it on open.
	s := openTestStore(t)
	var version int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_meta`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	// Later migrations move the head past this one. What this test means is
	// that the revocation step itself ran, so it asks whether the database got
	// at least that far.
	if version < revocationSchemaVersion {
		t.Fatalf("schema version = %d, want at least %d", version, revocationSchemaVersion)
	}
	var name string
	if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='recipe_revocations'`).Scan(&name); err != nil {
		t.Fatalf("recipe_revocations was not created: %v", err)
	}
}

func TestRevocationsMigrateADatabaseThatStoppedAtTheFleetUpgradeVersion(t *testing.T) {
	// The database a shipped release left behind records schema version 4 and
	// has no revocation table. Opening it must add one without disturbing the
	// rows already there.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	shipped, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	installed := InstalledModel{RecipeID: "recipe-a", RecipeVersion: 2, Status: "ready", ArtifactPath: "/managed/recipe-a", Active: true}
	if err := shipped.ActivateExclusively(ctx, installed); err != nil {
		t.Fatal(err)
	}
	// Wind the database back to exactly what the previous release left: every
	// table it created, its recorded schema version, and no revocations.
	for _, statement := range []string{`DROP TABLE recipe_revocations`, `UPDATE schema_meta SET version=4`} {
		if _, err := shipped.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := shipped.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a shipped database did not open: %v", err)
	}
	defer s.Close()
	if err := s.RecordRevocation(ctx, "recipe-a", 2, "wrong weights", time.Now()); err != nil {
		t.Fatalf("the migrated database must accept revocations: %v", err)
	}
	models, err := s.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || !models[0].Active {
		t.Fatalf("the migration disturbed the installed model: %#v", models)
	}
}
