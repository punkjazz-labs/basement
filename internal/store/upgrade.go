package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// FleetUpgradeRun is the controller-owned journal for one exact signed
// release. Manifest and signature bytes are public release material, but they
// stay out of JSON responses because nodes report the verified identity and
// state instead of echoing large transport inputs back to the console.
type FleetUpgradeRun struct {
	RunID            string             `json:"run_id"`
	FleetID          string             `json:"fleet_id"`
	ControllerNodeID string             `json:"controller_node_id"`
	ReleaseTag       string             `json:"release_tag"`
	TargetVersion    string             `json:"target_version"`
	ManifestSHA256   string             `json:"manifest_sha256"`
	ManifestBytes    []byte             `json:"-"`
	SignatureBytes   []byte             `json:"-"`
	AssetURL         string             `json:"-"`
	State            string             `json:"state"`
	Failure          string             `json:"failure,omitempty"`
	CreatedAt        string             `json:"created_at"`
	UpdatedAt        string             `json:"updated_at"`
	Nodes            []FleetUpgradeNode `json:"nodes"`
}

type FleetUpgradeNode struct {
	RunID          string `json:"run_id"`
	NodeID         string `json:"node_id"`
	DisplayName    string `json:"display_name"`
	Sequence       int    `json:"sequence"`
	Role           string `json:"role"`
	State          string `json:"state"`
	RunningVersion string `json:"running_version"`
	TargetVersion  string `json:"target_version"`
	AttemptID      string `json:"attempt_id,omitempty"`
	Failure        string `json:"failure,omitempty"`
	// ResolveState records what happened when the owner resolved a stopped
	// run: 'resolved' when this node settled its local state and released its
	// maintenance hold, 'unreachable' when the controller could not tell it.
	ResolveState   string `json:"resolve_state,omitempty"`
	ResolveFailure string `json:"resolve_failure,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

const fleetUpgradeRunSelect = `SELECT run_id,fleet_id,controller_node_id,release_tag,target_version,manifest_sha256,manifest_bytes,signature_bytes,asset_url,state,failure,created_at,updated_at FROM fleet_upgrade_runs`

func scanFleetUpgradeRun(row rowScanner, run *FleetUpgradeRun) error {
	return row.Scan(&run.RunID, &run.FleetID, &run.ControllerNodeID, &run.ReleaseTag,
		&run.TargetVersion, &run.ManifestSHA256, &run.ManifestBytes, &run.SignatureBytes,
		&run.AssetURL, &run.State, &run.Failure, &run.CreatedAt, &run.UpdatedAt)
}

func scanFleetUpgradeNode(row rowScanner, node *FleetUpgradeNode) error {
	return row.Scan(&node.RunID, &node.NodeID, &node.DisplayName, &node.Sequence,
		&node.Role, &node.State, &node.RunningVersion, &node.TargetVersion,
		&node.AttemptID, &node.Failure, &node.ResolveState, &node.ResolveFailure, &node.UpdatedAt)
}

// CreateFleetUpgradeRun takes the fleet maintenance latch and records the
// complete restart order before any node downloads or restarts. A retry of the
// same exact run is idempotent, while another active or failed run remains an
// explicit conflict for the owner to resolve. A resolved run is settled
// history, so it no longer blocks a fresh attempt.
func (s *Store) CreateFleetUpgradeRun(ctx context.Context, run FleetUpgradeRun, nodes []FleetUpgradeNode) (FleetUpgradeRun, bool, error) {
	if run.RunID == "" || run.FleetID == "" || run.ControllerNodeID == "" || run.TargetVersion == "" || run.ManifestSHA256 == "" || len(run.ManifestBytes) == 0 || len(run.SignatureBytes) == 0 || run.AssetURL == "" || len(nodes) == 0 {
		return FleetUpgradeRun{}, false, errors.New("fleet upgrade identity, signed release, and nodes are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FleetUpgradeRun{}, false, err
	}
	defer tx.Rollback()
	var activeID string
	// Newest first by rowid, the insertion order: see now() for why the
	// timestamp text does not sort chronologically.
	err = tx.QueryRowContext(ctx, `SELECT run_id FROM fleet_upgrade_runs WHERE state NOT IN ('succeeded','resolved') ORDER BY rowid DESC LIMIT 1`).Scan(&activeID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return FleetUpgradeRun{}, false, err
	}
	if activeID != "" && activeID != run.RunID {
		return FleetUpgradeRun{}, false, errors.New("a fleet upgrade already needs attention")
	}
	timestamp := now()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fleet_upgrade_runs(run_id,fleet_id,controller_node_id,release_tag,target_version,manifest_sha256,manifest_bytes,signature_bytes,asset_url,state,failure,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'staging','',?,?)`,
		run.RunID, run.FleetID, run.ControllerNodeID, run.ReleaseTag, run.TargetVersion,
		run.ManifestSHA256, run.ManifestBytes, run.SignatureBytes, run.AssetURL, timestamp, timestamp)
	if err != nil {
		return FleetUpgradeRun{}, false, err
	}
	count, _ := result.RowsAffected()
	if count == 1 {
		for _, node := range nodes {
			if node.NodeID == "" || node.DisplayName == "" || node.RunningVersion == "" || node.TargetVersion != run.TargetVersion {
				return FleetUpgradeRun{}, false, errors.New("fleet upgrade node identity and versions are required")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO fleet_upgrade_nodes(run_id,node_id,display_name,sequence,role,state,running_version,target_version,updated_at) VALUES(?,?,?,?,?,'pending',?,?,?)`,
				run.RunID, node.NodeID, node.DisplayName, node.Sequence, node.Role,
				node.RunningVersion, node.TargetVersion, timestamp); err != nil {
				return FleetUpgradeRun{}, false, err
			}
		}
	} else {
		var stored FleetUpgradeRun
		if err := scanFleetUpgradeRun(tx.QueryRowContext(ctx, fleetUpgradeRunSelect+` WHERE run_id=?`, run.RunID), &stored); err != nil {
			return FleetUpgradeRun{}, false, err
		}
		if stored.FleetID != run.FleetID || stored.ControllerNodeID != run.ControllerNodeID || stored.ReleaseTag != run.ReleaseTag ||
			stored.TargetVersion != run.TargetVersion || stored.ManifestSHA256 != run.ManifestSHA256 ||
			string(stored.ManifestBytes) != string(run.ManifestBytes) || string(stored.SignatureBytes) != string(run.SignatureBytes) || stored.AssetURL != run.AssetURL {
			return FleetUpgradeRun{}, false, errors.New("the fleet upgrade run was retried with different signed release details")
		}
	}
	if err := tx.Commit(); err != nil {
		return FleetUpgradeRun{}, false, err
	}
	stored, err := s.FleetUpgradeRun(ctx, run.RunID)
	return stored, count == 1, err
}

func (s *Store) FleetUpgradeRun(ctx context.Context, runID string) (FleetUpgradeRun, error) {
	var run FleetUpgradeRun
	if err := scanFleetUpgradeRun(s.db.QueryRowContext(ctx, fleetUpgradeRunSelect+` WHERE run_id=?`, runID), &run); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FleetUpgradeRun{}, os.ErrNotExist
		}
		return FleetUpgradeRun{}, err
	}
	nodes, err := s.fleetUpgradeNodes(ctx, runID)
	if err != nil {
		return FleetUpgradeRun{}, err
	}
	run.Nodes = nodes
	return run, nil
}

// LatestFleetUpgradeRun answers the run the fleet started last. The order key
// is rowid, the insertion order: see now() for why the timestamp text does not
// sort chronologically. A run row is updated in place through its whole life,
// so its rowid still marks the moment the rollout began.
func (s *Store) LatestFleetUpgradeRun(ctx context.Context) (FleetUpgradeRun, error) {
	var run FleetUpgradeRun
	if err := scanFleetUpgradeRun(s.db.QueryRowContext(ctx, fleetUpgradeRunSelect+` ORDER BY rowid DESC LIMIT 1`), &run); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FleetUpgradeRun{}, os.ErrNotExist
		}
		return FleetUpgradeRun{}, err
	}
	nodes, err := s.fleetUpgradeNodes(ctx, run.RunID)
	if err != nil {
		return FleetUpgradeRun{}, err
	}
	run.Nodes = nodes
	return run, nil
}

// ActiveFleetUpgradeRun answers the newest run that still needs attention. The
// order key is rowid, the insertion order, for the same reason as
// LatestFleetUpgradeRun.
func (s *Store) ActiveFleetUpgradeRun(ctx context.Context) (FleetUpgradeRun, error) {
	var run FleetUpgradeRun
	if err := scanFleetUpgradeRun(s.db.QueryRowContext(ctx, fleetUpgradeRunSelect+` WHERE state NOT IN ('succeeded','failed','resolved') ORDER BY rowid DESC LIMIT 1`), &run); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FleetUpgradeRun{}, os.ErrNotExist
		}
		return FleetUpgradeRun{}, err
	}
	nodes, err := s.fleetUpgradeNodes(ctx, run.RunID)
	if err != nil {
		return FleetUpgradeRun{}, err
	}
	run.Nodes = nodes
	return run, nil
}

func (s *Store) fleetUpgradeNodes(ctx context.Context, runID string) ([]FleetUpgradeNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id,node_id,display_name,sequence,role,state,running_version,target_version,attempt_id,failure,resolve_state,resolve_failure,updated_at FROM fleet_upgrade_nodes WHERE run_id=? ORDER BY sequence,node_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []FleetUpgradeNode{}
	for rows.Next() {
		var node FleetUpgradeNode
		if err := scanFleetUpgradeNode(rows, &node); err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

func (s *Store) UpdateFleetUpgradeNode(ctx context.Context, runID, nodeID, state, runningVersion, attemptID, failure string) error {
	if runID == "" || nodeID == "" || state == "" || runningVersion == "" {
		return errors.New("fleet upgrade node state and running version are required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE fleet_upgrade_nodes SET state=?,running_version=?,attempt_id=CASE WHEN ?='' THEN attempt_id ELSE ? END,failure=?,updated_at=? WHERE run_id=? AND node_id=?`,
		state, runningVersion, attemptID, attemptID, failure, now(), runID, nodeID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

func (s *Store) UpdateFleetUpgradeRunState(ctx context.Context, runID, state, failure string) error {
	if runID == "" || state == "" {
		return errors.New("fleet upgrade run and state are required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE fleet_upgrade_runs SET state=?,failure=?,updated_at=? WHERE run_id=?`, state, failure, now(), runID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

// FleetUpgradeMaintenanceActive remains true after a stopped rollout. Version
// skew is still real after the driver stops, so placement and membership stay
// closed until the owner explicitly resolves the run instead of being reopened
// by an error path. A resolved run is that explicit acknowledgment, so it
// counts as settled alongside succeeded.
func (s *Store) FleetUpgradeMaintenanceActive(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fleet_upgrade_runs WHERE state NOT IN ('succeeded','resolved')`).Scan(&count); err != nil {
		return false, fmt.Errorf("read fleet upgrade maintenance: %w", err)
	}
	return count > 0, nil
}

// UpdateFleetUpgradeNodeResolve records the per-node outcome of an owner
// resolve action without touching the node's upgrade state, which stays as the
// history of what actually happened during the rollout.
func (s *Store) UpdateFleetUpgradeNodeResolve(ctx context.Context, runID, nodeID, resolveState, resolveFailure string) error {
	if runID == "" || nodeID == "" || resolveState == "" {
		return errors.New("fleet upgrade node resolve outcome is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE fleet_upgrade_nodes SET resolve_state=?,resolve_failure=?,updated_at=? WHERE run_id=? AND node_id=?`,
		resolveState, resolveFailure, now(), runID, nodeID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}
