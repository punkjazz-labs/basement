package fleet

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/punkjazz-labs/basement/internal/inventory"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

const (
	ProtocolVersion    = 1
	HeartbeatInterval  = 10 * time.Second
	HeartbeatFreshness = 30 * time.Second
	ClockSkewBound     = 2 * time.Minute
)

type ModelSnapshot struct {
	RecipeID      string `json:"recipe_id"`
	RecipeVersion int    `json:"recipe_version"`
	Status        string `json:"status"`
	Active        bool   `json:"active"`
}

// AllocationSnapshot projects durable local authority without exposing a
// preparation token or controller grant. The signed heartbeat reports what
// this node already admitted; it never allocates from the read path.
type AllocationSnapshot struct {
	DeploymentID string `json:"deployment_id"`
	State        string `json:"state"`
}

type HeartbeatPayload struct {
	Version              int                  `json:"version"`
	FleetID              string               `json:"fleet_id"`
	NodeID               string               `json:"node_id"`
	ManagerVersion       string               `json:"manager_version"`
	ManagerBuildIdentity string               `json:"manager_build_identity"`
	Sequence             int64                `json:"sequence"`
	LocalTime            string               `json:"local_time"`
	Inventory            inventory.System     `json:"inventory"`
	InstalledModels      []ModelSnapshot      `json:"installed_models"`
	Allocations          []AllocationSnapshot `json:"allocations"`
	CatalogueDigest      string               `json:"catalogue_digest"`
	// The GPU power mode this node is set to, and the sentence that says the
	// machine did not take it. Carrying both in the heartbeat is what lets the
	// fleet dashboard show every Spark's mode with no extra call: the
	// controller already reads this envelope every ten seconds.
	PowerMode        string `json:"power_mode"`
	PowerModeFailure string `json:"power_mode_failure"`
}

type HeartbeatEnvelope struct {
	Payload   HeartbeatPayload `json:"payload"`
	Signature []byte           `json:"signature"`
}

func (e HeartbeatEnvelope) signedBytes() ([]byte, error) {
	return json.Marshal(e.Payload)
}

func SignHeartbeat(identity *Identity, payload HeartbeatPayload) (HeartbeatEnvelope, error) {
	envelope := HeartbeatEnvelope{Payload: payload}
	canonical, err := envelope.signedBytes()
	if err != nil {
		return HeartbeatEnvelope{}, fmt.Errorf("canonicalize heartbeat: %w", err)
	}
	envelope.Signature = identity.Sign(canonical)
	return envelope, nil
}

// VerifyHeartbeat establishes attribution, not truth. An enrolled node that
// is compromised can sign false inventory about itself, but it cannot sign as
// another node, change the fleet id, make an old sequence current, or obtain
// placement authority. Phase B does not expose any placement operation.
func VerifyHeartbeat(envelope HeartbeatEnvelope, publicKey ed25519.PublicKey, fleetID, nodeID string) error {
	if envelope.Payload.Version != ProtocolVersion {
		return errors.New("heartbeat protocol version is not supported")
	}
	if envelope.Payload.FleetID != fleetID {
		return errors.New("heartbeat names the wrong fleet")
	}
	if envelope.Payload.NodeID != nodeID {
		return errors.New("heartbeat names the wrong node")
	}
	if envelope.Payload.Sequence <= 0 {
		return errors.New("heartbeat sequence must be positive")
	}
	if _, err := time.Parse(time.RFC3339Nano, envelope.Payload.LocalTime); err != nil {
		return errors.New("heartbeat local time is invalid")
	}
	canonical, err := envelope.signedBytes()
	if err != nil {
		return err
	}
	if !VerifySignature(publicKey, canonical, envelope.Signature) {
		return errors.New("heartbeat signature is invalid")
	}
	return nil
}

func CatalogueDigest(recipes []recipe.Recipe) (string, error) {
	ordered := append([]recipe.Recipe(nil), recipes...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].ID == ordered[j].ID {
			return ordered[i].Version < ordered[j].Version
		}
		return ordered[i].ID < ordered[j].ID
	})
	payload, err := json.Marshal(ordered)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func BuildHeartbeat(ctx context.Context, identity *Identity, database *store.Store, provider inventory.Provider, fleetID, managerVersion, buildIdentity, catalogueDigest string, at time.Time) (HeartbeatEnvelope, error) {
	sequence, err := database.NextHeartbeatSequence(ctx)
	if err != nil {
		return HeartbeatEnvelope{}, fmt.Errorf("advance heartbeat sequence: %w", err)
	}
	system, err := provider.Inspect(ctx)
	if err != nil {
		return HeartbeatEnvelope{}, fmt.Errorf("inspect node for heartbeat: %w", err)
	}
	models, err := database.Models(ctx)
	if err != nil {
		return HeartbeatEnvelope{}, fmt.Errorf("read models for heartbeat: %w", err)
	}
	snapshots := make([]ModelSnapshot, 0, len(models))
	for _, model := range models {
		snapshots = append(snapshots, ModelSnapshot{RecipeID: model.RecipeID, RecipeVersion: model.RecipeVersion, Status: model.Status, Active: model.Active})
	}
	reservations, err := database.NodeReservations(ctx)
	if err != nil {
		return HeartbeatEnvelope{}, fmt.Errorf("read reservations for heartbeat: %w", err)
	}
	allocations := make([]AllocationSnapshot, 0, len(reservations))
	for _, reservation := range reservations {
		if reservation.State == "released" || reservation.State == "aborted" || reservation.State == "expired" {
			continue
		}
		allocations = append(allocations, AllocationSnapshot{DeploymentID: reservation.DeploymentID, State: reservation.State})
	}
	power, err := database.PowerMode(ctx)
	if err != nil {
		return HeartbeatEnvelope{}, fmt.Errorf("read power mode for heartbeat: %w", err)
	}
	return SignHeartbeat(identity, HeartbeatPayload{
		Version: ProtocolVersion, FleetID: fleetID, NodeID: identity.NodeID,
		ManagerVersion: managerVersion, ManagerBuildIdentity: buildIdentity,
		Sequence: sequence, LocalTime: at.UTC().Format(time.RFC3339Nano), Inventory: system,
		InstalledModels: snapshots, Allocations: allocations, CatalogueDigest: catalogueDigest,
		PowerMode: power.Mode, PowerModeFailure: power.Failure,
	})
}
