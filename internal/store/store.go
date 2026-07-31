package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
);`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate database: %w", err)
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

func (s *Store) Models(ctx context.Context) ([]InstalledModel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT recipe_id,recipe_version,status,artifact_path,container_id,active,updated_at FROM installed_models ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []InstalledModel{}
	for rows.Next() {
		var model InstalledModel
		if err := rows.Scan(&model.RecipeID, &model.RecipeVersion, &model.Status, &model.ArtifactPath, &model.ContainerID, &model.Active, &model.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, model)
	}
	return result, rows.Err()
}

func (s *Store) Model(ctx context.Context, recipeID string) (InstalledModel, error) {
	var m InstalledModel
	err := s.db.QueryRowContext(ctx, `SELECT recipe_id,recipe_version,status,artifact_path,container_id,active,updated_at FROM installed_models WHERE recipe_id=?`, recipeID).
		Scan(&m.RecipeID, &m.RecipeVersion, &m.Status, &m.ArtifactPath, &m.ContainerID, &m.Active, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return InstalledModel{}, os.ErrNotExist
	}
	return m, err
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
