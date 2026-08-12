package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/store"
	managerupdate "github.com/punkjazz-labs/basement/internal/update"
)

func (s *Server) updateAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.updateCheck(w, r)
		return
	}
	methodNotAllowed(w)
}

func (s *Server) updateApplyAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.applyUpdate(w, r)
		return
	}
	methodNotAllowed(w)
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	fleetRole, fleetNodeCount, err := s.updateFleetScope(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.updateResult != nil && time.Since(s.updateFetched) < time.Hour {
		s.updateResult["fleet_role"] = fleetRole
		s.updateResult["fleet_node_count"] = fleetNodeCount
		writeJSON(w, http.StatusOK, s.updateResult)
		return
	}
	result := map[string]any{
		"current_version": s.version, "checked": true, "update_available": false,
		"signed": false, "compatible": false, "installable": false,
		"fleet_role": fleetRole, "fleet_node_count": fleetNodeCount,
	}
	if _, err := managerupdate.ParseVersion(s.version); err != nil {
		result["note"] = "development builds cannot use console updates"
		s.finishUpdateCheck(result, nil)
		writeJSON(w, http.StatusOK, result)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	resolution, err := s.updateResolver.Resolve(ctx, s.version)
	if err != nil {
		result["checked"] = false
		result["note"] = cleanUpdateError(err)
		s.finishUpdateCheck(result, nil)
		writeJSON(w, http.StatusOK, result)
		return
	}
	if resolution.NewestPublished != "" {
		result["latest_version"] = strings.TrimPrefix(resolution.NewestPublished, "v")
		result["release_url"] = resolution.NewestReleaseURL
		result["update_available"] = managerupdate.IsNewer(s.version, resolution.NewestPublished)
	}
	if resolution.Candidate != nil {
		candidate := cloneUpdateCandidate(*resolution.Candidate)
		result["latest_version"] = strings.TrimPrefix(candidate.Manifest.ReleaseVersion, "v")
		result["target_version"] = candidate.Manifest.ReleaseVersion
		result["release_url"] = candidate.Release.HTMLURL
		result["update_available"] = true
		result["signed"] = true
		result["compatible"] = true
		if err := s.updateStager.BootstrapReady(); err != nil {
			result["manual_bootstrap_required"] = true
			result["reason"] = err.Error()
		} else {
			result["installable"] = true
		}
		s.finishUpdateCheck(result, &candidate)
		writeJSON(w, http.StatusOK, result)
		return
	}
	if resolution.ManualUpgrade {
		result["manual_upgrade_required"] = true
		result["reason"] = "this machine needs a one-time manual upgrade before console updates can continue"
	} else if resolution.NewestPublished == "" {
		result["note"] = "no published releases yet"
	} else if managerupdate.IsNewer(s.version, resolution.NewestPublished) {
		result["reason"] = "the newer release could not be verified for this machine"
	}
	s.finishUpdateCheck(result, nil)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) finishUpdateCheck(result map[string]any, candidate *managerupdate.Candidate) {
	result["checked_at"] = time.Now().UTC().Format(time.RFC3339)
	s.updateResult = result
	s.updateCandidate = candidate
	s.updateFetched = time.Now()
}

func (s *Server) applyUpdate(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if r.ContentLength != 0 {
		var request struct{}
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	s.updateAdmissionMu.Lock()
	defer s.updateAdmissionMu.Unlock()
	fleetRole, _, err := s.updateFleetScope(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if fleetRole != "standalone" {
		writeError(w, http.StatusConflict, errors.New("use the fleet rolling upgrade action so enrolled nodes update in sequence"))
		return
	}
	if s.updateMaintenanceActiveLocked() {
		writeError(w, http.StatusConflict, errors.New("a manager update is already in progress"))
		return
	}
	blocker, blocked, err := s.store.ActiveUpdateBlocker(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if blocked {
		writeError(w, http.StatusConflict, errors.New("cannot update while "+blocker.Activity+" is "+blocker.State+"; finish or cancel it first"))
		return
	}
	s.updateMu.Lock()
	var candidate *managerupdate.Candidate
	if s.updateCandidate != nil {
		copy := cloneUpdateCandidate(*s.updateCandidate)
		candidate = &copy
	}
	s.updateMu.Unlock()
	if candidate == nil {
		writeError(w, http.StatusConflict, errors.New("check for updates again before starting the update"))
		return
	}
	if err := s.updateStager.BootstrapReady(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	status, err := s.updateStager.Prepare(*candidate, s.version)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	s.updateApplying = true
	background := s.updateContext
	if background == nil {
		background = context.Background()
	} else if background.Err() != nil {
		s.updateApplying = false
		writeError(w, http.StatusServiceUnavailable, errors.New("the manager is shutting down"))
		return
	}
	s.updateWorkers.Add(1)
	go func(candidate managerupdate.Candidate) {
		defer s.updateWorkers.Done()
		ctx, cancel := context.WithTimeout(background, 30*time.Minute)
		defer cancel()
		_, err := s.updateStager.StagePrepared(ctx, candidate, status)
		if err != nil {
			s.updateAdmissionMu.Lock()
			s.updateApplying = false
			s.updateAdmissionMu.Unlock()
		}
	}(*candidate)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted": true, "attempt_id": status.AttemptID, "state": status.State,
	})
}

func (s *Server) updateFleetScope(ctx context.Context) (string, int, error) {
	if s.fleetManager == nil {
		return "standalone", 0, nil
	}
	config, err := s.store.FleetConfig(ctx)
	if err != nil {
		return "", 0, err
	}
	if config.Role == "standalone" {
		return "standalone", 0, nil
	}
	nodes, err := s.store.FleetNodes(ctx)
	if err != nil {
		return "", 0, err
	}
	return config.Role, len(nodes), nil
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	status, found, err := s.updateStager.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"active": false})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// RecoverAbandonedUpdate settles a staging attempt a previous manager process
// left behind, so a crash mid-download does not read as an update in progress
// forever. Call once at startup, before requests are served.
func (s *Server) RecoverAbandonedUpdate() error {
	return s.updateStager.ReconcileStartup()
}

const (
	updateInProgressRefusal = "a manager update is in progress; wait for basement to reconnect before starting new work"
	// A failed or resolved-but-not-settled fleet upgrade is not in progress,
	// and waiting never clears it. The refusal has to name the real cause and
	// the action that ends it.
	fleetUpgradeAttentionRefusal = "a fleet upgrade failed and needs attention; resolve it from the update screen on the fleet controller to release this machine"
	fleetUpgradeAwaitingRefusal  = "this machine finished its fleet update and is waiting for the fleet controller to confirm; if the fleet upgrade failed, resolve it from the controller's update screen"
)

func (s *Server) updateMaintenanceActiveLocked() bool {
	_, active := s.updateMaintenanceRefusalLocked()
	return active
}

// updateMaintenanceRefusalLocked reports whether local mutations must wait,
// and the sentence that honestly explains why. During a live update the right
// advice is to wait; once the local journal shows a failed or already-resolved
// fleet run, waiting is a lie and the advice is to resolve it.
func (s *Server) updateMaintenanceRefusalLocked() (string, bool) {
	if s.updateApplying {
		return updateInProgressRefusal, true
	}
	if s.store != nil {
		fleetActive, fleetErr := s.store.FleetUpgradeMaintenanceActive(context.Background())
		reservationActive, reservationErr := s.store.NodeMaintenanceReservationActive(context.Background())
		if (fleetErr == nil && fleetActive) || (reservationErr == nil && reservationActive) {
			if run, err := s.store.LatestFleetUpgradeRun(context.Background()); err == nil {
				switch run.State {
				case "failed", "resolved":
					return fleetUpgradeAttentionRefusal, true
				case "awaiting_fleet":
					return fleetUpgradeAwaitingRefusal, true
				}
			}
			return updateInProgressRefusal, true
		}
	}
	status, found, err := s.updateStager.Status()
	if err != nil || !found {
		return "", false
	}
	switch status.State {
	case "succeeded", "rolled_back", "recovery_required", "failed_before_handoff":
		return "", false
	default:
		return updateInProgressRefusal, true
	}
}

func (s *Server) fleetUpgradeAPI(w http.ResponseWriter, r *http.Request) {
	if s.fleetManager == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("fleet upgrade is unavailable"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		run, err := s.fleetManager.LatestUpgrade(r.Context())
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{
				"active": false, "confirmation_required": true, "controller_last": true,
				"restart_consequence": "each node's console and /v1 proxy reconnect while its manager restarts; existing model containers are left alone",
			})
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, run)
	case http.MethodPost:
		if err := s.auth.AuthorizeMutation(r); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
		var request struct {
			Confirmed bool   `json:"confirmed"`
			Action    string `json:"action"`
		}
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if request.Action == "resolve" {
			run, err := s.fleetManager.ResolveUpgrade(r.Context())
			if err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			writeJSON(w, http.StatusOK, run)
			return
		}
		if request.Action != "" && request.Action != "start" {
			writeError(w, http.StatusBadRequest, errors.New("unknown fleet upgrade action"))
			return
		}
		if !request.Confirmed {
			writeError(w, http.StatusConflict, errors.New("confirm the per-node console and /v1 restart before starting the fleet upgrade"))
			return
		}
		s.updateMu.Lock()
		var candidate *managerupdate.Candidate
		if s.updateCandidate != nil {
			cloned := cloneUpdateCandidate(*s.updateCandidate)
			candidate = &cloned
		}
		s.updateMu.Unlock()
		if candidate == nil {
			writeError(w, http.StatusConflict, errors.New("check for updates again before starting the fleet upgrade"))
			return
		}
		run, created, err := s.fleetManager.StartUpgrade(r.Context(), *candidate)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		status := http.StatusAccepted
		if !created {
			status = http.StatusOK
		}
		writeJSON(w, status, run)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) StageFleetUpgrade(ctx context.Context, release fleet.UpgradeRelease) (fleet.LocalUpgradeStatus, error) {
	if err := validateFleetUpgradeRelease(release); err != nil {
		return fleet.LocalUpgradeStatus{}, err
	}
	s.updateAdmissionMu.Lock()
	run, node, err := s.ensureLocalFleetUpgrade(ctx, release)
	if err != nil {
		s.updateAdmissionMu.Unlock()
		return fleet.LocalUpgradeStatus{}, err
	}
	if err := s.ensureFleetUpgradeReservation(ctx, run); err != nil {
		s.updateAdmissionMu.Unlock()
		return fleet.LocalUpgradeStatus{}, err
	}
	blocker, blocked, err := s.store.ActiveUpdateBlocker(ctx)
	if err != nil {
		s.updateAdmissionMu.Unlock()
		return fleet.LocalUpgradeStatus{}, err
	}
	if blocked {
		failure := "waiting for " + blocker.Activity + " to finish"
		_ = s.store.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "waiting_for_idle", s.version, node.AttemptID, failure)
		_ = s.store.UpdateFleetUpgradeRunState(ctx, run.RunID, "waiting_for_idle", "")
		s.updateAdmissionMu.Unlock()
		return fleet.LocalUpgradeStatus{Version: fleet.UpgradeProtocolVersion, RunID: run.RunID, State: "waiting_for_idle", RunningVersion: s.version, TargetVersion: run.TargetVersion, AttemptID: node.AttemptID, Failure: failure}, nil
	}
	status, found, statusErr := s.updateStager.Status()
	if statusErr != nil {
		s.updateAdmissionMu.Unlock()
		return fleet.LocalUpgradeStatus{}, statusErr
	}
	if found && node.AttemptID != "" && status.AttemptID == node.AttemptID && status.State == "staged" {
		s.updateAdmissionMu.Unlock()
		return localFleetUpgradeStatus(run.RunID, status), nil
	}
	candidate := managerupdate.Candidate{Release: managerupdate.Release{TagName: release.ReleaseTag}, ManifestBytes: append([]byte(nil), release.ManifestBytes...), Signature: append([]byte(nil), release.SignatureBytes...), AssetURL: release.AssetURL}
	if node.AttemptID == "" {
		status, err = s.updateStager.Prepare(candidate, s.version)
		if err != nil {
			_ = s.store.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "failed_before_handoff", s.version, "", cleanUpdateError(err))
			_ = s.store.UpdateFleetUpgradeRunState(ctx, run.RunID, "failed", cleanUpdateError(err))
			s.updateAdmissionMu.Unlock()
			return fleet.LocalUpgradeStatus{}, err
		}
		if err := s.store.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "staging", s.version, status.AttemptID, ""); err != nil {
			s.updateAdmissionMu.Unlock()
			return fleet.LocalUpgradeStatus{}, err
		}
	} else {
		if !found || status.AttemptID != node.AttemptID {
			s.updateAdmissionMu.Unlock()
			return fleet.LocalUpgradeStatus{}, errors.New("the persisted fleet upgrade attempt is unavailable")
		}
	}
	s.updateAdmissionMu.Unlock()
	status, err = s.updateStager.StageOnly(ctx, candidate, status)
	if err != nil {
		_ = s.store.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "failed_before_handoff", s.version, status.AttemptID, cleanUpdateError(err))
		_ = s.store.UpdateFleetUpgradeRunState(ctx, run.RunID, "failed", cleanUpdateError(err))
		return fleet.LocalUpgradeStatus{}, err
	}
	if err := s.store.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "staged", s.version, status.AttemptID, ""); err != nil {
		return fleet.LocalUpgradeStatus{}, err
	}
	_ = s.store.UpdateFleetUpgradeRunState(ctx, run.RunID, "staging", "")
	return localFleetUpgradeStatus(run.RunID, status), nil
}

func (s *Server) ApplyFleetUpgrade(ctx context.Context, request fleet.UpgradeApplyRequest) (fleet.LocalUpgradeStatus, error) {
	if request.Version != fleet.UpgradeProtocolVersion || request.RunID == "" || request.TargetVersion == "" || request.ManifestSHA256 == "" {
		return fleet.LocalUpgradeStatus{}, errors.New("fleet upgrade apply identity is invalid")
	}
	s.updateAdmissionMu.Lock()
	defer s.updateAdmissionMu.Unlock()
	run, err := s.store.FleetUpgradeRun(ctx, request.RunID)
	if err != nil {
		return fleet.LocalUpgradeStatus{}, err
	}
	if run.TargetVersion != request.TargetVersion || run.ManifestSHA256 != request.ManifestSHA256 {
		return fleet.LocalUpgradeStatus{}, errors.New("fleet upgrade apply does not match the independently staged release")
	}
	node, err := localUpgradeNode(run, s.fleetManager.Identity().NodeID)
	if err != nil {
		return fleet.LocalUpgradeStatus{}, err
	}
	blocker, blocked, err := s.store.ActiveUpdateBlocker(ctx)
	if err != nil {
		return fleet.LocalUpgradeStatus{}, err
	}
	if blocked {
		return fleet.LocalUpgradeStatus{Version: fleet.UpgradeProtocolVersion, RunID: run.RunID, State: "waiting_for_idle", RunningVersion: s.version, TargetVersion: run.TargetVersion, AttemptID: node.AttemptID, Failure: "waiting for " + blocker.Activity + " to finish"}, nil
	}
	status, found, err := s.updateStager.Status()
	if err != nil {
		return fleet.LocalUpgradeStatus{}, err
	}
	if !found || status.AttemptID != node.AttemptID || status.State != "staged" {
		return fleet.LocalUpgradeStatus{}, errors.New("the exact fleet release is not staged on this node")
	}
	if err := s.store.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "applying", s.version, status.AttemptID, ""); err != nil {
		return fleet.LocalUpgradeStatus{}, err
	}
	_ = s.store.UpdateFleetUpgradeRunState(ctx, run.RunID, "applying", "")
	status, err = s.updateStager.ApplyStaged(status)
	if err != nil {
		return fleet.LocalUpgradeStatus{}, err
	}
	return localFleetUpgradeStatus(run.RunID, status), nil
}

func (s *Server) FleetUpgradeStatus(ctx context.Context, runID string) (fleet.LocalUpgradeStatus, error) {
	if runID == "" {
		return fleet.LocalUpgradeStatus{}, errors.New("fleet upgrade run id is required")
	}
	run, err := s.store.FleetUpgradeRun(ctx, runID)
	if err != nil {
		return fleet.LocalUpgradeStatus{}, err
	}
	node, err := localUpgradeNode(run, s.fleetManager.Identity().NodeID)
	if err != nil {
		return fleet.LocalUpgradeStatus{}, err
	}
	status, found, err := s.updateStager.Status()
	if err != nil {
		return fleet.LocalUpgradeStatus{}, err
	}
	if !found || (node.AttemptID != "" && status.AttemptID != node.AttemptID) {
		return fleet.LocalUpgradeStatus{Version: fleet.UpgradeProtocolVersion, RunID: runID, State: node.State, RunningVersion: s.version, TargetVersion: run.TargetVersion, AttemptID: node.AttemptID, Failure: node.Failure}, nil
	}
	result := localFleetUpgradeStatus(runID, status)
	if result.RunningVersion == "" {
		result.RunningVersion = s.version
	}
	localState := result.State
	switch result.State {
	case "succeeded":
		if result.RunningVersion != run.TargetVersion || s.version != run.TargetVersion {
			localState = "checking_health"
		} else {
			config, configErr := s.store.FleetConfig(ctx)
			if configErr == nil && config.Role != "controller" {
				_ = s.store.UpdateFleetUpgradeRunState(ctx, runID, "awaiting_fleet", "")
			}
		}
	case "rolled_back", "recovery_required", "failed_before_handoff":
		_ = s.store.UpdateFleetUpgradeRunState(ctx, runID, "failed", result.Failure)
		if result.State != "recovery_required" {
			_ = s.releaseFleetUpgradeReservation(ctx, runID)
		}
	}
	_ = s.store.UpdateFleetUpgradeNode(ctx, runID, node.NodeID, localState, result.RunningVersion, result.AttemptID, result.Failure)
	result.State = localState
	return result, nil
}

func (s *Server) FinishFleetUpgrade(ctx context.Context, request fleet.UpgradeFinishRequest) error {
	if request.Version != fleet.UpgradeProtocolVersion || request.RunID == "" || request.TargetVersion == "" {
		return errors.New("fleet upgrade finish identity is invalid")
	}
	run, err := s.store.FleetUpgradeRun(ctx, request.RunID)
	if err != nil {
		return err
	}
	if run.TargetVersion != request.TargetVersion || s.version != request.TargetVersion {
		return errors.New("this node is not running the fleet target version")
	}
	node, err := localUpgradeNode(run, s.fleetManager.Identity().NodeID)
	if err != nil {
		return err
	}
	status, found, err := s.updateStager.Status()
	if err != nil {
		return err
	}
	if !found || status.AttemptID != node.AttemptID || status.State != "succeeded" || status.RunningVersion != request.TargetVersion {
		return errors.New("this node has not completed local update health verification")
	}
	if err := s.store.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "succeeded", s.version, status.AttemptID, ""); err != nil {
		return err
	}
	if err := s.store.UpdateFleetUpgradeRunState(ctx, run.RunID, "succeeded", ""); err != nil {
		return err
	}
	return s.releaseFleetUpgradeReservation(ctx, run.RunID)
}

// ResolveFleetUpgrade settles this node's share of a fleet upgrade the owner
// resolved on the controller. Whatever the node's own outcome was, succeeded
// included, its local run journal becomes terminal, any staged release that
// will never be applied is settled, and the per-node maintenance reservation
// is released so local installs, generations, and model control work again.
// Every step tolerates already being done, so the controller can retry.
func (s *Server) ResolveFleetUpgrade(ctx context.Context, request fleet.UpgradeResolveRequest) error {
	if request.Version != fleet.UpgradeProtocolVersion || request.RunID == "" {
		return errors.New("fleet upgrade resolve identity is invalid")
	}
	s.updateAdmissionMu.Lock()
	defer s.updateAdmissionMu.Unlock()
	run, err := s.store.FleetUpgradeRun(ctx, request.RunID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if node, nodeErr := localUpgradeNode(run, s.fleetManager.Identity().NodeID); nodeErr == nil && node.AttemptID != "" {
			status, found, statusErr := s.updateStager.Status()
			if statusErr != nil {
				return statusErr
			}
			if found && status.AttemptID == node.AttemptID {
				if settleErr := s.updateStager.SettleResolved("the fleet upgrade was resolved before this release was applied; start a new update when ready"); settleErr != nil {
					return settleErr
				}
			}
		}
		switch run.State {
		case "succeeded", "resolved":
		default:
			if stateErr := s.store.UpdateFleetUpgradeRunState(ctx, run.RunID, "resolved", run.Failure); stateErr != nil {
				return stateErr
			}
		}
	}
	if releaseErr := s.releaseFleetUpgradeReservation(ctx, request.RunID); releaseErr != nil && !errors.Is(releaseErr, os.ErrNotExist) {
		return releaseErr
	}
	return nil
}

func (s *Server) ensureLocalFleetUpgrade(ctx context.Context, release fleet.UpgradeRelease) (store.FleetUpgradeRun, store.FleetUpgradeNode, error) {
	config, err := s.store.FleetConfig(ctx)
	if err != nil {
		return store.FleetUpgradeRun{}, store.FleetUpgradeNode{}, err
	}
	nodeID := s.fleetManager.Identity().NodeID
	nodes, err := s.store.FleetNodes(ctx)
	if err != nil {
		return store.FleetUpgradeRun{}, store.FleetUpgradeNode{}, err
	}
	displayName := "this node"
	for _, fleetNode := range nodes {
		if fleetNode.NodeID == nodeID && fleetNode.DisplayName != "" {
			displayName = fleetNode.DisplayName
			break
		}
	}
	run := store.FleetUpgradeRun{RunID: release.RunID, FleetID: config.FleetID, ControllerNodeID: config.ControllerNodeID,
		ReleaseTag: release.ReleaseTag, TargetVersion: release.TargetVersion, ManifestSHA256: release.ManifestSHA256,
		ManifestBytes: append([]byte(nil), release.ManifestBytes...), SignatureBytes: append([]byte(nil), release.SignatureBytes...), AssetURL: release.AssetURL}
	stored, _, err := s.store.CreateFleetUpgradeRun(ctx, run, []store.FleetUpgradeNode{{NodeID: nodeID, DisplayName: displayName, Sequence: 0, Role: config.Role, RunningVersion: s.version, TargetVersion: release.TargetVersion}})
	if err != nil {
		return store.FleetUpgradeRun{}, store.FleetUpgradeNode{}, err
	}
	node, err := localUpgradeNode(stored, nodeID)
	return stored, node, err
}

func (s *Server) ensureFleetUpgradeReservation(ctx context.Context, run store.FleetUpgradeRun) error {
	allocator := s.fleetManager.Allocator()
	reservationID := fleet.ReservationID(fleet.ClaimKindUpdate, run.RunID)
	reservation, _, err := allocator.Prepare(ctx, fleet.ReservationRequest{
		ReservationID: reservationID, DeploymentID: "upgrade:" + run.RunID,
		FleetID: run.FleetID, ControllerNodeID: run.ControllerNodeID, DriverNodeID: run.ControllerNodeID,
		RecipeID: "basement-manager", RecipeVersion: 1, RecipeFingerprint: run.ManifestSHA256,
		Claims: fleet.Claims{Version: fleet.ClaimsVersion, Kind: fleet.ClaimKindUpdate, Runtime: true, Ports: []int{}, FabricInterfaces: []string{}},
	})
	if err != nil {
		return err
	}
	if reservation.State == "prepared" {
		if _, err := allocator.Commit(ctx, reservationID, fleet.LocalPrepareToken(reservationID), []byte(`{"kind":"manager-update"}`)); err != nil {
			return err
		}
	}
	return allocator.ActivateMaintenance(ctx, reservationID)
}

func (s *Server) releaseFleetUpgradeReservation(ctx context.Context, runID string) error {
	return s.fleetManager.Allocator().Release(ctx, fleet.ReservationID(fleet.ClaimKindUpdate, runID))
}

func validateFleetUpgradeRelease(release fleet.UpgradeRelease) error {
	if release.Version != fleet.UpgradeProtocolVersion || release.RunID == "" || release.ReleaseTag == "" || release.TargetVersion == "" || release.ManifestSHA256 == "" || len(release.ManifestBytes) == 0 || len(release.SignatureBytes) == 0 || release.AssetURL == "" {
		return errors.New("fleet upgrade release identity is invalid")
	}
	if release.ReleaseTag != release.TargetVersion || managerupdate.ManifestDigest(release.ManifestBytes) != release.ManifestSHA256 {
		return errors.New("fleet upgrade release does not match its manifest identity")
	}
	return nil
}

func localUpgradeNode(run store.FleetUpgradeRun, nodeID string) (store.FleetUpgradeNode, error) {
	for _, node := range run.Nodes {
		if node.NodeID == nodeID {
			return node, nil
		}
	}
	return store.FleetUpgradeNode{}, fmt.Errorf("fleet upgrade run does not include node %s", nodeID)
}

func localFleetUpgradeStatus(runID string, status managerupdate.AttemptStatus) fleet.LocalUpgradeStatus {
	return fleet.LocalUpgradeStatus{Version: fleet.UpgradeProtocolVersion, RunID: runID, State: status.State,
		RunningVersion: status.RunningVersion, TargetVersion: status.TargetVersion, AttemptID: status.AttemptID, Failure: status.Failure}
}

func (s *Server) refuseMutationDuringUpdate(w http.ResponseWriter) bool {
	message, active := s.updateMaintenanceRefusalLocked()
	if !active {
		return false
	}
	writeError(w, http.StatusConflict, errors.New(message))
	return true
}

func cloneUpdateCandidate(candidate managerupdate.Candidate) managerupdate.Candidate {
	candidate.ManifestBytes = append([]byte(nil), candidate.ManifestBytes...)
	candidate.Signature = append([]byte(nil), candidate.Signature...)
	candidate.Manifest.RollbackFrom = append([]string(nil), candidate.Manifest.RollbackFrom...)
	candidate.Release.Assets = append([]managerupdate.ReleaseAsset(nil), candidate.Release.Assets...)
	return candidate
}

func cleanUpdateError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}
