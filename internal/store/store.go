package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type UpdateBlocker struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Activity string `json:"activity"`
	State    string `json:"state"`
}

type Job struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	RecipeID       string          `json:"recipe_id"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	State          string          `json:"state"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Error          string          `json:"error,omitempty"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	Steps          []Step          `json:"steps"`
}

type Step struct {
	Index       int             `json:"index"`
	Operation   string          `json:"operation"`
	State       string          `json:"state"`
	Receipt     json.RawMessage `json:"receipt,omitempty"`
	Error       string          `json:"error,omitempty"`
	StartedAt   string          `json:"started_at,omitempty"`
	CompletedAt string          `json:"completed_at,omitempty"`
}

type InstalledModel struct {
	RecipeID      string `json:"recipe_id"`
	RecipeVersion int    `json:"recipe_version"`
	Status        string `json:"status"`
	ArtifactPath  string `json:"artifact_path"`
	ContainerID   string `json:"container_id,omitempty"`
	Active        bool   `json:"active"`
	UpdatedAt     string `json:"updated_at"`
	// Measured on this device by a benchmark job; zero until measured.
	TokensPerSecond    float64 `json:"tokens_per_second,omitempty"`
	TimeToFirstTokenMS int64   `json:"time_to_first_token_ms,omitempty"`
	MeasuredAt         string  `json:"measured_at,omitempty"`
}

// ModelTokenUsage is how many tokens one model has served on this Spark
// since basement started counting it. The serving runtime publishes
// cumulative counters that restart at zero with its container, so these
// totals are accumulated from readings rather than copied from a counter
// (see RecordTokenSample). A model basement has never taken a reading for
// has no row at all, which is not the same as one that has served nothing.
type ModelTokenUsage struct {
	RecipeID         string `json:"recipe_id"`
	PromptTokens     int64  `json:"prompt_tokens"`
	GenerationTokens int64  `json:"generation_tokens"`
	FirstCountedAt   string `json:"first_counted_at"`
	UpdatedAt        string `json:"updated_at"`
}

type APIKey struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

// Peer identifies another Spark this manager reads fleet status from. The
// stored api_key authenticates outbound calls this manager makes to the peer
// and is deliberately absent from this struct's JSON so a handler cannot
// serialize it into a response by accident; PeerCredentials is the only path
// to the plaintext key, and it never leaves the server process.
type Peer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	CreatedAt string `json:"created_at"`
}

// Role is a stable name clients address instead of a model: an app asks for
// role/fast forever while the owner changes which installed model answers to
// it. A row exists only while a role has a model assigned, so an unassigned
// role is an absent row rather than one pointing at nothing.
type Role struct {
	Name      string `json:"name"`
	RecipeID  string `json:"recipe_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure sqlite: %w", err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := s.db.Exec(`UPDATE jobs SET state='interrupted', updated_at=? WHERE state NOT IN ('ready','failed','cancelled','interrupted','stopped','removed')`, now()); err != nil {
		db.Close()
		return nil, fmt.Errorf("recover jobs: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE installed_models SET status='recovering', updated_at=? WHERE active=1 AND status='ready'`, now()); err != nil {
		db.Close()
		return nil, fmt.Errorf("mark active models for health reconciliation: %w", err)
	}
	// The generation queue lives in memory, so a generation that was queued
	// or running when this process stopped is not coming back. Saying so is
	// the honest answer; leaving it as "running" would be a spinner that
	// never resolves.
	if _, err := s.db.Exec(`UPDATE generations SET status='interrupted', error=?, finished_at=? WHERE status IN ('queued','running')`,
		"basement restarted while this generation was in progress", now()); err != nil {
		db.Close()
		return nil, fmt.Errorf("recover generations: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL);
INSERT INTO schema_meta(version) SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM schema_meta);
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  recipe_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  state TEXT NOT NULL,
  payload TEXT NOT NULL DEFAULT '{}',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(kind, recipe_id, idempotency_key)
);
CREATE TABLE IF NOT EXISTS job_steps (
  job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  step_index INTEGER NOT NULL,
  operation TEXT NOT NULL,
  state TEXT NOT NULL,
  receipt TEXT NOT NULL DEFAULT '{}',
  error TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL DEFAULT '',
  completed_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(job_id, step_index)
);
CREATE TABLE IF NOT EXISTS installed_models (
  recipe_id TEXT PRIMARY KEY,
  recipe_version INTEGER NOT NULL,
  status TEXT NOT NULL,
  artifact_path TEXT NOT NULL,
  container_id TEXT NOT NULL DEFAULT '',
  active INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS accepted_licences (
  recipe_id TEXT NOT NULL,
  recipe_version INTEGER NOT NULL,
  accepted_at TEXT NOT NULL,
  PRIMARY KEY(recipe_id, recipe_version)
);
CREATE TABLE IF NOT EXISTS territory_confirmations (
  recipe_id TEXT NOT NULL,
  recipe_version INTEGER NOT NULL,
  confirmed_at TEXT NOT NULL,
  PRIMARY KEY(recipe_id, recipe_version)
);
CREATE TABLE IF NOT EXISTS api_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  last_used_at TEXT NOT NULL DEFAULT '',
  revoked_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS model_metrics (
  recipe_id TEXT PRIMARY KEY,
  tokens_per_second REAL NOT NULL,
  time_to_first_token_ms INTEGER NOT NULL DEFAULT 0,
  measured_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS model_token_usage (
  recipe_id TEXT PRIMARY KEY,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  generation_tokens INTEGER NOT NULL DEFAULT 0,
  last_prompt_counter REAL NOT NULL DEFAULT 0,
  last_generation_counter REAL NOT NULL DEFAULT 0,
  first_counted_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS peers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  base_url TEXT NOT NULL,
  api_key TEXT NOT NULL,
  created_at TEXT NOT NULL,
  singleton INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS roles (
  name TEXT PRIMARY KEY,
  recipe_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS generations (
  id TEXT PRIMARY KEY,
  recipe_id TEXT NOT NULL,
  mode TEXT NOT NULL,
  first_frame TEXT NOT NULL DEFAULT '',
  prompt TEXT NOT NULL,
  blocks INTEGER NOT NULL,
  short_edge INTEGER NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  frames INTEGER NOT NULL,
  seed INTEGER NOT NULL,
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  output_path TEXT NOT NULL DEFAULT '',
  bytes INTEGER NOT NULL DEFAULT 0,
  progress_value INTEGER NOT NULL DEFAULT 0,
  progress_max INTEGER NOT NULL DEFAULT 0,
  progress_phase TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  started_at TEXT NOT NULL DEFAULT '',
  finished_at TEXT NOT NULL DEFAULT ''
);`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if err := s.migratePeersSingleton(); err != nil {
		return err
	}
	if err := s.migrateGenerationProgress(); err != nil {
		return err
	}
	if err := s.migrateGenerationFirstFrame(); err != nil {
		return err
	}
	if err := s.migrateFleetSchema(); err != nil {
		return err
	}
	if err := s.migrateReservationOwnership(); err != nil {
		return err
	}
	if err := s.migrateFleetUpgradeSchema(); err != nil {
		return err
	}
	if err := s.migrateRecipeRevocations(); err != nil {
		return err
	}
	return s.ensureFleetUpgradeResolveColumns()
}

// ensureFleetUpgradeResolveColumns adds the resolve columns to a
// fleet_upgrade_nodes table created by a release that shipped before they
// existed. The migration that creates the table was amended in place on the
// assumption it had never shipped; hardware proved otherwise (2026-08-12): a
// database whose schema version was already recorded skipped the amended
// migration entirely and the first fleet upgrade failed on the missing
// column. Checking the actual table shape is idempotent and heals every
// database regardless of which code created it.
func (s *Store) ensureFleetUpgradeResolveColumns() error {
	rows, err := s.db.Query(`PRAGMA table_info(fleet_upgrade_nodes)`)
	if err != nil {
		return fmt.Errorf("inspect fleet_upgrade_nodes: %w", err)
	}
	present := map[string]bool{}
	for rows.Next() {
		var (
			id         int
			name, kind string
			notNull    int
			defaultVal any
			primaryKey int
		)
		if err := rows.Scan(&id, &name, &kind, &notNull, &defaultVal, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		present[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for column, definition := range map[string]string{
		"resolve_state":   `ALTER TABLE fleet_upgrade_nodes ADD COLUMN resolve_state TEXT NOT NULL DEFAULT ''`,
		"resolve_failure": `ALTER TABLE fleet_upgrade_nodes ADD COLUMN resolve_failure TEXT NOT NULL DEFAULT ''`,
	} {
		if present[column] {
			continue
		}
		if _, err := s.db.Exec(definition); err != nil {
			return fmt.Errorf("add fleet_upgrade_nodes.%s: %w", column, err)
		}
	}
	return nil
}

const (
	fleetFoundationSchemaVersion = 2
	reservationSchemaVersion     = 3
	fleetSchemaVersion           = 4
	revocationSchemaVersion      = 5
	fleetMigrationStatementCount = 10
)

// fleetMigrationStep is a failure-injection seam for the migration tests.
// Production never reassigns it. The fleet tables form one authority boundary,
// so every statement and the schema version advance belong to one transaction.
var fleetMigrationStep = func(int) error { return nil }

// migrateFleetSchema is the first migration that uses schema_meta. The older
// manager seeded version 1 but never read it. Advancing it now records whether
// this multi-table unit committed without making rollback depend on that value:
// the previous manager ignores version 2 and continues to use its unchanged
// tables, while this manager retries the whole transaction after any failure.
func (s *Store) migrateFleetSchema() error {
	var version int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_meta`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= fleetFoundationSchemaVersion {
		return nil
	}
	statements := []string{
		`CREATE TABLE node_identity (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  node_id TEXT NOT NULL UNIQUE,
  public_key BLOB NOT NULL,
  certificate_fingerprint TEXT NOT NULL,
  created_at TEXT NOT NULL
)`,
		`CREATE TABLE fleet_config (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  fleet_id TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'standalone',
  controller_node_id TEXT NOT NULL DEFAULT '',
  controller_console_url TEXT NOT NULL DEFAULT '',
  controller_node_url TEXT NOT NULL DEFAULT '',
  controller_certificate BLOB NOT NULL DEFAULT '',
  membership_epoch INTEGER NOT NULL DEFAULT 0,
  joined_at TEXT NOT NULL DEFAULT '',
  migration_state TEXT NOT NULL DEFAULT 'ready',
  outbound_heartbeat_sequence INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TABLE fleet_nodes (
  fleet_id TEXT NOT NULL,
  node_id TEXT NOT NULL,
  display_name TEXT NOT NULL,
  console_url TEXT NOT NULL,
  node_url TEXT NOT NULL,
  certificate BLOB NOT NULL,
  manager_version TEXT NOT NULL DEFAULT '',
  manager_build_identity TEXT NOT NULL DEFAULT '',
  catalogue_digest TEXT NOT NULL DEFAULT '',
  membership_state TEXT NOT NULL,
  heartbeat_sequence INTEGER NOT NULL DEFAULT 0,
  heartbeat_received_at TEXT NOT NULL DEFAULT '',
  heartbeat_payload BLOB NOT NULL DEFAULT '',
  heartbeat_signature BLOB NOT NULL DEFAULT '',
  legacy_peer_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(fleet_id,node_id),
  UNIQUE(fleet_id,node_url)
)`,
		`CREATE TABLE fleet_deployments (
  deployment_id TEXT PRIMARY KEY,
  recipe_id TEXT NOT NULL,
  recipe_version INTEGER NOT NULL,
  recipe_fingerprint TEXT NOT NULL,
  topology_count INTEGER NOT NULL,
  owner_node_id TEXT NOT NULL,
  owner_job_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  last_observed_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
)`,
		`CREATE TABLE fleet_deployment_nodes (
  deployment_id TEXT NOT NULL REFERENCES fleet_deployments(deployment_id) ON DELETE CASCADE,
  node_id TEXT NOT NULL,
  node_role TEXT NOT NULL,
  rank INTEGER NOT NULL,
  reservation_id TEXT NOT NULL DEFAULT '',
  fabric_interface TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(deployment_id,node_id)
)`,
		`CREATE TABLE node_reservations (
  reservation_id TEXT PRIMARY KEY,
  deployment_id TEXT NOT NULL,
  fleet_id TEXT NOT NULL,
  controller_node_id TEXT NOT NULL,
  driver_node_id TEXT NOT NULL,
  recipe_id TEXT NOT NULL,
  recipe_version INTEGER NOT NULL,
  recipe_fingerprint TEXT NOT NULL,
  state TEXT NOT NULL,
  claims_json BLOB NOT NULL,
  prepare_token_hash TEXT NOT NULL,
  grant_json BLOB NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
)`,
		`CREATE TABLE distributed_ranks (
  deployment_id TEXT PRIMARY KEY,
  recipe_id TEXT NOT NULL,
  recipe_version INTEGER NOT NULL,
  recipe_fingerprint TEXT NOT NULL,
  rank INTEGER NOT NULL,
  driver_node_id TEXT NOT NULL,
  placement_json BLOB NOT NULL,
  container_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  driver_lease_expires_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
)`,
		`CREATE TABLE fleet_join_codes (
  code_hash TEXT PRIMARY KEY,
  expires_at TEXT NOT NULL,
  consumed_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
)`,
		`CREATE TABLE fleet_pending_joins (
  prepare_token_hash TEXT PRIMARY KEY,
  fleet_id TEXT NOT NULL,
  controller_node_id TEXT NOT NULL,
  controller_console_url TEXT NOT NULL,
  controller_node_url TEXT NOT NULL,
  controller_certificate BLOB NOT NULL,
  controller_certificate_fingerprint TEXT NOT NULL,
  membership_epoch INTEGER NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
)`,
		`UPDATE schema_meta SET version=2`,
	}
	if len(statements) != fleetMigrationStatementCount {
		return errors.New("fleet schema migration statement count is inconsistent")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin fleet schema migration: %w", err)
	}
	defer tx.Rollback()
	for index, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate fleet schema statement %d: %w", index+1, err)
		}
		if err := fleetMigrationStep(index + 1); err != nil {
			return fmt.Errorf("migrate fleet schema statement %d: %w", index+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit fleet schema migration: %w", err)
	}
	return nil
}

// migrateReservationOwnership separates a committed grant's short protocol
// deadline from the job that has started using it. A target writes job_id
// before starting work, so restart reconciliation can keep an interrupted
// download while expiring a controller that committed and then disappeared.
func (s *Store) migrateReservationOwnership() error {
	var version int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_meta`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= reservationSchemaVersion {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin reservation ownership migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE node_reservations ADD COLUMN job_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add reservation job ownership: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_meta SET version=3`); err != nil {
		return fmt.Errorf("advance reservation ownership schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reservation ownership migration: %w", err)
	}
	return nil
}

// migrateFleetUpgradeSchema adds only new tables so the immediately previous
// manager can still read and write every pre-existing row after a local
// updater rollback. The controller journal must survive its own restart, so
// the signed public release inputs and every node transition are committed in
// the same database that already owns fleet membership.
func (s *Store) migrateFleetUpgradeSchema() error {
	var version int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_meta`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= fleetSchemaVersion {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin fleet upgrade migration: %w", err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS fleet_upgrade_runs (
  run_id TEXT PRIMARY KEY,
  fleet_id TEXT NOT NULL,
  controller_node_id TEXT NOT NULL,
  release_tag TEXT NOT NULL,
  target_version TEXT NOT NULL,
  manifest_sha256 TEXT NOT NULL,
  manifest_bytes BLOB NOT NULL,
  signature_bytes BLOB NOT NULL,
  asset_url TEXT NOT NULL,
  state TEXT NOT NULL,
  failure TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS fleet_upgrade_nodes (
  run_id TEXT NOT NULL REFERENCES fleet_upgrade_runs(run_id) ON DELETE CASCADE,
  node_id TEXT NOT NULL,
  display_name TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  role TEXT NOT NULL,
  state TEXT NOT NULL,
  running_version TEXT NOT NULL,
  target_version TEXT NOT NULL,
  attempt_id TEXT NOT NULL DEFAULT '',
  failure TEXT NOT NULL DEFAULT '',
  resolve_state TEXT NOT NULL DEFAULT '',
  resolve_failure TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(run_id,node_id),
  UNIQUE(run_id,sequence)
)`,
		`UPDATE schema_meta SET version=4`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate fleet upgrade schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit fleet upgrade migration: %w", err)
	}
	return nil
}

// migrateRecipeRevocations adds the permanent home for revocations accepted
// from a signed index (ADR 0009 item 7). It is a new migration rather than an
// edit to an existing one: amending a shipped migration in place is exactly
// what left fleet_upgrade_nodes missing its resolve columns on hardware
// (2026-08-12), because a database whose schema version was already recorded
// skips the amended step entirely. Anything this table ever needs beyond its
// current shape belongs in the migration after this one.
//
// The table only ever gains rows. There is no un-revoke path anywhere above
// it, so a later index that omits an entry cannot restore what a compromised
// key pulled; the honest remedy for an over-broad revocation is publishing a
// fixed new version.
func (s *Store) migrateRecipeRevocations() error {
	var version int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_meta`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= revocationSchemaVersion {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin recipe revocation migration: %w", err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS recipe_revocations (
  recipe_id TEXT NOT NULL,
  recipe_version INTEGER NOT NULL,
  reason TEXT NOT NULL,
  revoked_at TEXT NOT NULL,
  accepted_at TEXT NOT NULL,
  PRIMARY KEY(recipe_id,recipe_version)
)`,
		`UPDATE schema_meta SET version=5`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate recipe revocations: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit recipe revocation migration: %w", err)
	}
	return nil
}

// migratePeersSingleton brings a database created before this column up to
// the current shape: peers holds at most one row, and that is a property of
// the schema rather than of a check somewhere in a handler.
//
// A database that somehow already holds several peers is left without the
// index instead of refusing to open. It is already a broken fleet
// (cmd/basement/main.go will not pick a worker from it) and the console is
// how the owner removes the extra one, so locking them out of the console
// would be the worse of the two failures. CreatePeer's conditional insert
// still keeps such a database from growing another row.
func (s *Store) migratePeersSingleton() error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('peers')`)
	if err != nil {
		return fmt.Errorf("inspect peers table: %w", err)
	}
	present := false
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			rows.Close()
			return err
		}
		if column == "singleton" {
			present = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if !present {
		if _, err := s.db.Exec(`ALTER TABLE peers ADD COLUMN singleton INTEGER NOT NULL DEFAULT 1`); err != nil {
			return fmt.Errorf("add peers.singleton: %w", err)
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM peers`).Scan(&count); err != nil {
		return fmt.Errorf("count peers: %w", err)
	}
	if count > 1 {
		return nil
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS peers_singleton ON peers(singleton)`); err != nil {
		return fmt.Errorf("constrain peers to a single row: %w", err)
	}
	return nil
}

// migrateGenerationProgress keeps the progress bridge additive for databases
// created before these columns existed. Older binaries ignore extra SQLite
// columns, so a manager rollback can still read every generation row.
func (s *Store) migrateGenerationProgress() error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('generations')`)
	if err != nil {
		return fmt.Errorf("inspect generations table: %w", err)
	}
	present := map[string]bool{}
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			rows.Close()
			return err
		}
		present[column] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "progress_value", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "progress_max", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "progress_phase", definition: "TEXT NOT NULL DEFAULT ''"},
	} {
		if present[column.name] {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE generations ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
			return fmt.Errorf("add generations.%s: %w", column.name, err)
		}
	}
	return nil
}

// migrateGenerationFirstFrame keeps the image-mode bridge additive for
// databases created before image_to_video shipped, the same way
// migrateGenerationProgress does for the progress columns.
func (s *Store) migrateGenerationFirstFrame() error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('generations')`)
	if err != nil {
		return fmt.Errorf("inspect generations table: %w", err)
	}
	present := false
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			rows.Close()
			return err
		}
		if column == "first_frame" {
			present = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if present {
		return nil
	}
	if _, err := s.db.Exec(`ALTER TABLE generations ADD COLUMN first_frame TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add generations.first_frame: %w", err)
	}
	return nil
}

func (s *Store) CreateJob(ctx context.Context, kind, recipeID, key string, payload any) (Job, bool, error) {
	if key == "" {
		return Job{}, false, errors.New("idempotency key is required")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Job{}, false, fmt.Errorf("encode job payload: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	id, err := randomID("job_")
	if err != nil {
		return Job{}, false, err
	}
	timestamp := now()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO jobs(id,kind,recipe_id,idempotency_key,state,payload,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, id, kind, recipeID, key, "queued", string(body), timestamp, timestamp)
	if err != nil {
		return Job{}, false, fmt.Errorf("create job: %w", err)
	}
	count, _ := result.RowsAffected()
	created := count == 1
	if !created {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE kind=? AND recipe_id=? AND idempotency_key=?`, kind, recipeID, key).Scan(&id); err != nil {
			return Job{}, false, fmt.Errorf("find idempotent job: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	job, err := s.GetJob(ctx, id)
	return job, created, err
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	var job Job
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT id,kind,recipe_id,idempotency_key,state,payload,error,created_at,updated_at FROM jobs WHERE id=?`, id).
		Scan(&job.ID, &job.Kind, &job.RecipeID, &job.IdempotencyKey, &job.State, &payload, &job.Error, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, os.ErrNotExist
		}
		return Job{}, err
	}
	job.Payload = json.RawMessage(payload)
	steps, err := s.steps(ctx, id)
	if err != nil {
		return Job{}, err
	}
	job.Steps = steps
	return job, nil
}

func (s *Store) UpdateJobPayload(ctx context.Context, id string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode job payload: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET payload=?,updated_at=? WHERE id=?`, string(body), now(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

// ListJobs answers newest first. The order key is rowid, the insertion order,
// not created_at: see now() for why the timestamp text does not sort
// chronologically. A job row is inserted once, at creation, so the insertion
// order is the creation order.
func (s *Store) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs ORDER BY rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(ids))
	for _, id := range ids {
		job, err := s.GetJob(ctx, id)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// ActiveUpdateBlocker names the first engine or generation activity that a
// manager restart would interrupt. The query is one database snapshot so the
// API never reports an idle machine assembled from two different moments.
//
// The order key is created_at, not rowid: this query ranks a job against a
// generation, and a rowid counts rows in its own table only, so no rowid can
// compare the two. rtrim removes the trailing 'Z', which is the one byte that
// breaks the text order, and what stays compares chronologically. See now()
// for the rule.
func (s *Store) ActiveUpdateBlocker(ctx context.Context) (UpdateBlocker, bool, error) {
	var blocker UpdateBlocker
	var recipeID string
	err := s.db.QueryRowContext(ctx, `
		SELECT category,id,activity,recipe_id,state FROM (
			SELECT 'job' AS category,id,kind AS activity,recipe_id,state,created_at
			FROM jobs
			WHERE state NOT IN ('ready','failed','cancelled','stopped','removed')
			UNION ALL
			SELECT 'generation' AS category,id,'generation' AS activity,recipe_id,status AS state,created_at
			FROM generations
			WHERE status IN ('queued','running')
		)
		ORDER BY rtrim(created_at,'Z') ASC
		LIMIT 1`).Scan(&blocker.Kind, &blocker.ID, &blocker.Activity, &recipeID, &blocker.State)
	if errors.Is(err, sql.ErrNoRows) {
		return UpdateBlocker{}, false, nil
	}
	if err != nil {
		return UpdateBlocker{}, false, err
	}
	if blocker.Kind == "generation" {
		blocker.Activity = fmt.Sprintf("generation %s for %s", blocker.ID, recipeID)
	} else {
		blocker.Activity = fmt.Sprintf("%s job %s for %s", blocker.Activity, blocker.ID, recipeID)
	}
	return blocker, true, nil
}

func (s *Store) UpdateJobState(ctx context.Context, id, state, message string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=?, error=?, updated_at=? WHERE id=?`, state, message, now(), id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return os.ErrNotExist
	}
	return nil
}

// MarkCancelling records cancellation intent without claiming the terminal
// state: the running goroutine may still need to roll back a partial switch
// and stays the only writer of the final job state. Returns false when the
// job already reached a terminal state.
func (s *Store) MarkCancelling(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state='cancelling', error=?, updated_at=? WHERE id=? AND state NOT IN ('ready','failed','cancelled','stopped','removed')`,
		"cancellation requested; finishing at a safe operation boundary", now(), id)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	return count == 1, nil
}

func (s *Store) BeginStep(ctx context.Context, jobID string, index int, operation string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_steps(job_id,step_index,operation,state,started_at) VALUES(?,?,?,?,?)
ON CONFLICT(job_id,step_index) DO UPDATE SET operation=excluded.operation,state='running',error='',started_at=excluded.started_at,completed_at=''`, jobID, index, operation, "running", now())
	return err
}

func (s *Store) UpdateStepReceipt(ctx context.Context, jobID string, index int, receipt any) error {
	body, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE job_steps SET receipt=? WHERE job_id=? AND step_index=?`, string(body), jobID, index)
	return err
}

func (s *Store) CompleteStep(ctx context.Context, jobID string, index int, receipt any) error {
	body, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE job_steps SET state='completed',receipt=?,error='',completed_at=? WHERE job_id=? AND step_index=?`, string(body), now(), jobID, index)
	return err
}

func (s *Store) FailStep(ctx context.Context, jobID string, index int, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE job_steps SET state='failed',error=?,completed_at=? WHERE job_id=? AND step_index=?`, message, now(), jobID, index)
	return err
}

func (s *Store) Step(ctx context.Context, jobID string, index int) (Step, bool, error) {
	var step Step
	var receipt string
	err := s.db.QueryRowContext(ctx, `SELECT step_index,operation,state,receipt,error,started_at,completed_at FROM job_steps WHERE job_id=? AND step_index=?`, jobID, index).
		Scan(&step.Index, &step.Operation, &step.State, &receipt, &step.Error, &step.StartedAt, &step.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Step{}, false, nil
	}
	if err != nil {
		return Step{}, false, err
	}
	step.Receipt = json.RawMessage(receipt)
	return step, true, nil
}

func (s *Store) steps(ctx context.Context, jobID string) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT step_index,operation,state,receipt,error,started_at,completed_at FROM job_steps WHERE job_id=? ORDER BY step_index`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Step{}
	for rows.Next() {
		var step Step
		var receipt string
		if err := rows.Scan(&step.Index, &step.Operation, &step.State, &receipt, &step.Error, &step.StartedAt, &step.CompletedAt); err != nil {
			return nil, err
		}
		step.Receipt = json.RawMessage(receipt)
		result = append(result, step)
	}
	return result, rows.Err()
}

func (s *Store) SetInstalled(ctx context.Context, model InstalledModel) error {
	model.UpdatedAt = now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO installed_models(recipe_id,recipe_version,status,artifact_path,container_id,active,updated_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(recipe_id) DO UPDATE SET recipe_version=excluded.recipe_version,status=excluded.status,artifact_path=excluded.artifact_path,container_id=excluded.container_id,active=excluded.active,updated_at=excluded.updated_at`,
		model.RecipeID, model.RecipeVersion, model.Status, model.ArtifactPath, model.ContainerID, model.Active, model.UpdatedAt)
	return err
}

// ActivateExclusively installs/updates the model and demotes every other
// model in one transaction, so a crash or write error can never leave two
// models marked active.
func (s *Store) ActivateExclusively(ctx context.Context, model InstalledModel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	model.UpdatedAt = now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO installed_models(recipe_id,recipe_version,status,artifact_path,container_id,active,updated_at) VALUES(?,?,?,?,?,1,?)
ON CONFLICT(recipe_id) DO UPDATE SET recipe_version=excluded.recipe_version,status=excluded.status,artifact_path=excluded.artifact_path,container_id=excluded.container_id,active=1,updated_at=excluded.updated_at`,
		model.RecipeID, model.RecipeVersion, model.Status, model.ArtifactPath, model.ContainerID, model.UpdatedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE installed_models SET status=CASE WHEN active=1 THEN 'stopped' ELSE status END,active=0,updated_at=? WHERE recipe_id<>?`, model.UpdatedAt, model.RecipeID); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrSwitchTargetChanged is what BeginSwitch gives when the serving slot no
// longer belongs to the predecessor named by its caller. The engine re-plans
// under its runtime lock immediately before this transaction, but this check
// remains the durable last guard for another process or a future caller that
// does not share that lock. Stopping the stale predecessor would stop nothing
// and leave the real one running. A predecessor that simply stopped is not
// this case: the room the switch wanted is free, so it proceeds.
var ErrSwitchTargetChanged = errors.New("another model took over serving while this job was running")

func (s *Store) BeginSwitch(ctx context.Context, previousRecipeID, targetRecipeID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	timestamp := now()
	// active=1 is the guarantee: the row must still be the serving one at the
	// moment the switch begins, not merely when it was planned.
	result, err := tx.ExecContext(ctx, `UPDATE installed_models SET status='switching',updated_at=? WHERE recipe_id=? AND active=1`, timestamp, previousRecipeID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM installed_models WHERE recipe_id=?`, previousRecipeID).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			return os.ErrNotExist
		}
		var usurpers int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM installed_models WHERE active=1 AND recipe_id NOT IN (?,?)`, previousRecipeID, targetRecipeID).Scan(&usurpers); err != nil {
			return err
		}
		if usurpers > 0 {
			return ErrSwitchTargetChanged
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE installed_models SET status='starting',active=0,updated_at=? WHERE recipe_id=?`, timestamp, targetRecipeID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetModelState(ctx context.Context, recipeID, status string, active bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE installed_models SET status=?,active=?,updated_at=? WHERE recipe_id=?`, status, active, now(), recipeID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

func (s *Store) SetOnlyActive(ctx context.Context, recipeID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	timestamp := now()
	result, err := tx.ExecContext(ctx, `UPDATE installed_models SET active=1,status='ready',updated_at=? WHERE recipe_id=?`, timestamp, recipeID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	if _, err := tx.ExecContext(ctx, `UPDATE installed_models SET status=CASE WHEN active=1 THEN 'stopped' ELSE status END,active=0,updated_at=? WHERE recipe_id<>?`, timestamp, recipeID); err != nil {
		return err
	}
	return tx.Commit()
}

const modelColumns = `m.recipe_id,m.recipe_version,m.status,m.artifact_path,m.container_id,m.active,m.updated_at,
COALESCE(x.tokens_per_second,0),COALESCE(x.time_to_first_token_ms,0),COALESCE(x.measured_at,'')`

func scanModel(row interface{ Scan(...any) error }) (InstalledModel, error) {
	var model InstalledModel
	err := row.Scan(&model.RecipeID, &model.RecipeVersion, &model.Status, &model.ArtifactPath, &model.ContainerID, &model.Active, &model.UpdatedAt,
		&model.TokensPerSecond, &model.TimeToFirstTokenMS, &model.MeasuredAt)
	return model, err
}

// Models lists what is installed, most recently changed first. The order key
// is updated_at, not rowid: a rowid records when a model was first installed,
// and this list must follow the last change instead. rtrim removes the
// trailing 'Z' so the text compares chronologically. See now() for the rule.
func (s *Store) Models(ctx context.Context) ([]InstalledModel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+modelColumns+` FROM installed_models m LEFT JOIN model_metrics x ON x.recipe_id=m.recipe_id ORDER BY rtrim(m.updated_at,'Z') DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []InstalledModel{}
	for rows.Next() {
		model, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, model)
	}
	return result, rows.Err()
}

func (s *Store) Model(ctx context.Context, recipeID string) (InstalledModel, error) {
	model, err := scanModel(s.db.QueryRowContext(ctx, `SELECT `+modelColumns+` FROM installed_models m LEFT JOIN model_metrics x ON x.recipe_id=m.recipe_id WHERE m.recipe_id=?`, recipeID))
	if errors.Is(err, sql.ErrNoRows) {
		return InstalledModel{}, os.ErrNotExist
	}
	return model, err
}

func (s *Store) SetModelMetrics(ctx context.Context, recipeID string, tokensPerSecond float64, timeToFirstTokenMS int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO model_metrics(recipe_id,tokens_per_second,time_to_first_token_ms,measured_at) VALUES(?,?,?,?)
ON CONFLICT(recipe_id) DO UPDATE SET tokens_per_second=excluded.tokens_per_second,time_to_first_token_ms=excluded.time_to_first_token_ms,measured_at=excluded.measured_at`,
		recipeID, tokensPerSecond, timeToFirstTokenMS, now())
	return err
}

// RecordTokenSample folds one reading of a runtime's cumulative token
// counters into a model's running totals. The counters live inside the
// serving container and restart at zero with it, so a reading at or above
// the previous one contributes the difference and a lower one contributes
// its whole value: the drop is a restart, not work that did not happen. The
// last reading is persisted next to the totals, so a manager restart neither
// loses a stretch of usage nor counts it twice.
func (s *Store) RecordTokenSample(ctx context.Context, recipeID string, promptCounter, generationCounter float64) error {
	if promptCounter < 0 || generationCounter < 0 {
		return errors.New("token counters cannot be negative")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	usage := ModelTokenUsage{RecipeID: recipeID}
	var lastPrompt, lastGeneration float64
	err = tx.QueryRowContext(ctx, `SELECT prompt_tokens,generation_tokens,last_prompt_counter,last_generation_counter,first_counted_at FROM model_token_usage WHERE recipe_id=?`, recipeID).
		Scan(&usage.PromptTokens, &usage.GenerationTokens, &lastPrompt, &lastGeneration, &usage.FirstCountedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	usage.UpdatedAt = now()
	if errors.Is(err, sql.ErrNoRows) {
		usage.FirstCountedAt = usage.UpdatedAt
	}
	usage.PromptTokens += tokenDelta(promptCounter, lastPrompt)
	usage.GenerationTokens += tokenDelta(generationCounter, lastGeneration)
	if _, err := tx.ExecContext(ctx, `INSERT INTO model_token_usage(recipe_id,prompt_tokens,generation_tokens,last_prompt_counter,last_generation_counter,first_counted_at,updated_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(recipe_id) DO UPDATE SET prompt_tokens=excluded.prompt_tokens,generation_tokens=excluded.generation_tokens,last_prompt_counter=excluded.last_prompt_counter,last_generation_counter=excluded.last_generation_counter,updated_at=excluded.updated_at`,
		usage.RecipeID, usage.PromptTokens, usage.GenerationTokens, promptCounter, generationCounter, usage.FirstCountedAt, usage.UpdatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// tokenDelta is the usage one reading adds: the rise since the last reading,
// or the whole reading when the counter went backwards, which only happens
// when the runtime that publishes it restarted.
func tokenDelta(current, last float64) int64 {
	if current < last {
		return int64(current)
	}
	return int64(current - last)
}

// ResetTokenCounters zeroes a model's stored last-seen runtime counters
// without touching its accumulated totals. Call this once basement itself
// has taken the final reading from a container it is about to stop: those
// counters can never rise again, so leaving them in place would make the
// next container's first reading compare against a dead series and read as
// only a partial rise (or a "restart" it already correctly is, but at the
// wrong, undercounted split). A model with no usage row yet has nothing to
// reset, so this is a no-op rather than an error.
func (s *Store) ResetTokenCounters(ctx context.Context, recipeID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE model_token_usage SET last_prompt_counter=0, last_generation_counter=0 WHERE recipe_id=?`, recipeID)
	return err
}

func (s *Store) TokenUsage(ctx context.Context) ([]ModelTokenUsage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT recipe_id,prompt_tokens,generation_tokens,first_counted_at,updated_at FROM model_token_usage ORDER BY prompt_tokens+generation_tokens DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ModelTokenUsage{}
	for rows.Next() {
		var usage ModelTokenUsage
		if err := rows.Scan(&usage.RecipeID, &usage.PromptTokens, &usage.GenerationTokens, &usage.FirstCountedAt, &usage.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, usage)
	}
	return result, rows.Err()
}

// CreateAPIKey returns the stored record and the plaintext secret, which is
// shown exactly once and persisted only as a SHA-256 hash.
func (s *Store) CreateAPIKey(ctx context.Context, name string) (APIKey, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return APIKey{}, "", errors.New("a key name between 1 and 64 characters is required")
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return APIKey{}, "", err
	}
	secret := "rosk_" + hex.EncodeToString(raw[:])
	id, err := randomID("key_")
	if err != nil {
		return APIKey{}, "", err
	}
	key := APIKey{ID: id, Name: name, CreatedAt: now()}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO api_keys(id,name,token_hash,created_at) VALUES(?,?,?,?)`, key.ID, key.Name, hashSecret(secret), key.CreatedAt); err != nil {
		return APIKey{}, "", err
	}
	return key, secret, nil
}

// APIKeys lists the live keys oldest first. The order key is rowid, the
// insertion order: see now() for why the timestamp text does not sort
// chronologically.
func (s *Store) APIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,created_at,last_used_at FROM api_keys WHERE revoked_at='' ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []APIKey{}
	for rows.Next() {
		var key APIKey
		if err := rows.Scan(&key.ID, &key.Name, &key.CreatedAt, &key.LastUsedAt); err != nil {
			return nil, err
		}
		result = append(result, key)
	}
	return result, rows.Err()
}

func (s *Store) RevokeAPIKey(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_at=? WHERE id=? AND revoked_at=''`, now(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

// VerifyAPIKey reports whether the supplied secret matches an unrevoked key.
// Lookup is by hash, so timing reveals nothing about stored secrets.
func (s *Store) VerifyAPIKey(ctx context.Context, secret string) bool {
	if !strings.HasPrefix(secret, "rosk_") {
		return false
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM api_keys WHERE token_hash=? AND revoked_at=''`, hashSecret(secret)).Scan(&id)
	if err != nil {
		return false
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at=? WHERE id=?`, now(), id)
	return true
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ErrPeerExists is what CreatePeer gives when the fleet already has its one
// Spark. It is a plain sentence because it reaches the owner as one.
var ErrPeerExists = errors.New("another Spark is already in the fleet, so remove it under Fleet before adding a different one")

// CreatePeer records another Spark to poll for fleet status. The caller is
// responsible for having already proven base_url and api_key work together
// before this is called; this method only persists.
//
// At most one peer exists (ADR 0005 defers multi-peer fleets, and
// cmd/basement/main.go refuses to pick a worker when there is more than one),
// and that rule lives here rather than in the callers. Reading the table and
// then inserting leaves a window between the two, and there are two doors
// into this: a console adoption and a manual add can be in flight at the same
// moment. The insert is conditional in a single statement, so exactly one of
// them writes a row and the other is told why it did not.
func (s *Store) CreatePeer(ctx context.Context, name, baseURL, apiKey string) (Peer, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return Peer{}, errors.New("a Spark name between 1 and 64 characters is required")
	}
	if baseURL == "" {
		return Peer{}, errors.New("a base URL is required")
	}
	if apiKey == "" {
		return Peer{}, errors.New("an API key is required")
	}
	id, err := randomID("peer_")
	if err != nil {
		return Peer{}, err
	}
	peer := Peer{ID: id, Name: name, BaseURL: baseURL, CreatedAt: now()}
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO peers(id,name,base_url,api_key,created_at,singleton)
		 SELECT ?,?,?,?,?,1 WHERE NOT EXISTS (SELECT 1 FROM peers)`,
		peer.ID, peer.Name, peer.BaseURL, apiKey, peer.CreatedAt)
	if err != nil {
		return Peer{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return Peer{}, ErrPeerExists
	}
	return peer, nil
}

// Peers never includes the api_key column, so a handler cannot leak a
// credential just by forwarding this result. The order key is rowid, the
// insertion order: see now() for why the timestamp text does not sort
// chronologically.
func (s *Store) Peers(ctx context.Context) ([]Peer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,base_url,created_at FROM peers ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Peer{}
	for rows.Next() {
		var peer Peer
		if err := rows.Scan(&peer.ID, &peer.Name, &peer.BaseURL, &peer.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, peer)
	}
	return result, rows.Err()
}

// PeerCredentials is the sole path to a peer's plaintext api_key; it exists
// only for the server to authenticate its own outbound calls to that peer,
// and its result must never be written to an HTTP response or a log line.
func (s *Store) PeerCredentials(ctx context.Context, id string) (Peer, string, error) {
	var peer Peer
	var apiKey string
	err := s.db.QueryRowContext(ctx, `SELECT id,name,base_url,created_at,api_key FROM peers WHERE id=?`, id).
		Scan(&peer.ID, &peer.Name, &peer.BaseURL, &peer.CreatedAt, &apiKey)
	if errors.Is(err, sql.ErrNoRows) {
		return Peer{}, "", os.ErrNotExist
	}
	return peer, apiKey, err
}

func (s *Store) DeletePeer(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM peers WHERE id=?`, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

// NormalizeRoleName trims and lower-cases a role name and rejects anything a
// client could not put in a model field cleanly: the name travels as
// "role/<name>" in an OpenAI request, so it holds to lowercase letters,
// numbers and inner dashes and nothing else.
func NormalizeRoleName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	invalid := errors.New("a role name uses lowercase letters, numbers and dashes, like fast or code-review")
	if name == "" || len(name) > 32 {
		return "", invalid
	}
	for index, letter := range name {
		switch {
		case letter >= 'a' && letter <= 'z', letter >= '0' && letter <= '9':
		case letter == '-' && index > 0 && index < len(name)-1:
		default:
			return "", invalid
		}
	}
	return name, nil
}

// AssignRole points a role at an installed model, creating the role if this
// is its first assignment. Whether the recipe is installed is the caller's
// question to answer, not this table's. Reassigning keeps created_at, so a
// role's age survives every change of the model behind it.
func (s *Store) AssignRole(ctx context.Context, name, recipeID string) (Role, error) {
	name, err := NormalizeRoleName(name)
	if err != nil {
		return Role{}, err
	}
	if strings.TrimSpace(recipeID) == "" {
		return Role{}, errors.New("a model is required")
	}
	timestamp := now()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO roles(name,recipe_id,created_at,updated_at) VALUES(?,?,?,?)
ON CONFLICT(name) DO UPDATE SET recipe_id=excluded.recipe_id,updated_at=excluded.updated_at`, name, recipeID, timestamp, timestamp); err != nil {
		return Role{}, err
	}
	return s.Role(ctx, name)
}

func (s *Store) Role(ctx context.Context, name string) (Role, error) {
	name, err := NormalizeRoleName(name)
	if err != nil {
		return Role{}, os.ErrNotExist
	}
	var role Role
	err = s.db.QueryRowContext(ctx, `SELECT name,recipe_id,created_at,updated_at FROM roles WHERE name=?`, name).
		Scan(&role.Name, &role.RecipeID, &role.CreatedAt, &role.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Role{}, os.ErrNotExist
	}
	return role, err
}

// Roles lists every assigned role, oldest first, so the console shows custom
// roles in the order they were added. The order key is rowid, the insertion
// order: see now() for why the timestamp text does not sort chronologically.
// Reassigning a role updates its row in place and keeps its rowid, so a role
// holds its position for its whole life.
func (s *Store) Roles(ctx context.Context) ([]Role, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,recipe_id,created_at,updated_at FROM roles ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Role{}
	for rows.Next() {
		var role Role
		if err := rows.Scan(&role.Name, &role.RecipeID, &role.CreatedAt, &role.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, role)
	}
	return result, rows.Err()
}

func (s *Store) ClearRole(ctx context.Context, name string) error {
	name, err := NormalizeRoleName(name)
	if err != nil {
		return os.ErrNotExist
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM roles WHERE name=?`, name)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

func (s *Store) DeleteModel(ctx context.Context, recipeID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM installed_models WHERE recipe_id=?`, recipeID)
	return err
}

func (s *Store) AcceptLicence(ctx context.Context, recipeID string, version int) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO accepted_licences(recipe_id,recipe_version,accepted_at) VALUES(?,?,?)`, recipeID, version, now())
	return err
}

func (s *Store) LicenceAccepted(ctx context.Context, recipeID string, version int) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM accepted_licences WHERE recipe_id=? AND recipe_version=?`, recipeID, version).Scan(&count)
	return count == 1, err
}

func (s *Store) ConfirmTerritoryEligibility(ctx context.Context, recipeID string, version int) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO territory_confirmations(recipe_id,recipe_version,confirmed_at) VALUES(?,?,?)`, recipeID, version, now())
	return err
}

func (s *Store) TerritoryEligibilityConfirmed(ctx context.Context, recipeID string, version int) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM territory_confirmations WHERE recipe_id=? AND recipe_version=?`, recipeID, version).Scan(&count)
	return count == 1, err
}

// Generation is one media generation: what was asked for, what happened to
// it, and where the result is. The seed is recorded whether the user chose it
// or basement did, so any result in the gallery can be made again.
//
// Prompt is stored in full. It is the user's own text on the user's own
// machine, and a gallery that shows a truncated prompt beside a clip is a
// gallery you cannot reproduce anything from.
type Generation struct {
	ID       string `json:"id"`
	RecipeID string `json:"recipe_id"`
	Mode     string `json:"mode"`
	// FirstFrame is the staged media id an image_to_video generation was made
	// from. Empty for text_to_video, which never has one.
	FirstFrame string `json:"first_frame,omitempty"`
	Prompt     string `json:"prompt"`
	Blocks     int    `json:"blocks"`
	ShortEdge  int    `json:"short_edge"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Frames     int    `json:"frames"`
	Seed       int64  `json:"seed"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	// OutputPath is the file on this machine. It is absent until the
	// generation finishes and is never a path outside the data directory.
	OutputPath    string `json:"output_path,omitempty"`
	Bytes         int64  `json:"bytes,omitempty"`
	ProgressValue int64  `json:"progress_value,omitempty"`
	ProgressMax   int64  `json:"progress_max,omitempty"`
	ProgressPhase string `json:"progress_phase,omitempty"`
	CreatedAt     string `json:"created_at"`
	StartedAt     string `json:"started_at,omitempty"`
	FinishedAt    string `json:"finished_at,omitempty"`
}

const generationColumns = `id,recipe_id,mode,first_frame,prompt,blocks,short_edge,width,height,frames,seed,status,error,output_path,bytes,progress_value,progress_max,progress_phase,created_at,started_at,finished_at`

func scanGeneration(row interface{ Scan(...any) error }) (Generation, error) {
	var g Generation
	err := row.Scan(&g.ID, &g.RecipeID, &g.Mode, &g.FirstFrame, &g.Prompt, &g.Blocks, &g.ShortEdge, &g.Width, &g.Height, &g.Frames, &g.Seed,
		&g.Status, &g.Error, &g.OutputPath, &g.Bytes, &g.ProgressValue, &g.ProgressMax, &g.ProgressPhase, &g.CreatedAt, &g.StartedAt, &g.FinishedAt)
	return g, err
}

// CreateGeneration records a request before anything runs, so a generation
// exists on disk from the moment it is accepted rather than from the moment
// it starts. The caller supplies everything except the id and the timestamps.
func (s *Store) CreateGeneration(ctx context.Context, g Generation) (Generation, error) {
	id, err := randomID("gen_")
	if err != nil {
		return Generation{}, err
	}
	g.ID = id
	g.Status = "queued"
	g.CreatedAt = now()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO generations(`+generationColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.RecipeID, g.Mode, g.FirstFrame, g.Prompt, g.Blocks, g.ShortEdge, g.Width, g.Height, g.Frames, g.Seed,
		g.Status, "", "", 0, 0, 0, "", g.CreatedAt, "", ""); err != nil {
		return Generation{}, err
	}
	return g, nil
}

func (s *Store) Generation(ctx context.Context, id string) (Generation, error) {
	g, err := scanGeneration(s.db.QueryRowContext(ctx, `SELECT `+generationColumns+` FROM generations WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Generation{}, os.ErrNotExist
	}
	return g, err
}

// Generations lists results newest first, which is the order the gallery
// reads them in. The order key is rowid, the insertion order, not
// created_at: the timestamp text trims trailing zeros (RFC3339Nano), so
// one value can be a prefix of another and the text comparison puts it
// on the wrong side. Rows are inserted once, at creation, so the
// insertion order is the creation order.
func (s *Store) Generations(ctx context.Context, limit int) ([]Generation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+generationColumns+` FROM generations ORDER BY rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Generation{}
	for rows.Next() {
		g, err := scanGeneration(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, g)
	}
	return result, rows.Err()
}

// StartGeneration marks a queued generation as running. It refuses a
// generation that is not queued, so a cancelled or deleted one can never be
// picked up by a worker that was already holding its id.
func (s *Store) StartGeneration(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE generations SET status='running', started_at=? WHERE id=? AND status='queued'`, now(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

// UpdateGenerationProgress persists only values ComfyUI actually reported.
// max zero clears determinate progress for a newly executing node; it never
// means zero percent. The row must still be running, so a late websocket event
// cannot rewrite a generation after /history made it terminal.
func (s *Store) UpdateGenerationProgress(ctx context.Context, id string, value, max int64, phase string) error {
	if value < 0 || max < 0 || (max == 0 && value != 0) || (max > 0 && value > max) {
		return errors.New("generation progress is outside its reported range")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE generations SET progress_value=?, progress_max=?, progress_phase=? WHERE id=? AND status='running'`, value, max, phase, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

func (s *Store) CompleteGeneration(ctx context.Context, id, outputPath string, bytes int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE generations SET status='completed', output_path=?, bytes=?, error='', finished_at=? WHERE id=?`, outputPath, bytes, now(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

// FailGeneration records why a generation did not produce anything. status is
// the terminal state to record: "failed" for a run that went wrong and
// "cancelled" for one the owner stopped, because those read differently in a
// gallery and only one of them is worth retrying.
func (s *Store) FailGeneration(ctx context.Context, id, status, message string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE generations SET status=?, error=?, finished_at=? WHERE id=? AND status NOT IN ('completed','failed','cancelled')`, status, message, now(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

func (s *Store) DeleteGeneration(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM generations WHERE id=?`, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

// GenerationReferencesFirstFrame reports whether any generation, whatever its
// status, still names id as its first_frame. It is how the staged-media sweep
// tells a source image nothing needs any more from one a completed or running
// generation was actually made from.
func (s *Store) GenerationReferencesFirstFrame(ctx context.Context, id string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM generations WHERE first_frame=?`, id).Scan(&count)
	return count > 0, err
}

func randomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// now is the one clock every stored timestamp comes from. RFC3339Nano removes
// trailing zeros, so one value can be a text prefix of another and the raw
// text does not sort chronologically: the 'Z' byte is above every digit, which
// puts "10:00:00.5Z" after the later "10:00:00.51Z".
//
// A time-ordered query therefore uses one of two keys. Never order by a raw
// timestamp column.
//
//   - rowid, when the query means the order in which the rows arrived. This is
//     the usual case, and it is the correct key for a listing of records that
//     get their timestamp once, at creation.
//   - rtrim(<column>,'Z'), when a timestamp column really is the key. What
//     stays after the 'Z' compares chronologically, because everything before
//     the fraction is fixed width and a shorter fraction is a prefix of a
//     longer one. Use this when the moment is not the insertion moment, as in
//     Models with updated_at, or when rows from two tables must be ranked
//     together, as in ActiveUpdateBlocker, where no rowid can compare them.
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
