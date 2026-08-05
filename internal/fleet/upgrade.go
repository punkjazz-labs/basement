package fleet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/punkjazz-labs/basement/internal/store"
	managerupdate "github.com/punkjazz-labs/basement/internal/update"
)

const UpgradeProtocolVersion = 1

type UpgradeRelease struct {
	Version        int    `json:"version"`
	RunID          string `json:"run_id"`
	ReleaseTag     string `json:"release_tag"`
	TargetVersion  string `json:"target_version"`
	ManifestSHA256 string `json:"manifest_sha256"`
	ManifestBytes  []byte `json:"manifest_bytes"`
	SignatureBytes []byte `json:"signature_bytes"`
	AssetURL       string `json:"asset_url"`
}

type UpgradeApplyRequest struct {
	Version        int    `json:"version"`
	RunID          string `json:"run_id"`
	TargetVersion  string `json:"target_version"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

type UpgradeFinishRequest struct {
	Version       int    `json:"version"`
	RunID         string `json:"run_id"`
	TargetVersion string `json:"target_version"`
}

type LocalUpgradeStatus struct {
	Version        int    `json:"version"`
	RunID          string `json:"run_id"`
	State          string `json:"state"`
	RunningVersion string `json:"running_version"`
	TargetVersion  string `json:"target_version"`
	AttemptID      string `json:"attempt_id,omitempty"`
	Failure        string `json:"failure,omitempty"`
}

type upgradeOrderedNode struct {
	node store.FleetNode
	role string
}

// UpgradeRuntime is the node-local boundary around the existing stager,
// updater receipt, busy check, and allocator maintenance claim. The fleet
// protocol can request an exact signed release, but only this local boundary
// can verify, stage, and hand it to this node's root updater.
type UpgradeRuntime interface {
	StageFleetUpgrade(context.Context, UpgradeRelease) (LocalUpgradeStatus, error)
	ApplyFleetUpgrade(context.Context, UpgradeApplyRequest) (LocalUpgradeStatus, error)
	FleetUpgradeStatus(context.Context, string) (LocalUpgradeStatus, error)
	FinishFleetUpgrade(context.Context, UpgradeFinishRequest) error
}

func (m *Manager) requireFleetMutationAllowed(ctx context.Context) error {
	active, err := m.database.FleetUpgradeMaintenanceActive(ctx)
	if err != nil {
		return err
	}
	if active {
		return errors.New("fleet maintenance is active; placement and membership changes wait until every node runs the target version")
	}
	return nil
}

func candidateFromUpgradeRelease(release UpgradeRelease) managerupdate.Candidate {
	return managerupdate.Candidate{
		Release:       managerupdate.Release{TagName: release.ReleaseTag},
		ManifestBytes: append([]byte(nil), release.ManifestBytes...),
		Signature:     append([]byte(nil), release.SignatureBytes...),
		AssetURL:      release.AssetURL,
	}
}

func validateUpgradeRelease(release UpgradeRelease) error {
	if release.Version != UpgradeProtocolVersion || release.RunID == "" || release.ReleaseTag == "" || release.TargetVersion == "" || release.ManifestSHA256 == "" || len(release.ManifestBytes) == 0 || len(release.SignatureBytes) == 0 || release.AssetURL == "" {
		return errors.New("fleet upgrade release identity is invalid")
	}
	if release.ReleaseTag != release.TargetVersion || managerupdate.ManifestDigest(release.ManifestBytes) != release.ManifestSHA256 {
		return errors.New("fleet upgrade release does not match its manifest identity")
	}
	return nil
}

func validateUpgradeApply(request UpgradeApplyRequest) error {
	if request.Version != UpgradeProtocolVersion || request.RunID == "" || request.TargetVersion == "" || request.ManifestSHA256 == "" {
		return errors.New("fleet upgrade apply identity is invalid")
	}
	return nil
}

// StartUpgrade records the complete plan before starting the asynchronous
// driver. Every node must currently agree with the controller so this action
// begins from a placement-safe fleet rather than trying to repair unknown
// pre-existing skew.
func (m *Manager) StartUpgrade(ctx context.Context, candidate managerupdate.Candidate) (store.FleetUpgradeRun, bool, error) {
	config, err := m.database.FleetConfig(ctx)
	if err != nil {
		return store.FleetUpgradeRun{}, false, err
	}
	if config.Role != "controller" {
		if config.Role == "standalone" {
			return store.FleetUpgradeRun{}, false, errors.New("this installation has no fleet; use the local manager update action")
		}
		return store.FleetUpgradeRun{}, false, errors.New("only the fleet controller can start a rolling upgrade")
	}
	if m.upgradeRuntimeValue() == nil {
		return store.FleetUpgradeRun{}, false, errors.New("fleet upgrade runtime is unavailable")
	}
	if err := m.PollOnce(ctx); err != nil {
		return store.FleetUpgradeRun{}, false, fmt.Errorf("refresh fleet before upgrade: %w", err)
	}
	nodes, err := m.database.FleetNodes(ctx)
	if err != nil {
		return store.FleetUpgradeRun{}, false, err
	}
	if len(nodes) == 0 {
		return store.FleetUpgradeRun{}, false, errors.New("the fleet has no enrolled nodes")
	}
	now := m.now()
	for _, node := range nodes {
		if node.MembershipState != "active" {
			return store.FleetUpgradeRun{}, false, fmt.Errorf("node %s is not an active fleet member", node.DisplayName)
		}
		if node.ManagerVersion != m.version || node.ManagerBuildIdentity != m.buildIdentity {
			return store.FleetUpgradeRun{}, false, fmt.Errorf("node %s does not exactly match the controller release", node.DisplayName)
		}
		received, parseErr := time.Parse(time.RFC3339Nano, node.HeartbeatReceivedAt)
		if parseErr != nil || now.Sub(received) > HeartbeatFreshness {
			return store.FleetUpgradeRun{}, false, fmt.Errorf("node %s does not have a fresh heartbeat", node.DisplayName)
		}
	}
	manifest := candidate.Manifest
	if manifest.ReleaseVersion == "" || len(candidate.ManifestBytes) == 0 || len(candidate.Signature) == 0 || candidate.AssetURL == "" {
		return store.FleetUpgradeRun{}, false, errors.New("the controller has no verified signed release selected")
	}
	if err := managerupdate.ValidateCandidate(manifest, candidate.Release.TagName, m.version); err != nil {
		return store.FleetUpgradeRun{}, false, err
	}
	random, err := randomSecret(18)
	if err != nil {
		return store.FleetUpgradeRun{}, false, err
	}
	runID := "fleet-upgrade-" + random
	ordered, err := m.upgradeOrder(ctx, nodes, config.ControllerNodeID, manifest.ReleaseVersion)
	if err != nil {
		return store.FleetUpgradeRun{}, false, err
	}
	run := store.FleetUpgradeRun{
		RunID: runID, FleetID: config.FleetID, ControllerNodeID: config.ControllerNodeID,
		ReleaseTag: candidate.Release.TagName, TargetVersion: manifest.ReleaseVersion,
		ManifestSHA256: managerupdate.ManifestDigest(candidate.ManifestBytes),
		ManifestBytes:  append([]byte(nil), candidate.ManifestBytes...), SignatureBytes: append([]byte(nil), candidate.Signature...),
		AssetURL: candidate.AssetURL,
	}
	createdRun, created, err := m.database.CreateFleetUpgradeRun(ctx, run, ordered)
	if err != nil {
		return store.FleetUpgradeRun{}, false, err
	}
	m.startUpgradeDriver(context.Background())
	return createdRun, created, nil
}

func (m *Manager) upgradeOrder(ctx context.Context, nodes []store.FleetNode, controllerID, targetVersion string) ([]store.FleetUpgradeNode, error) {
	deployments, err := m.database.FleetDeployments(ctx)
	if err != nil {
		return nil, err
	}
	nodeByID := make(map[string]store.FleetNode, len(nodes))
	assigned := make(map[string]bool)
	for _, node := range nodes {
		nodeByID[node.NodeID] = node
	}
	var idle, independent, grouped, remaining []upgradeOrderedNode
	for _, deployment := range deployments {
		if deployment.State == "removed" {
			continue
		}
		for _, node := range deployment.Nodes {
			assigned[node.NodeID] = true
		}
	}
	for _, node := range nodes {
		if node.NodeID == controllerID {
			continue
		}
		if !assigned[node.NodeID] {
			idle = append(idle, upgradeOrderedNode{node: node, role: "idle"})
		}
	}
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].DeploymentID < deployments[j].DeploymentID })
	seen := make(map[string]bool)
	for _, item := range idle {
		seen[item.node.NodeID] = true
	}
	for _, deployment := range deployments {
		if deployment.State == "removed" || deployment.TopologyCount != 1 {
			continue
		}
		for _, deploymentNode := range deployment.Nodes {
			if deploymentNode.NodeID != controllerID && !seen[deploymentNode.NodeID] {
				if node, ok := nodeByID[deploymentNode.NodeID]; ok {
					independent = append(independent, upgradeOrderedNode{node: node, role: "member"})
					seen[node.NodeID] = true
				}
			}
		}
	}
	for _, deployment := range deployments {
		if deployment.State == "removed" || deployment.TopologyCount != 2 {
			continue
		}
		var workers, heads []upgradeOrderedNode
		for _, deploymentNode := range deployment.Nodes {
			if deploymentNode.NodeID == controllerID || seen[deploymentNode.NodeID] {
				continue
			}
			node, ok := nodeByID[deploymentNode.NodeID]
			if !ok {
				continue
			}
			if deploymentNode.NodeID == deployment.OwnerNodeID || deploymentNode.NodeRole == "head" {
				heads = append(heads, upgradeOrderedNode{node: node, role: "group-head"})
			} else {
				workers = append(workers, upgradeOrderedNode{node: node, role: "worker"})
			}
		}
		sortUpgradeNodes(workers)
		sortUpgradeNodes(heads)
		for _, item := range append(workers, heads...) {
			grouped = append(grouped, item)
			seen[item.node.NodeID] = true
		}
	}
	for _, node := range nodes {
		if node.NodeID != controllerID && !seen[node.NodeID] {
			remaining = append(remaining, upgradeOrderedNode{node: node, role: "member"})
		}
	}
	sortUpgradeNodes(idle)
	sortUpgradeNodes(independent)
	sortUpgradeNodes(remaining)
	ordered := append(append(append(idle, independent...), grouped...), remaining...)
	controller, ok := nodeByID[controllerID]
	if !ok {
		return nil, errors.New("the fleet controller is missing from its membership projection")
	}
	ordered = append(ordered, upgradeOrderedNode{node: controller, role: "controller"})
	result := make([]store.FleetUpgradeNode, 0, len(ordered))
	for sequence, item := range ordered {
		result = append(result, store.FleetUpgradeNode{NodeID: item.node.NodeID, DisplayName: item.node.DisplayName,
			Sequence: sequence, Role: item.role, RunningVersion: item.node.ManagerVersion, TargetVersion: targetVersion})
	}
	return result, nil
}

func sortUpgradeNodes(nodes []upgradeOrderedNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].node.DisplayName != nodes[j].node.DisplayName {
			return nodes[i].node.DisplayName < nodes[j].node.DisplayName
		}
		return nodes[i].node.NodeID < nodes[j].node.NodeID
	})
}

func (m *Manager) LatestUpgrade(ctx context.Context) (store.FleetUpgradeRun, error) {
	return m.database.LatestFleetUpgradeRun(ctx)
}

func (m *Manager) startUpgradeDriver(ctx context.Context) {
	m.upgradeMu.Lock()
	if m.upgradeRunning {
		m.upgradeMu.Unlock()
		return
	}
	m.upgradeRunning = true
	m.upgradeMu.Unlock()
	go func() {
		defer func() {
			m.upgradeMu.Lock()
			m.upgradeRunning = false
			m.upgradeMu.Unlock()
		}()
		for {
			done, err := m.AdvanceUpgrade(ctx)
			if done || err != nil {
				return
			}
			timer := time.NewTimer(m.upgradeRetry)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// AdvanceUpgrade performs at most one remote transition. Persisting before
// every call makes a lost response and a controller restart converge through
// status instead of replaying a second node restart.
func (m *Manager) AdvanceUpgrade(ctx context.Context) (bool, error) {
	config, err := m.database.FleetConfig(ctx)
	if err != nil {
		return false, err
	}
	if config.Role != "controller" {
		return true, nil
	}
	run, err := m.database.ActiveFleetUpgradeRun(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, node := range run.Nodes {
		switch node.State {
		case "staged", "applying", "checking_health", "succeeded":
			continue
		}
		status, err := m.upgradeStageCall(ctx, run, node)
		if err != nil {
			return true, m.failUpgrade(ctx, run, node, "failed", node.RunningVersion, err)
		}
		if status.State == "waiting_for_idle" {
			if err := m.database.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, status.State, status.RunningVersion, status.AttemptID, status.Failure); err != nil {
				return false, err
			}
			_ = m.database.UpdateFleetUpgradeRunState(ctx, run.RunID, "waiting_for_idle", "")
			return false, nil
		}
		if status.State != "staged" {
			return true, m.failUpgrade(ctx, run, node, "failed", status.RunningVersion, errors.New("node did not report a verified staged release"))
		}
		if err := m.database.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "staged", status.RunningVersion, status.AttemptID, ""); err != nil {
			return false, err
		}
		_ = m.database.UpdateFleetUpgradeRunState(ctx, run.RunID, "staging", "")
		return false, nil
	}
	if run.State != "applying" && run.State != "finalizing" {
		if err := m.database.UpdateFleetUpgradeRunState(ctx, run.RunID, "applying", ""); err != nil {
			return false, err
		}
		return false, nil
	}
	for _, node := range run.Nodes {
		if node.State == "succeeded" {
			continue
		}
		if node.State == "staged" {
			if err := m.database.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "applying", node.RunningVersion, node.AttemptID, ""); err != nil {
				return false, err
			}
			status, applyErr := m.upgradeApplyCall(ctx, run, node)
			if applyErr == nil {
				return m.recordUpgradeStatus(ctx, run, node, status)
			}
			// Restarting the target manager can close the connection that asked
			// for it. The durable applying state makes that an expected status
			// poll, not evidence that the node failed.
			return false, nil
		}
		status, statusErr := m.upgradeStatusCall(ctx, node)
		if statusErr != nil {
			return false, nil
		}
		return m.recordUpgradeStatus(ctx, run, node, status)
	}
	if run.State != "finalizing" {
		if err := m.database.UpdateFleetUpgradeRunState(ctx, run.RunID, "finalizing", ""); err != nil {
			return false, err
		}
		return false, nil
	}
	for _, node := range run.Nodes {
		if err := m.upgradeFinishCall(ctx, run, node); err != nil {
			return false, nil
		}
	}
	if err := m.database.UpdateFleetUpgradeRunState(ctx, run.RunID, "succeeded", ""); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) recordUpgradeStatus(ctx context.Context, run store.FleetUpgradeRun, node store.FleetUpgradeNode, status LocalUpgradeStatus) (bool, error) {
	switch status.State {
	case "staged", "waiting_for_idle":
		// A blocker discovered after the fleet-wide staging barrier has not
		// handed anything to root. Keep this node eligible for the same apply
		// step and do not advance to the next node.
		if err := m.database.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "staged", status.RunningVersion, status.AttemptID, status.Failure); err != nil {
			return false, err
		}
		return false, nil
	case "succeeded":
		if status.RunningVersion != run.TargetVersion {
			return true, m.failUpgrade(ctx, run, node, "failed", status.RunningVersion, errors.New("node health reported the wrong manager version"))
		}
		if err := m.database.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "succeeded", status.RunningVersion, status.AttemptID, ""); err != nil {
			return false, err
		}
		return false, nil
	case "rolled_back", "recovery_required", "failed_before_handoff":
		failure := strings.TrimSpace(status.Failure)
		if failure == "" {
			failure = "the node update did not become healthy"
		}
		return true, m.failUpgrade(ctx, run, node, status.State, status.RunningVersion, errors.New(failure))
	default:
		if err := m.database.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, "checking_health", status.RunningVersion, status.AttemptID, status.Failure); err != nil {
			return false, err
		}
		return false, nil
	}
}

func (m *Manager) failUpgrade(ctx context.Context, run store.FleetUpgradeRun, node store.FleetUpgradeNode, state, runningVersion string, cause error) error {
	if runningVersion == "" {
		runningVersion = node.RunningVersion
	}
	failure := cleanUpgradeFailure(cause)
	_ = m.database.UpdateFleetUpgradeNode(ctx, run.RunID, node.NodeID, state, runningVersion, node.AttemptID, failure)
	_ = m.database.UpdateFleetUpgradeRunState(ctx, run.RunID, "failed", fmt.Sprintf("node %s failed: %s", node.DisplayName, failure))
	return cause
}

func cleanUpgradeFailure(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}

func releaseForRun(run store.FleetUpgradeRun) UpgradeRelease {
	return UpgradeRelease{Version: UpgradeProtocolVersion, RunID: run.RunID, ReleaseTag: run.ReleaseTag,
		TargetVersion: run.TargetVersion, ManifestSHA256: run.ManifestSHA256,
		ManifestBytes: append([]byte(nil), run.ManifestBytes...), SignatureBytes: append([]byte(nil), run.SignatureBytes...),
		AssetURL: run.AssetURL}
}

func (m *Manager) stageUpgradeNode(ctx context.Context, run store.FleetUpgradeRun, node store.FleetUpgradeNode) (LocalUpgradeStatus, error) {
	if node.NodeID == m.identity.NodeID {
		return m.upgradeRuntimeValue().StageFleetUpgrade(ctx, releaseForRun(run))
	}
	var response LocalUpgradeStatus
	err := m.callUpgradeNode(ctx, node.NodeID, http.MethodPost, "/internal/fleet/v1/upgrade/stage", releaseForRun(run), &response)
	return response, err
}

func (m *Manager) applyUpgradeNode(ctx context.Context, run store.FleetUpgradeRun, node store.FleetUpgradeNode) (LocalUpgradeStatus, error) {
	request := UpgradeApplyRequest{Version: UpgradeProtocolVersion, RunID: run.RunID, TargetVersion: run.TargetVersion, ManifestSHA256: run.ManifestSHA256}
	if node.NodeID == m.identity.NodeID {
		return m.upgradeRuntimeValue().ApplyFleetUpgrade(ctx, request)
	}
	var response LocalUpgradeStatus
	err := m.callUpgradeNode(ctx, node.NodeID, http.MethodPost, "/internal/fleet/v1/upgrade/apply", request, &response)
	return response, err
}

func (m *Manager) statusUpgradeNode(ctx context.Context, node store.FleetUpgradeNode) (LocalUpgradeStatus, error) {
	if node.NodeID == m.identity.NodeID {
		return m.upgradeRuntimeValue().FleetUpgradeStatus(ctx, node.RunID)
	}
	var response LocalUpgradeStatus
	err := m.callUpgradeNode(ctx, node.NodeID, http.MethodGet, "/internal/fleet/v1/upgrade/status?run_id="+node.RunID, nil, &response)
	return response, err
}

func (m *Manager) finishUpgradeNode(ctx context.Context, run store.FleetUpgradeRun, node store.FleetUpgradeNode) error {
	request := UpgradeFinishRequest{Version: UpgradeProtocolVersion, RunID: run.RunID, TargetVersion: run.TargetVersion}
	if node.NodeID == m.identity.NodeID {
		return m.upgradeRuntimeValue().FinishFleetUpgrade(ctx, request)
	}
	return m.callUpgradeNode(ctx, node.NodeID, http.MethodPost, "/internal/fleet/v1/upgrade/finish", request, &struct{}{})
}

func (m *Manager) callUpgradeNode(ctx context.Context, nodeID, method, endpoint string, request, response any) error {
	nodes, err := m.database.FleetNodes(ctx)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if node.NodeID != nodeID {
			continue
		}
		_, details, err := ParseCertificatePEM(node.Certificate)
		if err != nil {
			return err
		}
		client := m.newClient(details.Fingerprint)
		client.Timeout = 35 * time.Minute
		return callFleetJSON(ctx, client, method, node.NodeURL+endpoint, request, response)
	}
	return os.ErrNotExist
}

func (m *Manager) upgradeStage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	if err := m.authorizeUpgradeController(r); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	var request UpgradeRelease
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateUpgradeRelease(request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	runtime := m.upgradeRuntimeValue()
	if runtime == nil {
		writeFleetError(w, http.StatusServiceUnavailable, errors.New("fleet upgrade runtime is unavailable"))
		return
	}
	status, err := runtime.StageFleetUpgrade(r.Context(), request)
	if err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, status)
}

func (m *Manager) upgradeApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	if err := m.authorizeUpgradeController(r); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	var request UpgradeApplyRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateUpgradeApply(request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	status, err := m.upgradeRuntimeValue().ApplyFleetUpgrade(r.Context(), request)
	if err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, status)
}

func (m *Manager) upgradeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fleetMethodNotAllowed(w)
		return
	}
	if err := m.authorizeUpgradeController(r); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	status, err := m.upgradeRuntimeValue().FleetUpgradeStatus(r.Context(), r.URL.Query().Get("run_id"))
	if err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, status)
}

func (m *Manager) upgradeFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	if err := m.authorizeUpgradeController(r); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	var request UpgradeFinishRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	if err := m.upgradeRuntimeValue().FinishFleetUpgrade(r.Context(), request); err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, struct{}{})
}

func (m *Manager) authorizeUpgradeController(r *http.Request) error {
	config, err := m.database.FleetConfig(r.Context())
	if err != nil {
		return err
	}
	if config.Role != "member" || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
		return errors.New("only this node's adopted controller can manage its upgrade")
	}
	_, details, err := inspectRawCertificate(r.TLS.PeerCertificates[0])
	if err != nil || details.NodeID != config.ControllerNodeID {
		return errors.New("the upgrade caller is not this node's controller")
	}
	return nil
}
