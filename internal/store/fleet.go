package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

const MaxFleetNodes = 4

var (
	ErrFleetFull                = errors.New("this fleet already has four nodes")
	ErrHeartbeatReplay          = errors.New("heartbeat sequence was already received")
	ErrReservationConflict      = errors.New("this node's requested resources are already reserved")
	ErrReservationRetryConflict = errors.New("the reservation id was retried with different details")
)

type NodeIdentity struct {
	NodeID                 string
	PublicKey              []byte
	CertificateFingerprint string
	CreatedAt              string
}

type FleetConfig struct {
	FleetID               string
	Role                  string
	ControllerNodeID      string
	ControllerConsoleURL  string
	ControllerNodeURL     string
	ControllerCertificate []byte
	MembershipEpoch       int64
	JoinedAt              string
	MigrationState        string
	HeartbeatSequence     int64
}

type FleetNode struct {
	FleetID              string `json:"fleet_id"`
	NodeID               string `json:"node_id"`
	DisplayName          string `json:"display_name"`
	ConsoleURL           string `json:"console_url"`
	NodeURL              string `json:"node_url"`
	Certificate          []byte `json:"-"`
	ManagerVersion       string `json:"manager_version"`
	ManagerBuildIdentity string `json:"manager_build_identity"`
	CatalogueDigest      string `json:"catalogue_digest"`
	MembershipState      string `json:"membership_state"`
	HeartbeatSequence    int64  `json:"heartbeat_sequence"`
	HeartbeatReceivedAt  string `json:"heartbeat_received_at"`
	HeartbeatPayload     []byte `json:"-"`
	HeartbeatSignature   []byte `json:"-"`
	LegacyPeerID         string `json:"legacy_peer_id,omitempty"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
}

type LegacyDeploymentCandidate struct {
	RecipeID          string
	RecipeVersion     int
	RecipeFingerprint string
	TopologyCount     int
}

type FleetDeployment struct {
	DeploymentID      string                `json:"deployment_id"`
	RecipeID          string                `json:"recipe_id"`
	RecipeVersion     int                   `json:"recipe_version"`
	RecipeFingerprint string                `json:"recipe_fingerprint"`
	TopologyCount     int                   `json:"topology_count"`
	OwnerNodeID       string                `json:"owner_node_id"`
	OwnerJobID        string                `json:"owner_job_id"`
	State             string                `json:"state"`
	LastObservedAt    string                `json:"last_observed_at"`
	CreatedAt         string                `json:"created_at"`
	UpdatedAt         string                `json:"updated_at"`
	Nodes             []FleetDeploymentNode `json:"nodes"`
}

type FleetDeploymentNode struct {
	DeploymentID    string `json:"deployment_id"`
	NodeID          string `json:"node_id"`
	NodeRole        string `json:"node_role"`
	Rank            int    `json:"rank"`
	ReservationID   string `json:"reservation_id"`
	FabricInterface string `json:"fabric_interface,omitempty"`
}

type PendingFleetJoin struct {
	PrepareTokenHash                 string
	FleetID                          string
	ControllerNodeID                 string
	ControllerConsoleURL             string
	ControllerNodeURL                string
	ControllerCertificate            []byte
	ControllerCertificateFingerprint string
	MembershipEpoch                  int64
	ExpiresAt                        string
}

// NodeReservation is one node's durable admission record. ClaimsJSON and
// GrantJSON stay opaque in the store because their versioned wire structs
// belong to internal/fleet. The store owns transition atomicity and exact
// retry comparison without interpreting a controller-signed document.
type NodeReservation struct {
	ReservationID     string
	DeploymentID      string
	FleetID           string
	ControllerNodeID  string
	DriverNodeID      string
	RecipeID          string
	RecipeVersion     int
	RecipeFingerprint string
	State             string
	JobID             string
	ClaimsJSON        []byte
	PrepareTokenHash  string
	GrantJSON         []byte
	ExpiresAt         string
	CreatedAt         string
	UpdatedAt         string
}

// EnsureNodeIdentity couples the protected key file to its public database
// row. A mismatch is never repaired by overwriting either side: it means the
// database and data directory came from different nodes, and silently choosing
// one would let an address or copied store replace durable identity.
func (s *Store) EnsureNodeIdentity(ctx context.Context, identity NodeIdentity) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current NodeIdentity
	err = tx.QueryRowContext(ctx, `SELECT node_id,public_key,certificate_fingerprint,created_at FROM node_identity WHERE singleton=1`).
		Scan(&current.NodeID, &current.PublicKey, &current.CertificateFingerprint, &current.CreatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_identity(singleton,node_id,public_key,certificate_fingerprint,created_at) VALUES(1,?,?,?,?)`,
			identity.NodeID, identity.PublicKey, identity.CertificateFingerprint, identity.CreatedAt); err != nil {
			return fmt.Errorf("record node identity: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read node identity: %w", err)
	case current.NodeID != identity.NodeID || !bytes.Equal(current.PublicKey, identity.PublicKey) || current.CertificateFingerprint != identity.CertificateFingerprint:
		return errors.New("the fleet identity key does not match this manager database")
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fleet_config(singleton,role,migration_state) VALUES(1,'standalone','ready')`); err != nil {
		return fmt.Errorf("initialize fleet configuration: %w", err)
	}
	return tx.Commit()
}

func (s *Store) LocalNodeIdentity(ctx context.Context) (NodeIdentity, error) {
	var identity NodeIdentity
	err := s.db.QueryRowContext(ctx, `SELECT node_id,public_key,certificate_fingerprint,created_at FROM node_identity WHERE singleton=1`).
		Scan(&identity.NodeID, &identity.PublicKey, &identity.CertificateFingerprint, &identity.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeIdentity{}, os.ErrNotExist
	}
	return identity, err
}

// InitializeFleetMigration records only facts available on this manager. A
// legacy peer becomes a placeholder and an active distributed model becomes
// a candidate. Neither is treated as authenticated membership or a proven
// worker rank. The old peer row and credential remain untouched for rollback
// and for the existing two-node executor.
func (s *Store) InitializeFleetMigration(ctx context.Context, self FleetNode, candidates []LegacyDeploymentCandidate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var config FleetConfig
	if err := scanFleetConfig(tx.QueryRowContext(ctx, fleetConfigSelect), &config); err != nil {
		return err
	}
	if config.Role == "member" {
		return tx.Commit()
	}
	if config.FleetID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_config SET controller_console_url=?,controller_node_url=?,controller_certificate=? WHERE singleton=1 AND role='controller'`, self.ConsoleURL, self.NodeURL, self.Certificate); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_nodes SET display_name=?,console_url=?,node_url=?,certificate=?,manager_version=?,manager_build_identity=?,catalogue_digest=?,updated_at=? WHERE fleet_id=? AND node_id=?`,
			self.DisplayName, self.ConsoleURL, self.NodeURL, self.Certificate, self.ManagerVersion, self.ManagerBuildIdentity, self.CatalogueDigest, now(), config.FleetID, self.NodeID); err != nil {
			return err
		}
		return tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,name,base_url,created_at FROM peers ORDER BY created_at`)
	if err != nil {
		return fmt.Errorf("read legacy peers: %w", err)
	}
	var peers []Peer
	for rows.Next() {
		var peer Peer
		if err := rows.Scan(&peer.ID, &peer.Name, &peer.BaseURL, &peer.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		peers = append(peers, peer)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	switch len(peers) {
	case 0:
		_, err = tx.ExecContext(ctx, `UPDATE fleet_config SET migration_state='ready' WHERE singleton=1`)
		if err != nil {
			return err
		}
		return tx.Commit()
	case 1:
		fleetID, err := randomID("fleet_")
		if err != nil {
			return err
		}
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_config SET fleet_id=?,role='controller',controller_node_id=?,controller_console_url=?,controller_node_url=?,controller_certificate=?,membership_epoch=1,joined_at=?,migration_state='legacy-pending' WHERE singleton=1`,
			fleetID, self.NodeID, self.ConsoleURL, self.NodeURL, self.Certificate, timestamp); err != nil {
			return err
		}
		self.FleetID, self.MembershipState = fleetID, "active"
		if err := insertFleetNode(ctx, tx, self); err != nil {
			return err
		}
		peer := peers[0]
		legacyNodeID := "legacy_" + peer.ID
		legacy := FleetNode{FleetID: fleetID, NodeID: legacyNodeID, DisplayName: peer.Name, ConsoleURL: peer.BaseURL,
			MembershipState: "legacy-pending", LegacyPeerID: peer.ID, CreatedAt: peer.CreatedAt, UpdatedAt: timestamp}
		if err := insertFleetNode(ctx, tx, legacy); err != nil {
			return err
		}
		for _, candidate := range candidates {
			deploymentID, err := randomID("legacy_deployment_")
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO fleet_deployments(deployment_id,recipe_id,recipe_version,recipe_fingerprint,topology_count,owner_node_id,state,created_at,updated_at) VALUES(?,?,?,?,?,?, 'legacy-candidate',?,?)`,
				deploymentID, candidate.RecipeID, candidate.RecipeVersion, candidate.RecipeFingerprint, candidate.TopologyCount, self.NodeID, timestamp, timestamp); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO fleet_deployment_nodes(deployment_id,node_id,node_role,rank) VALUES(?,?, 'head',0),(?,?, 'worker',1)`,
				deploymentID, self.NodeID, deploymentID, legacyNodeID); err != nil {
				return err
			}
		}
		return tx.Commit()
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_config SET migration_state='repair-required' WHERE singleton=1`); err != nil {
			return err
		}
		return tx.Commit()
	}
}

const fleetConfigSelect = `SELECT fleet_id,role,controller_node_id,controller_console_url,controller_node_url,controller_certificate,membership_epoch,joined_at,migration_state,outbound_heartbeat_sequence FROM fleet_config WHERE singleton=1`

type rowScanner interface{ Scan(...any) error }

func scanFleetConfig(row rowScanner, config *FleetConfig) error {
	return row.Scan(&config.FleetID, &config.Role, &config.ControllerNodeID, &config.ControllerConsoleURL,
		&config.ControllerNodeURL, &config.ControllerCertificate, &config.MembershipEpoch, &config.JoinedAt,
		&config.MigrationState, &config.HeartbeatSequence)
}

func (s *Store) FleetConfig(ctx context.Context) (FleetConfig, error) {
	var config FleetConfig
	if err := scanFleetConfig(s.db.QueryRowContext(ctx, fleetConfigSelect), &config); err != nil {
		return FleetConfig{}, err
	}
	return config, nil
}

func insertFleetNode(ctx context.Context, tx *sql.Tx, node FleetNode) error {
	timestamp := now()
	if node.CreatedAt == "" {
		node.CreatedAt = timestamp
	}
	if node.UpdatedAt == "" {
		node.UpdatedAt = timestamp
	}
	if node.Certificate == nil {
		node.Certificate = []byte{}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO fleet_nodes(fleet_id,node_id,display_name,console_url,node_url,certificate,manager_version,manager_build_identity,catalogue_digest,membership_state,legacy_peer_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(fleet_id,node_id) DO NOTHING`,
		node.FleetID, node.NodeID, node.DisplayName, node.ConsoleURL, node.NodeURL, node.Certificate, node.ManagerVersion,
		node.ManagerBuildIdentity, node.CatalogueDigest, node.MembershipState, node.LegacyPeerID, node.CreatedAt, node.UpdatedAt)
	return err
}

func (s *Store) FleetNodes(ctx context.Context) ([]FleetNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT fleet_id,node_id,display_name,console_url,node_url,certificate,manager_version,manager_build_identity,catalogue_digest,membership_state,heartbeat_sequence,heartbeat_received_at,heartbeat_payload,heartbeat_signature,legacy_peer_id,created_at,updated_at FROM fleet_nodes ORDER BY created_at,node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []FleetNode{}
	for rows.Next() {
		var node FleetNode
		if err := rows.Scan(&node.FleetID, &node.NodeID, &node.DisplayName, &node.ConsoleURL, &node.NodeURL, &node.Certificate,
			&node.ManagerVersion, &node.ManagerBuildIdentity, &node.CatalogueDigest, &node.MembershipState, &node.HeartbeatSequence,
			&node.HeartbeatReceivedAt, &node.HeartbeatPayload, &node.HeartbeatSignature, &node.LegacyPeerID, &node.CreatedAt, &node.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

// EnsureFleetController gives an owner-initiated adoption a fleet id before
// it contacts the proposed member. This is not remote admission: it adds only
// the already-local node. The proposed node still needs its one-use code and
// the later capacity transaction before it can appear in fleet_nodes.
func (s *Store) EnsureFleetController(ctx context.Context, self FleetNode) (FleetConfig, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FleetConfig{}, err
	}
	defer tx.Rollback()
	var config FleetConfig
	if err := scanFleetConfig(tx.QueryRowContext(ctx, fleetConfigSelect), &config); err != nil {
		return FleetConfig{}, err
	}
	if config.Role == "member" {
		return FleetConfig{}, errors.New("a fleet member cannot become a controller")
	}
	if config.MigrationState == "repair-required" {
		return FleetConfig{}, errors.New("legacy peer records need repair before this manager can adopt a fleet node")
	}
	if config.FleetID == "" {
		config.FleetID, err = randomID("fleet_")
		if err != nil {
			return FleetConfig{}, err
		}
		config.Role, config.ControllerNodeID, config.MembershipEpoch = "controller", self.NodeID, 1
		config.JoinedAt, config.MigrationState = now(), "ready"
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_config SET fleet_id=?,role='controller',controller_node_id=?,controller_console_url=?,controller_node_url=?,controller_certificate=?,membership_epoch=?,joined_at=?,migration_state='ready' WHERE singleton=1`,
			config.FleetID, self.NodeID, self.ConsoleURL, self.NodeURL, self.Certificate, config.MembershipEpoch, config.JoinedAt); err != nil {
			return FleetConfig{}, err
		}
		self.FleetID, self.MembershipState = config.FleetID, "active"
		if err := insertFleetNode(ctx, tx, self); err != nil {
			return FleetConfig{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_config SET controller_console_url=?,controller_node_url=?,controller_certificate=? WHERE singleton=1 AND role='controller'`, self.ConsoleURL, self.NodeURL, self.Certificate); err != nil {
			return FleetConfig{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_nodes SET display_name=?,console_url=?,node_url=?,certificate=?,manager_version=?,manager_build_identity=?,catalogue_digest=?,updated_at=? WHERE fleet_id=? AND node_id=?`,
			self.DisplayName, self.ConsoleURL, self.NodeURL, self.Certificate, self.ManagerVersion, self.ManagerBuildIdentity, self.CatalogueDigest, now(), config.FleetID, self.NodeID); err != nil {
			return FleetConfig{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return FleetConfig{}, err
	}
	return config, nil
}

// PrepareFleetNode serializes capacity admission in the database transaction.
// A remote node cannot call this method through the fleet listener. Only the
// controller's authenticated browser adoption path can create the row, so a
// discovered, spoofed, or even fully compromised unadopted node cannot join by
// sending heartbeats or presenting a certificate.
func (s *Store) PrepareFleetNode(ctx context.Context, self, node FleetNode) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var config FleetConfig
	if err := scanFleetConfig(tx.QueryRowContext(ctx, fleetConfigSelect), &config); err != nil {
		return "", false, err
	}
	if config.Role == "member" {
		return "", false, errors.New("a fleet member cannot adopt another node")
	}
	if config.MigrationState == "repair-required" {
		return "", false, errors.New("legacy peer records need repair before this manager can adopt a fleet node")
	}
	if config.FleetID == "" {
		config.FleetID, err = randomID("fleet_")
		if err != nil {
			return "", false, err
		}
		config.Role, config.ControllerNodeID, config.MembershipEpoch = "controller", self.NodeID, 1
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_config SET fleet_id=?,role='controller',controller_node_id=?,controller_console_url=?,controller_node_url=?,controller_certificate=?,membership_epoch=?,joined_at=?,migration_state='ready' WHERE singleton=1`,
			config.FleetID, self.NodeID, self.ConsoleURL, self.NodeURL, self.Certificate, config.MembershipEpoch, now()); err != nil {
			return "", false, err
		}
		self.FleetID, self.MembershipState = config.FleetID, "active"
		if err := insertFleetNode(ctx, tx, self); err != nil {
			return "", false, err
		}
	}
	node.FleetID = config.FleetID
	var existing FleetNode
	err = tx.QueryRowContext(ctx, `SELECT node_id,node_url,certificate,membership_state FROM fleet_nodes WHERE fleet_id=? AND node_id=?`, config.FleetID, node.NodeID).
		Scan(&existing.NodeID, &existing.NodeURL, &existing.Certificate, &existing.MembershipState)
	if err == nil {
		if !bytes.Equal(existing.Certificate, node.Certificate) {
			return "", false, errors.New("that node id is already pinned to a different certificate")
		}
		if existing.NodeURL != node.NodeURL {
			return "", false, errors.New("that node id is already enrolled at a different node URL")
		}
		if err := tx.Commit(); err != nil {
			return "", false, err
		}
		return config.FleetID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	var conflictingID string
	err = tx.QueryRowContext(ctx, `SELECT node_id FROM fleet_nodes WHERE fleet_id=? AND node_url=? AND node_url<>''`, config.FleetID, node.NodeURL).Scan(&conflictingID)
	if err == nil {
		return "", false, errors.New("that node URL is already pinned to a different identity")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	var legacyNodeID, legacyPeerID string
	err = tx.QueryRowContext(ctx, `SELECT node_id,legacy_peer_id FROM fleet_nodes WHERE fleet_id=? AND membership_state='legacy-pending' AND console_url=?`, config.FleetID, node.ConsoleURL).
		Scan(&legacyNodeID, &legacyPeerID)
	if err == nil {
		node.LegacyPeerID = legacyPeerID
		if _, err := tx.ExecContext(ctx, `DELETE FROM fleet_nodes WHERE fleet_id=? AND node_id=?`, config.FleetID, legacyNodeID); err != nil {
			return "", false, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM fleet_nodes WHERE fleet_id=?`, config.FleetID).Scan(&count); err != nil {
		return "", false, err
	}
	if count >= MaxFleetNodes {
		return "", false, ErrFleetFull
	}
	node.MembershipState = "adopting"
	if err := insertFleetNode(ctx, tx, node); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return config.FleetID, false, nil
}

func (s *Store) CommitFleetNode(ctx context.Context, fleetID, nodeID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE fleet_nodes SET membership_state='active',updated_at=? WHERE fleet_id=? AND node_id=? AND membership_state IN ('adopting','adoption-uncertain')`, now(), fleetID, nodeID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

func (s *Store) MarkFleetNodeAdoptionUncertain(ctx context.Context, fleetID, nodeID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE fleet_nodes SET membership_state='adoption-uncertain',updated_at=? WHERE fleet_id=? AND node_id=? AND membership_state='adopting'`, now(), fleetID, nodeID)
	return err
}

func (s *Store) AbortFleetNode(ctx context.Context, fleetID, nodeID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM fleet_nodes WHERE fleet_id=? AND node_id=? AND membership_state='adopting'`, fleetID, nodeID)
	return err
}

func (s *Store) CreateFleetJoinCode(ctx context.Context, codeHash string, expires time.Time) error {
	config, err := s.FleetConfig(ctx)
	if err != nil {
		return err
	}
	if config.Role != "standalone" {
		return errors.New("this node already belongs to a fleet")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO fleet_join_codes(code_hash,expires_at,created_at) VALUES(?,?,?)`, codeHash, expires.UTC().Format(time.RFC3339Nano), now())
	return err
}

func (s *Store) HasOpenFleetJoinCode(ctx context.Context, at time.Time) (bool, error) {
	var present int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fleet_join_codes WHERE consumed_at='' AND expires_at>?)`, at.UTC().Format(time.RFC3339Nano)).Scan(&present)
	return present == 1, err
}

func (s *Store) PrepareMemberJoin(ctx context.Context, codeHash string, pending PendingFleetJoin, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var config FleetConfig
	if err := scanFleetConfig(tx.QueryRowContext(ctx, fleetConfigSelect), &config); err != nil {
		return err
	}
	if config.Role != "standalone" {
		return errors.New("this node already belongs to a fleet")
	}
	result, err := tx.ExecContext(ctx, `UPDATE fleet_join_codes SET consumed_at=? WHERE code_hash=? AND consumed_at='' AND expires_at>?`, now(), codeHash, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("the fleet join code is invalid, expired, or already used")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO fleet_pending_joins(prepare_token_hash,fleet_id,controller_node_id,controller_console_url,controller_node_url,controller_certificate,controller_certificate_fingerprint,membership_epoch,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		pending.PrepareTokenHash, pending.FleetID, pending.ControllerNodeID, pending.ControllerConsoleURL, pending.ControllerNodeURL,
		pending.ControllerCertificate, pending.ControllerCertificateFingerprint, pending.MembershipEpoch, pending.ExpiresAt, now())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommitMemberJoin(ctx context.Context, prepareTokenHash string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pending PendingFleetJoin
	err = tx.QueryRowContext(ctx, `SELECT prepare_token_hash,fleet_id,controller_node_id,controller_console_url,controller_node_url,controller_certificate,controller_certificate_fingerprint,membership_epoch,expires_at FROM fleet_pending_joins WHERE prepare_token_hash=?`, prepareTokenHash).
		Scan(&pending.PrepareTokenHash, &pending.FleetID, &pending.ControllerNodeID, &pending.ControllerConsoleURL, &pending.ControllerNodeURL,
			&pending.ControllerCertificate, &pending.ControllerCertificateFingerprint, &pending.MembershipEpoch, &pending.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("the fleet join preparation is unknown")
	}
	if err != nil {
		return err
	}
	expires, err := time.Parse(time.RFC3339Nano, pending.ExpiresAt)
	if err != nil || !expires.After(at) {
		return errors.New("the fleet join preparation expired")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fleet_config SET fleet_id=?,role='member',controller_node_id=?,controller_console_url=?,controller_node_url=?,controller_certificate=?,membership_epoch=?,joined_at=?,migration_state='ready' WHERE singleton=1 AND role='standalone'`,
		pending.FleetID, pending.ControllerNodeID, pending.ControllerConsoleURL, pending.ControllerNodeURL, pending.ControllerCertificate, pending.MembershipEpoch, now()); err != nil {
		return err
	}
	var role string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM fleet_config WHERE singleton=1`).Scan(&role); err != nil {
		return err
	}
	if role != "member" {
		return errors.New("this node is no longer standalone")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fleet_pending_joins WHERE prepare_token_hash=?`, prepareTokenHash); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AbortMemberJoin(ctx context.Context, prepareTokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM fleet_pending_joins WHERE prepare_token_hash=?`, prepareTokenHash)
	return err
}

func (s *Store) PinnedFleetCertificates(ctx context.Context, at time.Time) ([][]byte, error) {
	config, err := s.FleetConfig(ctx)
	if err != nil {
		return nil, err
	}
	var result [][]byte
	if config.Role == "member" && len(config.ControllerCertificate) > 0 {
		result = append(result, config.ControllerCertificate)
	}
	if config.Role == "controller" {
		rows, err := s.db.QueryContext(ctx, `SELECT certificate FROM fleet_nodes WHERE fleet_id=? AND certificate<>'' AND membership_state IN ('active','adopting','adoption-uncertain')`, config.FleetID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var certificate []byte
			if err := rows.Scan(&certificate); err != nil {
				rows.Close()
				return nil, err
			}
			result = append(result, certificate)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT controller_certificate FROM fleet_pending_joins WHERE expires_at>?`, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var certificate []byte
		if err := rows.Scan(&certificate); err != nil {
			return nil, err
		}
		result = append(result, certificate)
	}
	return result, rows.Err()
}

func (s *Store) NextHeartbeatSequence(ctx context.Context) (int64, error) {
	var sequence int64
	err := s.db.QueryRowContext(ctx, `UPDATE fleet_config SET outbound_heartbeat_sequence=outbound_heartbeat_sequence+1 WHERE singleton=1 RETURNING outbound_heartbeat_sequence`).Scan(&sequence)
	return sequence, err
}

func (s *Store) AcceptHeartbeat(ctx context.Context, fleetID, nodeID string, sequence int64, receivedAt string, payload, signature []byte, managerVersion, buildIdentity, catalogueDigest string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE fleet_nodes SET heartbeat_sequence=?,heartbeat_received_at=?,heartbeat_payload=?,heartbeat_signature=?,manager_version=?,manager_build_identity=?,catalogue_digest=?,updated_at=? WHERE fleet_id=? AND node_id=? AND membership_state='active' AND heartbeat_sequence<?`,
		sequence, receivedAt, payload, signature, managerVersion, buildIdentity, catalogueDigest, now(), fleetID, nodeID, sequence)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		return nil
	}
	var current int64
	err = s.db.QueryRowContext(ctx, `SELECT heartbeat_sequence FROM fleet_nodes WHERE fleet_id=? AND node_id=? AND membership_state='active'`, fleetID, nodeID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("the heartbeat node is not an adopted fleet member")
	}
	if err != nil {
		return err
	}
	return ErrHeartbeatReplay
}

func (s *Store) UpdateLocalFleetNode(ctx context.Context, node FleetNode, payload, signature []byte, sequence int64, receivedAt string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE fleet_nodes SET display_name=?,console_url=?,node_url=?,manager_version=?,manager_build_identity=?,catalogue_digest=?,heartbeat_sequence=?,heartbeat_received_at=?,heartbeat_payload=?,heartbeat_signature=?,updated_at=? WHERE fleet_id=? AND node_id=?`,
		node.DisplayName, node.ConsoleURL, node.NodeURL, node.ManagerVersion, node.ManagerBuildIdentity, node.CatalogueDigest,
		sequence, receivedAt, payload, signature, now(), node.FleetID, node.NodeID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

const fleetDeploymentSelect = `SELECT deployment_id,recipe_id,recipe_version,recipe_fingerprint,topology_count,owner_node_id,owner_job_id,state,last_observed_at,created_at,updated_at FROM fleet_deployments`

func scanFleetDeployment(row rowScanner, deployment *FleetDeployment) error {
	return row.Scan(&deployment.DeploymentID, &deployment.RecipeID, &deployment.RecipeVersion,
		&deployment.RecipeFingerprint, &deployment.TopologyCount, &deployment.OwnerNodeID,
		&deployment.OwnerJobID, &deployment.State, &deployment.LastObservedAt,
		&deployment.CreatedAt, &deployment.UpdatedAt)
}

func (s *Store) CreateFleetDeployment(ctx context.Context, deployment FleetDeployment, node FleetDeploymentNode) (FleetDeployment, bool, error) {
	if deployment.DeploymentID == "" || deployment.RecipeID == "" || deployment.RecipeVersion <= 0 || deployment.TopologyCount != 1 || deployment.OwnerNodeID == "" {
		return FleetDeployment{}, false, errors.New("an independent deployment requires an id, exact recipe, and owner node")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FleetDeployment{}, false, err
	}
	defer tx.Rollback()
	timestamp := now()
	state := deployment.State
	if state == "" {
		state = "preparing"
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fleet_deployments(deployment_id,recipe_id,recipe_version,recipe_fingerprint,topology_count,owner_node_id,owner_job_id,state,last_observed_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		deployment.DeploymentID, deployment.RecipeID, deployment.RecipeVersion, deployment.RecipeFingerprint,
		deployment.TopologyCount, deployment.OwnerNodeID, deployment.OwnerJobID, state, deployment.LastObservedAt, timestamp, timestamp)
	if err != nil {
		return FleetDeployment{}, false, err
	}
	count, _ := result.RowsAffected()
	var stored FleetDeployment
	if err := scanFleetDeployment(tx.QueryRowContext(ctx, fleetDeploymentSelect+` WHERE deployment_id=?`, deployment.DeploymentID), &stored); err != nil {
		return FleetDeployment{}, false, err
	}
	if count == 0 && (stored.RecipeID != deployment.RecipeID || stored.RecipeVersion != deployment.RecipeVersion ||
		stored.RecipeFingerprint != deployment.RecipeFingerprint || stored.TopologyCount != deployment.TopologyCount ||
		stored.OwnerNodeID != deployment.OwnerNodeID) {
		return FleetDeployment{}, false, errors.New("the deployment id was retried with different placement details")
	}
	if count == 1 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO fleet_deployment_nodes(deployment_id,node_id,node_role,rank,reservation_id,fabric_interface) VALUES(?,?, 'independent',0,?, '')`, deployment.DeploymentID, node.NodeID, node.ReservationID); err != nil {
			return FleetDeployment{}, false, err
		}
	} else {
		var storedNodeID, storedReservationID string
		if err := tx.QueryRowContext(ctx, `SELECT node_id,reservation_id FROM fleet_deployment_nodes WHERE deployment_id=? AND node_role='independent' AND rank=0`, deployment.DeploymentID).
			Scan(&storedNodeID, &storedReservationID); err != nil {
			return FleetDeployment{}, false, err
		}
		if storedNodeID != node.NodeID || storedReservationID != node.ReservationID {
			return FleetDeployment{}, false, errors.New("the deployment id was retried with a different reservation")
		}
	}
	if err := tx.Commit(); err != nil {
		return FleetDeployment{}, false, err
	}
	stored.Nodes = []FleetDeploymentNode{{DeploymentID: stored.DeploymentID, NodeID: node.NodeID, NodeRole: "independent", Rank: 0, ReservationID: node.ReservationID}}
	return stored, count == 1, nil
}

func (s *Store) SetFleetDeploymentJob(ctx context.Context, deploymentID, ownerJobID, state, observedAt string) error {
	if ownerJobID == "" {
		return errors.New("the deployment owner job id is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE fleet_deployments SET owner_job_id=CASE WHEN owner_job_id='' THEN ? ELSE owner_job_id END,state=?,last_observed_at=?,updated_at=? WHERE deployment_id=? AND (owner_job_id='' OR owner_job_id=?)`,
		ownerJobID, state, observedAt, now(), deploymentID, ownerJobID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("the deployment already points to a different owner job")
	}
	return nil
}

// AdvanceFleetDeploymentJob moves the controller projection from one
// target-owned lifecycle job to its next one. The expected id keeps two
// browser actions from silently overwriting each other's remote progress.
func (s *Store) AdvanceFleetDeploymentJob(ctx context.Context, deploymentID, expectedJobID, ownerJobID, state, observedAt string) error {
	if deploymentID == "" || expectedJobID == "" || ownerJobID == "" {
		return errors.New("deployment and expected target job ids are required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE fleet_deployments SET owner_job_id=?,state=?,last_observed_at=?,updated_at=? WHERE deployment_id=? AND owner_job_id=?`,
		ownerJobID, state, observedAt, now(), deploymentID, expectedJobID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("the deployment target job changed before this action was recorded")
	}
	return nil
}

func (s *Store) ObserveFleetDeployment(ctx context.Context, deploymentID, state, observedAt string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE fleet_deployments SET state=?,last_observed_at=?,updated_at=? WHERE deployment_id=?`, state, observedAt, now(), deploymentID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return os.ErrNotExist
	}
	return nil
}

func (s *Store) FleetDeployment(ctx context.Context, deploymentID string) (FleetDeployment, error) {
	var deployment FleetDeployment
	if err := scanFleetDeployment(s.db.QueryRowContext(ctx, fleetDeploymentSelect+` WHERE deployment_id=?`, deploymentID), &deployment); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FleetDeployment{}, os.ErrNotExist
		}
		return FleetDeployment{}, err
	}
	nodes, err := s.fleetDeploymentNodes(ctx, deploymentID)
	if err != nil {
		return FleetDeployment{}, err
	}
	deployment.Nodes = nodes
	return deployment, nil
}

func (s *Store) FleetDeployments(ctx context.Context) ([]FleetDeployment, error) {
	rows, err := s.db.QueryContext(ctx, fleetDeploymentSelect+` ORDER BY created_at DESC,deployment_id`)
	if err != nil {
		return nil, err
	}
	result := []FleetDeployment{}
	for rows.Next() {
		var deployment FleetDeployment
		if err := scanFleetDeployment(rows, &deployment); err != nil {
			return nil, err
		}
		result = append(result, deployment)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range result {
		result[index].Nodes, err = s.fleetDeploymentNodes(ctx, result[index].DeploymentID)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) fleetDeploymentNodes(ctx context.Context, deploymentID string) ([]FleetDeploymentNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT deployment_id,node_id,node_role,rank,reservation_id,fabric_interface FROM fleet_deployment_nodes WHERE deployment_id=? ORDER BY rank,node_id`, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []FleetDeploymentNode{}
	for rows.Next() {
		var node FleetDeploymentNode
		if err := rows.Scan(&node.DeploymentID, &node.NodeID, &node.NodeRole, &node.Rank, &node.ReservationID, &node.FabricInterface); err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

const nodeReservationSelect = `SELECT reservation_id,deployment_id,fleet_id,controller_node_id,driver_node_id,recipe_id,recipe_version,recipe_fingerprint,state,job_id,claims_json,prepare_token_hash,grant_json,expires_at,created_at,updated_at FROM node_reservations`

func scanNodeReservation(row rowScanner, reservation *NodeReservation) error {
	return row.Scan(&reservation.ReservationID, &reservation.DeploymentID, &reservation.FleetID,
		&reservation.ControllerNodeID, &reservation.DriverNodeID, &reservation.RecipeID,
		&reservation.RecipeVersion, &reservation.RecipeFingerprint, &reservation.State,
		&reservation.JobID, &reservation.ClaimsJSON, &reservation.PrepareTokenHash, &reservation.GrantJSON,
		&reservation.ExpiresAt, &reservation.CreatedAt, &reservation.UpdatedAt)
}

// PrepareNodeReservation writes admission before a caller can acknowledge it.
// A repeated byte-identical request returns the existing row in any state so
// the protocol can answer a lost response without creating a second claim.
// Reusing the id for different authority or resources is always a conflict.
func (s *Store) PrepareNodeReservation(ctx context.Context, reservation NodeReservation) (NodeReservation, bool, error) {
	if reservation.ReservationID == "" || reservation.DeploymentID == "" || reservation.RecipeID == "" || reservation.PrepareTokenHash == "" || len(reservation.ClaimsJSON) == 0 {
		return NodeReservation{}, false, errors.New("reservation identity, recipe, claims, and preparation token are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeReservation{}, false, err
	}
	defer tx.Rollback()
	var maintenanceID string
	err = tx.QueryRowContext(ctx, `SELECT reservation_id FROM node_reservations WHERE state='maintenance' AND reservation_id<>? LIMIT 1`, reservation.ReservationID).Scan(&maintenanceID)
	if err == nil {
		return NodeReservation{}, false, ErrReservationConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return NodeReservation{}, false, err
	}
	timestamp := now()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO node_reservations(reservation_id,deployment_id,fleet_id,controller_node_id,driver_node_id,recipe_id,recipe_version,recipe_fingerprint,state,claims_json,prepare_token_hash,grant_json,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?, 'prepared',?,?, '',?,?,?)`,
		reservation.ReservationID, reservation.DeploymentID, reservation.FleetID, reservation.ControllerNodeID,
		reservation.DriverNodeID, reservation.RecipeID, reservation.RecipeVersion, reservation.RecipeFingerprint,
		reservation.ClaimsJSON, reservation.PrepareTokenHash, reservation.ExpiresAt, timestamp, timestamp)
	if err != nil {
		return NodeReservation{}, false, err
	}
	count, _ := result.RowsAffected()
	var stored NodeReservation
	if err := scanNodeReservation(tx.QueryRowContext(ctx, nodeReservationSelect+` WHERE reservation_id=?`, reservation.ReservationID), &stored); err != nil {
		return NodeReservation{}, false, err
	}
	if count == 0 && (stored.DeploymentID != reservation.DeploymentID || stored.FleetID != reservation.FleetID ||
		stored.ControllerNodeID != reservation.ControllerNodeID || stored.DriverNodeID != reservation.DriverNodeID ||
		stored.RecipeID != reservation.RecipeID || stored.RecipeVersion != reservation.RecipeVersion ||
		stored.RecipeFingerprint != reservation.RecipeFingerprint || !bytes.Equal(stored.ClaimsJSON, reservation.ClaimsJSON) ||
		stored.PrepareTokenHash != reservation.PrepareTokenHash || stored.ExpiresAt != reservation.ExpiresAt) {
		return NodeReservation{}, false, ErrReservationRetryConflict
	}
	if err := tx.Commit(); err != nil {
		return NodeReservation{}, false, err
	}
	return stored, count == 1, nil
}

func (s *Store) NodeReservation(ctx context.Context, reservationID string) (NodeReservation, error) {
	var reservation NodeReservation
	err := scanNodeReservation(s.db.QueryRowContext(ctx, nodeReservationSelect+` WHERE reservation_id=?`, reservationID), &reservation)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeReservation{}, os.ErrNotExist
	}
	return reservation, err
}

func (s *Store) NodeReservations(ctx context.Context) ([]NodeReservation, error) {
	rows, err := s.db.QueryContext(ctx, nodeReservationSelect+` ORDER BY created_at,reservation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []NodeReservation{}
	for rows.Next() {
		var reservation NodeReservation
		if err := scanNodeReservation(rows, &reservation); err != nil {
			return nil, err
		}
		result = append(result, reservation)
	}
	return result, rows.Err()
}

// CommitNodeReservation records the exact grant without executing recipe
// work. Losing the response is safe because the same grant is idempotent and
// different bytes cannot replace authority that was already committed.
func (s *Store) CommitNodeReservation(ctx context.Context, reservationID, prepareTokenHash string, grant []byte) (NodeReservation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeReservation{}, err
	}
	defer tx.Rollback()
	var reservation NodeReservation
	if err := scanNodeReservation(tx.QueryRowContext(ctx, nodeReservationSelect+` WHERE reservation_id=?`, reservationID), &reservation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodeReservation{}, os.ErrNotExist
		}
		return NodeReservation{}, err
	}
	if reservation.PrepareTokenHash != prepareTokenHash {
		return NodeReservation{}, errors.New("the reservation preparation token does not match")
	}
	switch reservation.State {
	case "prepared":
		if _, err := tx.ExecContext(ctx, `UPDATE node_reservations SET state='committed',grant_json=?,updated_at=? WHERE reservation_id=? AND state='prepared'`, grant, now(), reservationID); err != nil {
			return NodeReservation{}, err
		}
		reservation.State, reservation.GrantJSON = "committed", append([]byte(nil), grant...)
	case "committed", "active":
		if !bytes.Equal(reservation.GrantJSON, grant) {
			return NodeReservation{}, ErrReservationRetryConflict
		}
	case "released", "aborted", "expired":
		return NodeReservation{}, fmt.Errorf("reservation is %s and cannot be committed", reservation.State)
	default:
		return NodeReservation{}, fmt.Errorf("reservation has unknown state %s", reservation.State)
	}
	if err := tx.Commit(); err != nil {
		return NodeReservation{}, err
	}
	return reservation, nil
}

// AttachNodeReservationJob is the boundary between a short-lived committed
// grant and work that must survive restart. The exact job is immutable after
// the first successful write, which keeps a retry from adopting another
// job's capacity claim.
func (s *Store) AttachNodeReservationJob(ctx context.Context, reservationID, jobID string) error {
	if reservationID == "" || jobID == "" {
		return errors.New("reservation and job ids are required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE node_reservations SET job_id=CASE WHEN job_id='' THEN ? ELSE job_id END,updated_at=? WHERE reservation_id=? AND state IN ('committed','active') AND (job_id='' OR job_id=?)`, jobID, now(), reservationID, jobID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		return nil
	}
	reservation, err := s.NodeReservation(ctx, reservationID)
	if err != nil {
		return err
	}
	if reservation.JobID != "" && reservation.JobID != jobID {
		return ErrReservationRetryConflict
	}
	if reservation.JobID == jobID && (reservation.State == "committed" || reservation.State == "active") {
		return nil
	}
	return fmt.Errorf("reservation is %s and cannot be attached to a job", reservation.State)
}

// ActivateNodeReservation transfers this node's one runtime slot to the
// committed claim. replaceRecipeID is the predecessor observed under the
// engine's runtime lock. An empty predecessor grants no replacement authority,
// even when the active claim names the same recipe, because two remote drivers
// can legitimately request the same recipe and must not displace each other.
func (s *Store) ActivateNodeReservation(ctx context.Context, reservationID, replaceRecipeID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var maintenanceID string
	err = tx.QueryRowContext(ctx, `SELECT reservation_id FROM node_reservations WHERE state='maintenance' AND reservation_id<>? LIMIT 1`, reservationID).Scan(&maintenanceID)
	if err == nil {
		return ErrReservationConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var target NodeReservation
	if err := scanNodeReservation(tx.QueryRowContext(ctx, nodeReservationSelect+` WHERE reservation_id=?`, reservationID), &target); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return os.ErrNotExist
		}
		return err
	}
	if target.State == "active" {
		return tx.Commit()
	}
	if target.State != "committed" {
		return fmt.Errorf("reservation is %s and cannot claim the runtime slot", target.State)
	}
	rows, err := tx.QueryContext(ctx, nodeReservationSelect+` WHERE state IN ('active','reclaiming') AND reservation_id<>?`, reservationID)
	if err != nil {
		return err
	}
	var active []NodeReservation
	for rows.Next() {
		var current NodeReservation
		if err := scanNodeReservation(rows, &current); err != nil {
			rows.Close()
			return err
		}
		active = append(active, current)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, current := range active {
		// A reclaiming reservation still owns the runtime until its container
		// has stopped. Letting a replacement release it here would make the
		// bookkeeping free before the process using the port and GPU is gone.
		if current.State == "reclaiming" {
			return ErrReservationConflict
		}
		if replaceRecipeID == "" || current.RecipeID != replaceRecipeID {
			return ErrReservationConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE node_reservations SET state='released',updated_at=? WHERE state='active' AND reservation_id<>?`, now(), reservationID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE node_reservations SET state='active',updated_at=? WHERE reservation_id=? AND state='committed'`, now(), reservationID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrReservationConflict
	}
	return tx.Commit()
}

// ActivateNodeMaintenanceReservation closes runtime admission while allowing
// the currently serving reservation to remain active. Update does not stop a
// model container, but no prepared or future claim may become another runtime
// owner until this maintenance row is released.
func (s *Store) ActivateNodeMaintenanceReservation(ctx context.Context, reservationID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM node_reservations WHERE reservation_id=?`, reservationID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return os.ErrNotExist
		}
		return err
	}
	if state == "maintenance" {
		return tx.Commit()
	}
	if state != "committed" {
		return fmt.Errorf("reservation is %s and cannot claim runtime maintenance", state)
	}
	var other string
	err = tx.QueryRowContext(ctx, `SELECT reservation_id FROM node_reservations WHERE state='maintenance' AND reservation_id<>? LIMIT 1`, reservationID).Scan(&other)
	if err == nil {
		return ErrReservationConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE node_reservations SET state='maintenance',updated_at=? WHERE reservation_id=? AND state='committed'`, now(), reservationID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrReservationConflict
	}
	return tx.Commit()
}

func (s *Store) NodeMaintenanceReservationActive(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_reservations WHERE state='maintenance'`).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) RenewNodeReservation(ctx context.Context, reservationID string, expires time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE node_reservations SET expires_at=?,updated_at=? WHERE reservation_id=? AND state IN ('prepared','committed','active')`, expires.UTC().Format(time.RFC3339Nano), now(), reservationID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("only a prepared, committed, or active reservation can be renewed")
	}
	return nil
}

// BeginNodeReservationReclaim takes ownership of stopping one expired active
// driver rank. The reclaiming state remains a runtime conflict, so another
// claimant cannot enter between this transition and the container stop.
func (s *Store) BeginNodeReservationReclaim(ctx context.Context, reservationID string, at time.Time) (NodeReservation, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE node_reservations SET state='reclaiming',updated_at=? WHERE reservation_id=? AND state='active' AND expires_at<>'' AND expires_at<=?`, now(), reservationID, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return NodeReservation{}, err
	}
	reservation, err := s.NodeReservation(ctx, reservationID)
	if err != nil {
		return NodeReservation{}, err
	}
	if count, _ := result.RowsAffected(); count == 1 || reservation.State == "reclaiming" {
		return reservation, nil
	}
	if reservation.State == "active" {
		return NodeReservation{}, errors.New("the active reservation was renewed before reclaim began")
	}
	return NodeReservation{}, fmt.Errorf("reservation is %s and cannot begin reclaim", reservation.State)
}

// FinishNodeReservationReclaim frees authority only after the caller has
// stopped the process that held it. A failed stop leaves the row reclaiming
// and blocking, so a later sweep can retry without admitting overlapping work.
func (s *Store) FinishNodeReservationReclaim(ctx context.Context, reservationID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE node_reservations SET state='expired',updated_at=? WHERE reservation_id=? AND state='reclaiming'`, now(), reservationID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		return nil
	}
	reservation, err := s.NodeReservation(ctx, reservationID)
	if err != nil {
		return err
	}
	if reservation.State == "expired" {
		return nil
	}
	return fmt.Errorf("reservation is %s and cannot finish reclaim", reservation.State)
}

func (s *Store) AbortNodeReservation(ctx context.Context, reservationID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE node_reservations SET state='aborted',updated_at=? WHERE reservation_id=? AND state IN ('prepared','committed')`, now(), reservationID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		return nil
	}
	reservation, err := s.NodeReservation(ctx, reservationID)
	if err != nil {
		return err
	}
	if reservation.State == "aborted" {
		return nil
	}
	return fmt.Errorf("reservation is %s and cannot be aborted", reservation.State)
}

func (s *Store) ReleaseNodeReservation(ctx context.Context, reservationID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE node_reservations SET state='released',updated_at=? WHERE reservation_id=? AND state IN ('prepared','committed','active','reclaiming','maintenance')`, now(), reservationID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		return nil
	}
	reservation, err := s.NodeReservation(ctx, reservationID)
	if err != nil {
		return err
	}
	if reservation.State == "released" {
		return nil
	}
	return fmt.Errorf("reservation is %s and cannot be released", reservation.State)
}

// DeleteSettledNodeReservation removes a reservation whose life is over, so
// its identity can be prepared again. Recovery reservations use one
// deterministic identity per recipe, and a settled row left under that
// identity would otherwise block startup recovery forever: Prepare returns
// the row unchanged and Activate refuses it. The state guard means a live
// reservation is never deleted, whoever calls this.
func (s *Store) DeleteSettledNodeReservation(ctx context.Context, reservationID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM node_reservations WHERE reservation_id=? AND state IN ('released','expired')`, reservationID)
	return err
}

func (s *Store) ReleaseActiveRecipeReservations(ctx context.Context, recipeID, exceptReservationID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE node_reservations SET state='released',updated_at=? WHERE state='active' AND recipe_id=? AND reservation_id<>?`, now(), recipeID, exceptReservationID)
	return err
}

// ExpireNodeReservations releases only claims that have not started. A job
// attachment proves that execution crossed its durable ownership boundary,
// while an active independent model can keep serving through either manager
// or controller restart. Neither is governed by the prepare deadline.
func (s *Store) ExpireNodeReservations(ctx context.Context, at time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE node_reservations SET state='expired',updated_at=? WHERE state IN ('prepared','committed') AND job_id='' AND expires_at<>'' AND expires_at<=?`, now(), at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
