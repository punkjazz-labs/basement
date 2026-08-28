package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

const localReservationLifetime = 24 * time.Hour

type ReservationRequest struct {
	ReservationID     string
	DeploymentID      string
	FleetID           string
	ControllerNodeID  string
	DriverNodeID      string
	RecipeID          string
	RecipeVersion     int
	RecipeFingerprint string
	Claims            Claims
	PrepareToken      string
	ExpiresAt         time.Time
}

type Reservation struct {
	store.NodeReservation
	Claims Claims
}

// Allocator is one manager's persistent admission authority. Every caller,
// including the local engine and the legacy worker API, uses the same SQLite
// transitions so a process restart cannot create a second view of disk or the
// runtime slot.
type Allocator struct {
	database *store.Store
	nodeID   string
	now      func() time.Time
}

func NewAllocator(database *store.Store, nodeID string) *Allocator {
	return &Allocator{database: database, nodeID: nodeID, now: time.Now}
}

func (a *Allocator) NodeID() string { return a.nodeID }

func ReservationID(kind, identity string) string {
	digest := sha256.Sum256([]byte(kind + "\x00" + identity))
	return "reservation_" + hex.EncodeToString(digest[:16])
}

// ExactRecipeReservationID excludes the full recipe document because a
// compatibility retry must keep the same key across process and catalogue
// reloads. The exact version still separates different pinned recipes, while
// the allocator's stored fingerprint and claims reject changed details.
func ExactRecipeReservationID(kind, nodeID, jobID, recipeID string, recipeVersion int) string {
	identity := nodeID + "\x00" + jobID + "\x00" + recipeID + "\x00" + strconv.Itoa(recipeVersion)
	return ReservationID(kind, identity)
}

func LocalPrepareToken(reservationID string) string {
	digest := sha256.Sum256([]byte("local-reservation\x00" + reservationID))
	return hex.EncodeToString(digest[:])
}

func (a *Allocator) Prepare(ctx context.Context, request ReservationRequest) (Reservation, bool, error) {
	request.Claims = canonicalClaims(request.Claims)
	if err := request.Claims.validate(); err != nil {
		return Reservation{}, false, err
	}
	if request.ReservationID == "" || request.DeploymentID == "" || request.RecipeID == "" || request.RecipeVersion <= 0 {
		return Reservation{}, false, errors.New("reservation identity, deployment, and exact recipe are required")
	}
	claimsJSON, err := json.Marshal(request.Claims)
	if err != nil {
		return Reservation{}, false, err
	}
	if existing, err := a.Reservation(ctx, request.ReservationID); err == nil {
		if !sameReservationRequest(existing, request, claimsJSON) {
			return Reservation{}, false, store.ErrReservationRetryConflict
		}
		return existing, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Reservation{}, false, err
	}
	if request.PrepareToken == "" {
		request.PrepareToken = LocalPrepareToken(request.ReservationID)
	}
	if request.ExpiresAt.IsZero() {
		request.ExpiresAt = a.now().Add(localReservationLifetime)
	}
	stored, created, err := a.database.PrepareNodeReservation(ctx, store.NodeReservation{
		ReservationID: request.ReservationID, DeploymentID: request.DeploymentID, FleetID: request.FleetID,
		ControllerNodeID: request.ControllerNodeID, DriverNodeID: request.DriverNodeID,
		RecipeID: request.RecipeID, RecipeVersion: request.RecipeVersion, RecipeFingerprint: request.RecipeFingerprint,
		ClaimsJSON: claimsJSON, PrepareTokenHash: hashSecret(request.PrepareToken),
		ExpiresAt: request.ExpiresAt.UTC().Format(time.RFC3339Nano),
	})
	// Two identical first prepares can both observe a missing row before the
	// SQLite insert serializes them. Their generated expiry timestamps differ,
	// but expiry is not the reservation's resource meaning, so the loser must
	// compare the durable authority and claims just like the ordinary retry
	// path instead of reporting a false idempotency conflict.
	if errors.Is(err, store.ErrReservationRetryConflict) {
		existing, existingErr := a.Reservation(ctx, request.ReservationID)
		if existingErr == nil && sameReservationRequest(existing, request, claimsJSON) {
			return existing, false, nil
		}
	}
	if err != nil {
		return Reservation{}, false, err
	}
	decoded, _, err := decodeReservation(stored)
	return decoded, created, err
}

func sameReservationRequest(existing Reservation, request ReservationRequest, claimsJSON []byte) bool {
	return existing.DeploymentID == request.DeploymentID && existing.FleetID == request.FleetID &&
		existing.ControllerNodeID == request.ControllerNodeID && existing.DriverNodeID == request.DriverNodeID &&
		existing.RecipeID == request.RecipeID && existing.RecipeVersion == request.RecipeVersion &&
		existing.RecipeFingerprint == request.RecipeFingerprint && string(existing.ClaimsJSON) == string(claimsJSON)
}

func (a *Allocator) Reservation(ctx context.Context, reservationID string) (Reservation, error) {
	stored, err := a.database.NodeReservation(ctx, reservationID)
	if err != nil {
		return Reservation{}, err
	}
	reservation, _, err := decodeReservation(stored)
	return reservation, err
}

func (a *Allocator) AllReservations(ctx context.Context) ([]Reservation, error) {
	stored, err := a.database.NodeReservations(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Reservation, 0, len(stored))
	for _, row := range stored {
		reservation, _, err := decodeReservation(row)
		if err != nil {
			return nil, err
		}
		result = append(result, reservation)
	}
	return result, nil
}

func decodeReservation(stored store.NodeReservation) (Reservation, bool, error) {
	var claims Claims
	if err := strictJSON(stored.ClaimsJSON, &claims); err != nil {
		return Reservation{}, false, fmt.Errorf("decode reservation claims: %w", err)
	}
	if err := claims.validate(); err != nil {
		return Reservation{}, false, err
	}
	return Reservation{NodeReservation: stored, Claims: claims}, true, nil
}

func (a *Allocator) Commit(ctx context.Context, reservationID, prepareToken string, grant []byte) (Reservation, error) {
	stored, err := a.database.CommitNodeReservation(ctx, reservationID, hashSecret(prepareToken), grant)
	if err != nil {
		return Reservation{}, err
	}
	reservation, _, err := decodeReservation(stored)
	return reservation, err
}

func (a *Allocator) AttachJob(ctx context.Context, reservationID, jobID string) error {
	return a.database.AttachNodeReservationJob(ctx, reservationID, jobID)
}

func (a *Allocator) Activate(ctx context.Context, reservationID, replaceRecipeID string) error {
	reservation, err := a.Reservation(ctx, reservationID)
	if err != nil {
		return err
	}
	if !reservation.Claims.Runtime {
		return errors.New("a disk-only reservation cannot claim the runtime slot")
	}
	return a.database.ActivateNodeReservation(ctx, reservationID, replaceRecipeID)
}

// ActivateMaintenance takes the allocator's durable admission latch without
// displacing an already serving model. The manager restart leaves that model's
// container alone, while every later runtime activation observes this claim
// and waits until the update result is known.
func (a *Allocator) ActivateMaintenance(ctx context.Context, reservationID string) error {
	reservation, err := a.Reservation(ctx, reservationID)
	if err != nil {
		return err
	}
	if reservation.Claims.Kind != ClaimKindUpdate || !reservation.Claims.Runtime {
		return errors.New("only a manager update can claim runtime maintenance")
	}
	return a.database.ActivateNodeMaintenanceReservation(ctx, reservationID)
}

func (a *Allocator) MaintenanceActive(ctx context.Context) (bool, error) {
	return a.database.NodeMaintenanceReservationActive(ctx)
}

func (a *Allocator) Renew(ctx context.Context, reservationID string, expires time.Time) error {
	return a.database.RenewNodeReservation(ctx, reservationID, expires)
}

// LegacyRanksDueForReclaim returns only driver-owned ranks whose liveness
// deadline passed. Local and independent active models have durable local
// ownership and are deliberately outside this driver lease.
func (a *Allocator) LegacyRanksDueForReclaim(ctx context.Context, at time.Time) ([]Reservation, error) {
	reservations, err := a.AllReservations(ctx)
	if err != nil {
		return nil, err
	}
	result := []Reservation{}
	for _, reservation := range reservations {
		if reservation.Claims.Kind != ClaimKindLegacyRank {
			continue
		}
		if reservation.State == "reclaiming" {
			result = append(result, reservation)
			continue
		}
		if reservation.State != "active" || reservation.ExpiresAt == "" {
			continue
		}
		expires, err := time.Parse(time.RFC3339Nano, reservation.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("parse legacy rank expiry: %w", err)
		}
		if !expires.After(at) {
			result = append(result, reservation)
		}
	}
	return result, nil
}

func (a *Allocator) BeginReclaim(ctx context.Context, reservationID string, at time.Time) (Reservation, error) {
	stored, err := a.database.BeginNodeReservationReclaim(ctx, reservationID, at)
	if err != nil {
		return Reservation{}, err
	}
	reservation, _, err := decodeReservation(stored)
	return reservation, err
}

func (a *Allocator) FinishReclaim(ctx context.Context, reservationID string) error {
	return a.database.FinishNodeReservationReclaim(ctx, reservationID)
}

func (a *Allocator) Abort(ctx context.Context, reservationID string) error {
	return a.database.AbortNodeReservation(ctx, reservationID)
}

func (a *Allocator) Release(ctx context.Context, reservationID string) error {
	return a.database.ReleaseNodeReservation(ctx, reservationID)
}

// ClearSettled removes a released or expired reservation so its deterministic
// identity can be prepared again. A settled row holds no claim; left in place
// it makes Prepare return it unchanged and Activate refuse it, which the
// caller reads as a fatal admission conflict. That is the 2026-08-12 recovery
// crash-loop, relearned on 2026-08-28 by the legacy-rank worker path when a
// reclaimed rank wedged its own job's identity. A row in any other state is
// left alone: the caller's Prepare and Activate judge a live claim.
func (a *Allocator) ClearSettled(ctx context.Context, reservationID string) error {
	existing, err := a.Reservation(ctx, reservationID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.State != "released" && existing.State != "expired" {
		return nil
	}
	return a.database.DeleteSettledNodeReservation(ctx, reservationID)
}

func (a *Allocator) ReleaseRecipe(ctx context.Context, recipeID, exceptReservationID string) error {
	return a.database.ReleaseActiveRecipeReservations(ctx, recipeID, exceptReservationID)
}

func (a *Allocator) ReservedDiskBytes(ctx context.Context, exceptReservationID string) (int64, error) {
	reservations, err := a.database.NodeReservations(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, stored := range reservations {
		if stored.ReservationID == exceptReservationID || (stored.State != "prepared" && stored.State != "committed") {
			continue
		}
		reservation, _, err := decodeReservation(stored)
		if err != nil {
			return 0, err
		}
		total += reservation.Claims.DiskBytes
	}
	return total, nil
}

// Reconcile keeps interrupted jobs' claims, releases terminal job claims,
// expires work that never became active, and reconstructs the runtime slot
// from the local installed-model authority. Active legacy ranks remain
// blocking here because the HTTP layer must stop their containers before it
// can finish reclaiming their expired driver leases.
func (a *Allocator) Reconcile(ctx context.Context, recipes []recipe.Recipe) error {
	reservations, err := a.database.NodeReservations(ctx)
	if err != nil {
		return err
	}
	// Local reservations created before job_id existed already carry the same
	// immutable owner inside their versioned claims. Promote that fact before
	// applying deadlines so an interrupted download cannot be mistaken for an
	// abandoned prepare after upgrading the manager.
	for _, stored := range reservations {
		reservation, _, err := decodeReservation(stored)
		if err != nil {
			return err
		}
		if stored.JobID == "" && reservation.Claims.JobID != "" && (stored.State == "committed" || stored.State == "active") {
			if err := a.AttachJob(ctx, stored.ReservationID, reservation.Claims.JobID); err != nil {
				return err
			}
		}
	}
	if _, err := a.database.ExpireNodeReservations(ctx, a.now()); err != nil {
		return err
	}
	reservations, err = a.database.NodeReservations(ctx)
	if err != nil {
		return err
	}
	models, err := a.database.Models(ctx)
	if err != nil {
		return err
	}
	activeModels := map[string]store.InstalledModel{}
	for _, model := range models {
		if model.Active {
			activeModels[model.RecipeID] = model
		}
	}
	activeClaims := map[string]bool{}
	for _, stored := range reservations {
		reservation, _, err := decodeReservation(stored)
		if err != nil {
			return err
		}
		if stored.State == "active" {
			if reservation.Claims.Kind == ClaimKindLegacyRank {
				continue
			}
			if _, active := activeModels[stored.RecipeID]; active {
				activeClaims[stored.RecipeID] = true
				continue
			}
			if err := a.Release(ctx, stored.ReservationID); err != nil {
				return err
			}
			continue
		}
		jobID := stored.JobID
		if jobID == "" {
			jobID = reservation.Claims.JobID
		}
		if (stored.State == "prepared" || stored.State == "committed") && jobID != "" {
			job, err := a.database.GetJob(ctx, jobID)
			if errors.Is(err, os.ErrNotExist) || (err == nil && reservationJobTerminal(job.State)) {
				if err := a.Release(ctx, stored.ReservationID); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
		}
	}
	for recipeID, model := range activeModels {
		if activeClaims[recipeID] {
			continue
		}
		selected, ok := recipe.FindVersion(recipes, recipeID, model.RecipeVersion)
		fingerprint := ""
		claims := Claims{Version: ClaimsVersion, Kind: ClaimKindRecovered, Runtime: true, Ports: []int{}, FabricInterfaces: []string{}}
		if ok {
			fingerprint, _ = RecipeFingerprint(selected)
			claims = ClaimsForRecipe(selected, RecipeClaimOptions{Kind: ClaimKindRecovered, Runtime: true})
		}
		reservationID := ReservationID(ClaimKindRecovered, recipeID)
		// A settled reservation left under this deterministic identity would
		// make Prepare return it unchanged and Activate refuse it, and the
		// caller treats that refusal as fatal. Hardware proved what that
		// means (2026-08-12): a machine whose active model once released its
		// recovery reservation turned its next restart into a crash loop,
		// with the console dead and nothing to click. A settled row holds no
		// claim, so the identity is cleared for reuse instead.
		if err := a.ClearSettled(ctx, reservationID); err != nil {
			return err
		}
		prepared, _, err := a.Prepare(ctx, ReservationRequest{
			ReservationID: reservationID, DeploymentID: "recovered:" + recipeID,
			DriverNodeID: a.nodeID, RecipeID: recipeID, RecipeVersion: model.RecipeVersion,
			RecipeFingerprint: fingerprint, Claims: claims,
		})
		if err != nil {
			return err
		}
		if prepared.State == "prepared" {
			if _, err := a.Commit(ctx, reservationID, LocalPrepareToken(reservationID), []byte(`{"kind":"startup-reconciliation"}`)); err != nil {
				return err
			}
		}
		if err := a.Activate(ctx, reservationID, recipeID); err != nil {
			return err
		}
	}
	return nil
}

func reservationJobTerminal(state string) bool {
	switch state {
	case "ready", "failed", "cancelled", "stopped", "removed":
		return true
	}
	return false
}
