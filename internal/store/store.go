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
);`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	return s.migratePeersSingleton()
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

func (s *Store) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
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

func (s *Store) BeginSwitch(ctx context.Context, previousRecipeID, targetRecipeID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	timestamp := now()
	result, err := tx.ExecContext(ctx, `UPDATE installed_models SET status='switching',active=1,updated_at=? WHERE recipe_id=?`, timestamp, previousRecipeID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
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

func (s *Store) Models(ctx context.Context) ([]InstalledModel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+modelColumns+` FROM installed_models m LEFT JOIN model_metrics x ON x.recipe_id=m.recipe_id ORDER BY m.updated_at DESC`)
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

func (s *Store) APIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,created_at,last_used_at FROM api_keys WHERE revoked_at='' ORDER BY created_at`)
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
// credential just by forwarding this result.
func (s *Store) Peers(ctx context.Context) ([]Peer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,base_url,created_at FROM peers ORDER BY created_at`)
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

func randomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
