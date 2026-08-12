package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

var fleetTables = []string{
	"fleet_deployment_nodes", "node_reservations", "distributed_ranks", "fleet_deployments",
	"fleet_pending_joins", "fleet_join_codes", "fleet_nodes", "fleet_config", "node_identity",
}

func TestExistingSingleNodeDatabaseOpensUnchangedAfterFleetSchemaAddition(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`INSERT INTO jobs(id,kind,recipe_id,idempotency_key,state,payload,error,created_at,updated_at) VALUES('job_old','install','recipe-one','old-click','ready','{}','','2026-08-01T10:00:00Z','2026-08-01T10:01:00Z')`,
		`INSERT INTO job_steps(job_id,step_index,operation,state,receipt,completed_at) VALUES('job_old',0,'verify_disk','completed','{"ok":true}','2026-08-01T10:00:30Z')`,
		`INSERT INTO installed_models(recipe_id,recipe_version,status,artifact_path,active,updated_at) VALUES('recipe-one',1,'stopped','/managed/recipe-one',0,'2026-08-01T10:01:00Z')`,
		`INSERT INTO accepted_licences(recipe_id,recipe_version,accepted_at) VALUES('recipe-one',1,'2026-08-01T09:00:00Z')`,
		`INSERT INTO territory_confirmations(recipe_id,recipe_version,confirmed_at) VALUES('recipe-one',1,'2026-08-01T09:01:00Z')`,
		`INSERT INTO api_keys(id,name,token_hash,created_at) VALUES('key_old','existing key','hash-old','2026-08-01T09:02:00Z')`,
		`INSERT INTO model_metrics(recipe_id,tokens_per_second,time_to_first_token_ms,measured_at) VALUES('recipe-one',12.5,80,'2026-08-01T09:03:00Z')`,
		`INSERT INTO roles(name,recipe_id,created_at,updated_at) VALUES('fast','recipe-one','2026-08-01T09:04:00Z','2026-08-01T09:04:00Z')`,
	}
	for _, statement := range statements {
		if _, err := database.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	tables := []string{"jobs", "job_steps", "installed_models", "accepted_licences", "territory_confirmations", "api_keys", "model_metrics", "roles"}
	before := snapshotTables(t, database.db, tables)
	makeDatabasePreFleet(t, database)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(path)
	if err != nil {
		t.Fatalf("pre-fleet database did not open: %v", err)
	}
	defer database.Close()
	after := snapshotTables(t, database.db, tables)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("legacy rows changed during additive migration\nbefore=%#v\nafter=%#v", before, after)
	}
	if _, created, err := database.CreateJob(ctx, "install", "recipe-two", "new-click", map[string]bool{"confirmed": true}); err != nil || !created {
		t.Fatalf("existing job API stopped working after migration: created=%v err=%v", created, err)
	}
	var version int
	if err := database.db.QueryRow(`SELECT version FROM schema_meta`).Scan(&version); err != nil || version != fleetSchemaVersion {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
}

func TestFleetSchemaMigrationRollsBackEveryStatement(t *testing.T) {
	previous := fleetMigrationStep
	t.Cleanup(func() { fleetMigrationStep = previous })
	for failedStep := 1; failedStep <= fleetMigrationStatementCount; failedStep++ {
		t.Run(fmt.Sprintf("statement_%d", failedStep), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manager.db")
			database, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			makeDatabasePreFleet(t, database)
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			fleetMigrationStep = func(step int) error {
				if step == failedStep {
					return errors.New("injected migration failure")
				}
				return nil
			}
			if failed, err := Open(path); err == nil {
				failed.Close()
				t.Fatal("migration succeeded despite injected failure")
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			var version int
			if err := raw.QueryRow(`SELECT version FROM schema_meta`).Scan(&version); err != nil || version != 1 {
				t.Fatalf("failed migration advanced schema version: version=%d err=%v", version, err)
			}
			var created int
			if err := raw.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='node_identity'`).Scan(&created); err != nil || created != 0 {
				t.Fatalf("failed migration left a fleet table: count=%d err=%v", created, err)
			}
			raw.Close()
			fleetMigrationStep = previous
			reopened, err := Open(path)
			if err != nil {
				t.Fatalf("database did not reopen after rollback: %v", err)
			}
			reopened.Close()
		})
	}
}

func TestLegacyPeerBecomesPendingWithoutChangingItsCredential(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.EnsureNodeIdentity(ctx, testNodeIdentity("node_00000000000000000000000000000001")); err != nil {
		t.Fatal(err)
	}
	peer, err := database.CreatePeer(ctx, "spark-worker", "http://192.168.99.20:7070", "legacy-secret")
	if err != nil {
		t.Fatal(err)
	}
	self := testFleetNode("node_00000000000000000000000000000001", "https://192.168.99.10:7071")
	candidate := LegacyDeploymentCandidate{RecipeID: "distributed-model", RecipeVersion: 2, RecipeFingerprint: "recipe-fingerprint", TopologyCount: 2}
	if err := database.InitializeFleetMigration(ctx, self, []LegacyDeploymentCandidate{candidate}); err != nil {
		t.Fatal(err)
	}
	config, err := database.FleetConfig(ctx)
	if err != nil || config.Role != "controller" || config.MigrationState != "legacy-pending" {
		t.Fatalf("unexpected fleet config: %+v err=%v", config, err)
	}
	nodes, err := database.FleetNodes(ctx)
	legacyFound := false
	for _, node := range nodes {
		legacyFound = legacyFound || node.LegacyPeerID == peer.ID && node.MembershipState == "legacy-pending"
	}
	if err != nil || len(nodes) != 2 || !legacyFound {
		t.Fatalf("unexpected migrated nodes: %+v err=%v", nodes, err)
	}
	_, credential, err := database.PeerCredentials(ctx, peer.ID)
	if err != nil || credential != "legacy-secret" {
		t.Fatalf("legacy credential changed: credential=%q err=%v", credential, err)
	}
	var state string
	var topology int
	if err := database.db.QueryRow(`SELECT state,topology_count FROM fleet_deployments WHERE recipe_id=?`, candidate.RecipeID).Scan(&state, &topology); err != nil || state != "legacy-candidate" || topology != 2 {
		t.Fatalf("legacy deployment was asserted beyond available evidence: state=%q topology=%d err=%v", state, topology, err)
	}
	var ranks int
	if err := database.db.QueryRow(`SELECT count(*) FROM fleet_deployment_nodes`).Scan(&ranks); err != nil || ranks != 2 {
		t.Fatalf("legacy candidate node rows=%d err=%v", ranks, err)
	}
}

func TestMultipleLegacyPeersArePreservedAndRequireRepair(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`DROP INDEX peers_singleton`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO peers(id,name,base_url,api_key,created_at,singleton) VALUES
('peer_one','spark-worker','http://192.168.99.20:7070','key-one','2026-08-01T00:00:00Z',1),
('peer_two','edgexpert-alpha','http://192.168.99.21:7070','key-two','2026-08-01T00:01:00Z',1)`); err != nil {
		t.Fatal(err)
	}
	if err := database.EnsureNodeIdentity(ctx, testNodeIdentity("node_00000000000000000000000000000001")); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeFleetMigration(ctx, testFleetNode("node_00000000000000000000000000000001", "https://192.168.99.10:7071"), nil); err != nil {
		t.Fatal(err)
	}
	config, _ := database.FleetConfig(ctx)
	peers, _ := database.Peers(ctx)
	if config.Role != "standalone" || config.FleetID != "" || config.MigrationState != "repair-required" || len(peers) != 2 {
		t.Fatalf("broken legacy state was not preserved for repair: config=%+v peers=%+v", config, peers)
	}
}

// Adopting the legacy peer is the only moment that can settle the migration
// marker, because every other path that writes it is unreachable once this
// controller already has a fleet id.
func TestAdoptingTheLegacyPeerAbsorbsItsRowAndSettlesMigrationState(t *testing.T) {
	ctx := context.Background()
	database, peer, self := legacyPendingController(t)
	adopted := testFleetNode("node_00000000000000000000000000000002", "https://192.168.99.20:7071")
	adopted.ConsoleURL = peer.BaseURL
	fleetID, idempotent, err := database.PrepareFleetNode(ctx, self, adopted)
	if err != nil || idempotent || fleetID != self.FleetID {
		t.Fatalf("prepare fleet node: fleet=%q idempotent=%v err=%v", fleetID, idempotent, err)
	}
	nodes, err := database.FleetNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("the legacy placeholder was not absorbed: %+v", nodes)
	}
	merged := findFleetNode(t, nodes, adopted.NodeID)
	if merged.LegacyPeerID != peer.ID || merged.MembershipState != "adopting" {
		t.Fatalf("the adopted node did not carry the legacy peer forward: %+v", merged)
	}
	for _, node := range nodes {
		if node.MembershipState == "legacy-pending" {
			t.Fatalf("the legacy placeholder row survived the merge: %+v", node)
		}
	}
	config, err := database.FleetConfig(ctx)
	if err != nil || config.MigrationState != "ready" {
		t.Fatalf("migration state after the merge=%q err=%v", config.MigrationState, err)
	}
	// The old peer row and credential stay untouched on purpose: they are the
	// rollback path and still serve the existing two-node executor.
	_, credential, err := database.PeerCredentials(ctx, peer.ID)
	if err != nil || credential != "legacy-secret" {
		t.Fatalf("the legacy peer row changed: credential=%q err=%v", credential, err)
	}
}

// The merge matches the console URL as an exact string. The same machine
// reached by another spelling is a different address to this code, so the
// placeholder and the marker must both survive untouched.
func TestAdoptingADifferentConsoleURLKeepsTheLegacyPlaceholder(t *testing.T) {
	ctx := context.Background()
	database, peer, self := legacyPendingController(t)
	adopted := testFleetNode("node_00000000000000000000000000000002", "https://192.168.99.20:7071")
	adopted.ConsoleURL = "http://spark-worker.local:7070"
	if _, _, err := database.PrepareFleetNode(ctx, self, adopted); err != nil {
		t.Fatal(err)
	}
	nodes, err := database.FleetNodes(ctx)
	if err != nil || len(nodes) != 3 {
		t.Fatalf("unexpected nodes after a non-matching console URL: %+v err=%v", nodes, err)
	}
	if merged := findFleetNode(t, nodes, adopted.NodeID); merged.LegacyPeerID != "" {
		t.Fatalf("a different console URL claimed the legacy peer: %+v", merged)
	}
	legacy := findFleetNode(t, nodes, "legacy_"+peer.ID)
	if legacy.MembershipState != "legacy-pending" || legacy.LegacyPeerID != peer.ID {
		t.Fatalf("the legacy placeholder was disturbed: %+v", legacy)
	}
	config, err := database.FleetConfig(ctx)
	if err != nil || config.MigrationState != "legacy-pending" {
		t.Fatalf("migration state=%q err=%v", config.MigrationState, err)
	}
}

func legacyPendingController(t *testing.T) (*Store, Peer, FleetNode) {
	t.Helper()
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	self := testFleetNode("node_00000000000000000000000000000001", "https://192.168.99.10:7071")
	if err := database.EnsureNodeIdentity(ctx, testNodeIdentity(self.NodeID)); err != nil {
		t.Fatal(err)
	}
	peer, err := database.CreatePeer(ctx, "spark-worker", "http://192.168.99.20:7070", "legacy-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeFleetMigration(ctx, self, nil); err != nil {
		t.Fatal(err)
	}
	config, err := database.FleetConfig(ctx)
	if err != nil || config.FleetID == "" || config.MigrationState != "legacy-pending" {
		t.Fatalf("legacy setup config=%+v err=%v", config, err)
	}
	self.FleetID = config.FleetID
	return database, peer, self
}

func findFleetNode(t *testing.T, nodes []FleetNode, nodeID string) FleetNode {
	t.Helper()
	for _, node := range nodes {
		if node.NodeID == nodeID {
			return node
		}
	}
	t.Fatalf("node %s is missing from %+v", nodeID, nodes)
	return FleetNode{}
}

func TestFourConcurrentFleetAdmissionsAtFinalSlotAdmitOne(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	self := testFleetNode("node_00000000000000000000000000000001", "https://192.168.99.10:7071")
	if err := database.EnsureNodeIdentity(ctx, testNodeIdentity(self.NodeID)); err != nil {
		t.Fatal(err)
	}
	config, err := database.EnsureFleetController(ctx, self)
	if err != nil {
		t.Fatal(err)
	}
	for index := 2; index <= 3; index++ {
		node := testFleetNode(fmt.Sprintf("node_%032x", index), fmt.Sprintf("https://192.168.99.%d:7071", 9+index))
		if _, _, err := database.PrepareFleetNode(ctx, self, node); err != nil {
			t.Fatal(err)
		}
		if err := database.CommitFleetNode(ctx, config.FleetID, node.NodeID); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 4)
	var wait sync.WaitGroup
	for index := 4; index <= 7; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			node := testFleetNode(fmt.Sprintf("node_%032x", index), fmt.Sprintf("https://192.168.99.%d:7071", 9+index))
			_, _, err := database.PrepareFleetNode(ctx, self, node)
			results <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)
	winners, full := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrFleetFull):
			full++
		default:
			t.Errorf("unexpected admission error: %v", err)
		}
	}
	if winners != 1 || full != 3 {
		t.Fatalf("winners=%d full=%d, want 1 and 3", winners, full)
	}
}

func makeDatabasePreFleet(t *testing.T, database *Store) {
	t.Helper()
	if _, err := database.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	for _, table := range fleetTables {
		if _, err := database.db.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.db.Exec(`UPDATE schema_meta SET version=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
}

func snapshotTables(t *testing.T, database *sql.DB, tables []string) map[string][][]any {
	t.Helper()
	result := make(map[string][][]any, len(tables))
	for _, table := range tables {
		rows, err := database.Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for index, value := range values {
				if payload, ok := value.([]byte); ok {
					values[index] = string(payload)
				}
			}
			result[table] = append(result[table], values)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func testNodeIdentity(nodeID string) NodeIdentity {
	return NodeIdentity{NodeID: nodeID, PublicKey: []byte("public-" + nodeID), CertificateFingerprint: "fingerprint-" + nodeID, CreatedAt: "2026-08-05T00:00:00Z"}
}

func testFleetNode(nodeID, nodeURL string) FleetNode {
	return FleetNode{NodeID: nodeID, DisplayName: nodeID, ConsoleURL: strings.Replace(nodeURL, "https://", "http://", 1), NodeURL: nodeURL, Certificate: []byte("certificate-" + nodeID), ManagerVersion: "test", ManagerBuildIdentity: "test-build", CatalogueDigest: "catalogue"}
}
