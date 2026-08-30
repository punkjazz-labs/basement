package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestDelegatedRankLivenessMigratesADatabaseAtVersionSix(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	shipped, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE delegated_rank_liveness`,
		`UPDATE schema_meta SET version=6`,
	} {
		if _, err := shipped.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := shipped.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("a version-six database did not open: %v", err)
	}
	defer migrated.Close()
	if err := migrated.SetDelegatedRankRunning(ctx, "reservation-1", true); err != nil {
		t.Fatalf("the migrated database did not accept worker liveness: %v", err)
	}
	running, known, err := migrated.DelegatedRankRunning(ctx, "reservation-1")
	if err != nil || !known || !running {
		t.Fatalf("delegated liveness running=%v known=%v err=%v", running, known, err)
	}
	var version int
	if err := migrated.db.QueryRow(`SELECT MAX(version) FROM schema_meta`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != delegatedRankSchemaVersion {
		t.Fatalf("schema version=%d, want %d", version, delegatedRankSchemaVersion)
	}
}
