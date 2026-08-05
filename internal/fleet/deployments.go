package fleet

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

type IndependentIntent struct {
	Confirmed                   bool `json:"confirmed"`
	AcceptLicence               bool `json:"accept_licence"`
	ConfirmTerritoryEligibility bool `json:"confirm_territory_eligibility"`
	Activate                    bool `json:"activate"`
	RemoveArtifacts             bool `json:"remove_artifacts,omitempty"`
}

// IndependentRuntime is implemented by the local HTTP and engine layer on
// every manager. The fleet package sends owner intent and exact recipe
// identity through this seam, while the target remains authoritative for its
// preflight, acceptance records, job row, switch, rollback, and receipts.
type IndependentRuntime interface {
	PreflightIndependent(context.Context, recipe.Recipe, string) (json.RawMessage, bool, error)
	CreateIndependentJob(context.Context, recipe.Recipe, IndependentIntent, string, string, string) (store.Job, bool, error)
	IndependentJob(context.Context, string) (store.Job, error)
	IndependentAction(context.Context, store.Job, string, string, IndependentIntent) (store.Job, error)
}

type CreateDeploymentRequest struct {
	RecipeID       string            `json:"recipe_id"`
	NodeID         string            `json:"node_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	Intent         IndependentIntent `json:"intent"`
}

type Deployment struct {
	store.FleetDeployment
	Job   *store.Job `json:"job,omitempty"`
	Stale bool       `json:"stale"`
}

type placementGrant struct {
	Version           int    `json:"version"`
	FleetID           string `json:"fleet_id"`
	ControllerNodeID  string `json:"controller_node_id"`
	DriverNodeID      string `json:"driver_node_id"`
	DeploymentID      string `json:"deployment_id"`
	NodeID            string `json:"node_id"`
	ReservationID     string `json:"reservation_id"`
	RecipeID          string `json:"recipe_id"`
	RecipeVersion     int    `json:"recipe_version"`
	RecipeFingerprint string `json:"recipe_fingerprint"`
	IdempotencyKey    string `json:"idempotency_key"`
	IssuedAt          string `json:"issued_at"`
	ExpiresAt         string `json:"expires_at"`
	Signature         []byte `json:"signature"`
}

func (grant placementGrant) signedBytes() ([]byte, error) {
	grant.Signature = nil
	return json.Marshal(grant)
}

func (m *Manager) signGrant(grant placementGrant) (placementGrant, error) {
	payload, err := grant.signedBytes()
	if err != nil {
		return placementGrant{}, err
	}
	grant.Signature = m.identity.Sign(payload)
	return grant, nil
}

type reservationPrepareRequest struct {
	Version              int    `json:"version"`
	FleetID              string `json:"fleet_id"`
	ControllerNodeID     string `json:"controller_node_id"`
	RequestID            string `json:"request_id"`
	DeploymentID         string `json:"deployment_id"`
	ReservationID        string `json:"reservation_id"`
	NodeID               string `json:"node_id"`
	RecipeID             string `json:"recipe_id"`
	RecipeVersion        int    `json:"recipe_version"`
	RecipeFingerprint    string `json:"recipe_fingerprint"`
	ManagerVersion       string `json:"manager_version"`
	ManagerBuildIdentity string `json:"manager_build_identity"`
	CatalogueDigest      string `json:"catalogue_digest"`
	Claims               Claims `json:"claims"`
}

type reservationPrepareResponse struct {
	PrepareToken string          `json:"prepare_token"`
	PreparedAt   string          `json:"prepared_at"`
	ExpiresAt    string          `json:"expires_at"`
	Preflight    json.RawMessage `json:"preflight"`
}

type reservationCommitRequest struct {
	PrepareToken string         `json:"prepare_token"`
	Grant        placementGrant `json:"grant"`
}

type reservationIDRequest struct {
	ReservationID string `json:"reservation_id"`
}

type independentDeploymentRequest struct {
	Grant  placementGrant    `json:"grant"`
	Intent IndependentIntent `json:"intent"`
}

type independentDeploymentResponse struct {
	Job     store.Job `json:"job"`
	Created bool      `json:"created"`
}

type remoteJobActionRequest struct {
	Action         string            `json:"action"`
	IdempotencyKey string            `json:"idempotency_key"`
	Intent         IndependentIntent `json:"intent"`
}

func stablePlacementID(prefix, authority, key string) string {
	digest := sha256.Sum256([]byte(authority + "\x00" + key))
	return prefix + hex.EncodeToString(digest[:16])
}

func (m *Manager) CreateIndependentDeployment(ctx context.Context, request CreateDeploymentRequest) (Deployment, bool, error) {
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		return Deployment{}, false, errors.New("a valid idempotency key is required")
	}
	if !request.Intent.Confirmed {
		return Deployment{}, false, errors.New("explicit placement confirmation is required")
	}
	plan, err := m.PlanIndependent(ctx, request.RecipeID)
	if err != nil {
		return Deployment{}, false, err
	}
	if _, err := plan.candidate(request.NodeID); err != nil {
		return Deployment{}, false, fmt.Errorf("node %s: %w", request.NodeID, err)
	}
	selected := plan.selected
	if selected.ID != plan.RecipeID || selected.Version != plan.RecipeVersion {
		return Deployment{}, false, errors.New("the exact planned recipe is no longer available")
	}
	selectedFingerprint, err := RecipeFingerprint(selected)
	if err != nil || selectedFingerprint != plan.RecipeFingerprint {
		return Deployment{}, false, errors.New("the exact planned recipe changed before placement")
	}
	config, err := m.database.FleetConfig(ctx)
	if err != nil {
		return Deployment{}, false, err
	}
	authority := config.FleetID
	if authority == "" {
		authority = m.identity.NodeID
	}
	deploymentID := stablePlacementID("deployment_", authority, request.IdempotencyKey)
	reservationID := stablePlacementID("reservation_", request.NodeID, deploymentID)
	lock := m.placementLock(deploymentID)
	lock.Lock()
	defer lock.Unlock()
	if existing, err := m.database.FleetDeployment(ctx, deploymentID); err == nil {
		if existing.RecipeID != plan.RecipeID || existing.RecipeVersion != plan.RecipeVersion ||
			existing.RecipeFingerprint != plan.RecipeFingerprint || existing.OwnerNodeID != request.NodeID {
			return Deployment{}, false, errors.New("the idempotency key was retried with different placement details")
		}
		if existing.OwnerJobID != "" || existing.State == "failed" {
			view, viewErr := m.Deployment(ctx, deploymentID)
			return view, false, viewErr
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Deployment{}, false, err
	}
	claims := ClaimsForRecipe(selected, RecipeClaimOptions{
		Kind: ClaimKindIndependent, ReserveDisk: true, Runtime: request.Intent.Activate,
	})
	prepareRequest := reservationPrepareRequest{
		Version: ProtocolVersion, FleetID: config.FleetID, ControllerNodeID: m.identity.NodeID,
		RequestID: deploymentID, DeploymentID: deploymentID, ReservationID: reservationID,
		NodeID: request.NodeID, RecipeID: selected.ID, RecipeVersion: selected.Version,
		RecipeFingerprint: plan.RecipeFingerprint, ManagerVersion: m.version,
		ManagerBuildIdentity: m.buildIdentity, CatalogueDigest: m.digest(), Claims: claims,
	}
	prepared, target, local, err := m.prepareOnNode(ctx, prepareRequest)
	if err != nil {
		return Deployment{}, false, fmt.Errorf("node %s refused placement: %w", request.NodeID, err)
	}
	createdDeployment, created, err := m.database.CreateFleetDeployment(ctx, store.FleetDeployment{
		DeploymentID: deploymentID, RecipeID: selected.ID, RecipeVersion: selected.Version,
		RecipeFingerprint: plan.RecipeFingerprint, TopologyCount: 1, OwnerNodeID: request.NodeID, State: "committing",
	}, store.FleetDeploymentNode{NodeID: request.NodeID, ReservationID: reservationID})
	if err != nil {
		_ = m.abortOnNode(context.Background(), target, local, reservationID)
		return Deployment{}, false, err
	}
	grant, err := m.signGrant(placementGrant{
		Version: ProtocolVersion, FleetID: config.FleetID, ControllerNodeID: m.identity.NodeID,
		DriverNodeID: request.NodeID, DeploymentID: deploymentID, NodeID: request.NodeID,
		ReservationID: reservationID, RecipeID: selected.ID, RecipeVersion: selected.Version,
		RecipeFingerprint: plan.RecipeFingerprint, IdempotencyKey: request.IdempotencyKey,
		IssuedAt: prepared.PreparedAt, ExpiresAt: prepared.ExpiresAt,
	})
	if err != nil {
		_ = m.abortOnNode(context.Background(), target, local, reservationID)
		return Deployment{}, false, err
	}
	if err := m.commitOnNode(ctx, target, local, prepared.PrepareToken, grant); err != nil {
		_ = m.abortOnNode(context.Background(), target, local, reservationID)
		_ = m.database.ObserveFleetDeployment(context.Background(), deploymentID, "failed", m.now().UTC().Format(time.RFC3339Nano))
		return Deployment{}, false, fmt.Errorf("node %s did not commit placement: %w", request.NodeID, err)
	}
	job, jobCreated, err := m.startOnNode(ctx, target, local, grant, request.Intent)
	if err != nil {
		_ = m.abortOnNode(context.Background(), target, local, reservationID)
		_ = m.database.ObserveFleetDeployment(context.Background(), deploymentID, "failed", m.now().UTC().Format(time.RFC3339Nano))
		return Deployment{}, false, fmt.Errorf("node %s did not create its deployment job: %w", request.NodeID, err)
	}
	observedAt := m.now().UTC().Format(time.RFC3339Nano)
	if err := m.database.SetFleetDeploymentJob(ctx, deploymentID, job.ID, job.State, observedAt); err != nil {
		return Deployment{}, false, err
	}
	createdDeployment.OwnerJobID, createdDeployment.State, createdDeployment.LastObservedAt = job.ID, job.State, observedAt
	return Deployment{FleetDeployment: createdDeployment, Job: &job}, created && jobCreated, nil
}

func (m *Manager) placementLock(deploymentID string) *sync.Mutex {
	m.placementMu.Lock()
	defer m.placementMu.Unlock()
	lock := m.placementLocks[deploymentID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.placementLocks[deploymentID] = lock
	}
	return lock
}

func (m *Manager) prepareOnNode(ctx context.Context, request reservationPrepareRequest) (reservationPrepareResponse, store.FleetNode, bool, error) {
	target, local, err := m.placementNode(ctx, request.NodeID)
	if err != nil {
		return reservationPrepareResponse{}, store.FleetNode{}, false, err
	}
	if local {
		prepared, err := m.prepareIndependent(ctx, request)
		return prepared, target, true, err
	}
	client, err := m.clientForNode(target)
	if err != nil {
		return reservationPrepareResponse{}, store.FleetNode{}, false, err
	}
	var response reservationPrepareResponse
	err = callFleetJSON(ctx, client, http.MethodPost, target.NodeURL+"/internal/fleet/v1/reservations/prepare", request, &response)
	return response, target, false, err
}

func (m *Manager) commitOnNode(ctx context.Context, target store.FleetNode, local bool, token string, grant placementGrant) error {
	if local {
		return m.commitIndependent(ctx, token, grant)
	}
	client, err := m.clientForNode(target)
	if err != nil {
		return err
	}
	return callFleetJSON(ctx, client, http.MethodPost, target.NodeURL+"/internal/fleet/v1/reservations/commit", reservationCommitRequest{PrepareToken: token, Grant: grant}, &struct{}{})
}

func (m *Manager) abortOnNode(ctx context.Context, target store.FleetNode, local bool, reservationID string) error {
	if local {
		return m.allocator.Abort(ctx, reservationID)
	}
	client, err := m.clientForNode(target)
	if err != nil {
		return err
	}
	return callFleetJSON(ctx, client, http.MethodPost, target.NodeURL+"/internal/fleet/v1/reservations/abort", reservationIDRequest{ReservationID: reservationID}, &struct{}{})
}

func (m *Manager) startOnNode(ctx context.Context, target store.FleetNode, local bool, grant placementGrant, intent IndependentIntent) (store.Job, bool, error) {
	if local {
		return m.startIndependent(ctx, grant, intent)
	}
	client, err := m.clientForNode(target)
	if err != nil {
		return store.Job{}, false, err
	}
	var response independentDeploymentResponse
	err = callFleetJSON(ctx, client, http.MethodPost, target.NodeURL+"/internal/fleet/v1/deployments/independent", independentDeploymentRequest{Grant: grant, Intent: intent}, &response)
	return response.Job, response.Created, err
}

func (m *Manager) placementNode(ctx context.Context, nodeID string) (store.FleetNode, bool, error) {
	if nodeID == m.identity.NodeID {
		config, err := m.database.FleetConfig(ctx)
		if err != nil {
			return store.FleetNode{}, false, err
		}
		return m.selfNode(config.FleetID), true, nil
	}
	nodes, err := m.database.FleetNodes(ctx)
	if err != nil {
		return store.FleetNode{}, false, err
	}
	for _, node := range nodes {
		if node.NodeID == nodeID && node.MembershipState == "active" {
			return node, false, nil
		}
	}
	return store.FleetNode{}, false, errors.New("the selected node is not an active fleet member")
}

func (m *Manager) clientForNode(node store.FleetNode) (*http.Client, error) {
	_, details, err := ParseCertificatePEM(node.Certificate)
	if err != nil || details.NodeID != node.NodeID {
		return nil, errors.New("the selected node certificate does not match its stored identity")
	}
	return m.newClient(details.Fingerprint), nil
}

func (m *Manager) prepareIndependent(ctx context.Context, request reservationPrepareRequest) (reservationPrepareResponse, error) {
	selected, err := m.validateIndependentPrepare(ctx, request)
	if err != nil {
		return reservationPrepareResponse{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return reservationPrepareResponse{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(m.identity.Sign(payload))
	expires := m.now().Add(10 * time.Minute)
	reservation, _, err := m.allocator.Prepare(ctx, ReservationRequest{
		ReservationID: request.ReservationID, DeploymentID: request.DeploymentID,
		FleetID: request.FleetID, ControllerNodeID: request.ControllerNodeID, DriverNodeID: request.NodeID,
		RecipeID: request.RecipeID, RecipeVersion: request.RecipeVersion, RecipeFingerprint: request.RecipeFingerprint,
		Claims: request.Claims, PrepareToken: token, ExpiresAt: expires,
	})
	if err != nil {
		return reservationPrepareResponse{}, err
	}
	runtime := m.independentRuntime()
	if runtime == nil {
		_ = m.allocator.Abort(ctx, request.ReservationID)
		return reservationPrepareResponse{}, errors.New("the target manager is not ready to create placements")
	}
	report, ready, err := runtime.PreflightIndependent(ctx, selected, request.ReservationID)
	if err != nil || !ready {
		_ = m.allocator.Abort(ctx, request.ReservationID)
		if err != nil {
			return reservationPrepareResponse{}, err
		}
		return reservationPrepareResponse{}, errors.New("the target node's preflight did not pass")
	}
	return reservationPrepareResponse{PrepareToken: token, PreparedAt: reservation.CreatedAt, ExpiresAt: reservation.ExpiresAt, Preflight: report}, nil
}

func (m *Manager) validateIndependentPrepare(ctx context.Context, request reservationPrepareRequest) (recipe.Recipe, error) {
	if request.Version != ProtocolVersion || request.NodeID != m.identity.NodeID || request.DeploymentID == "" || request.ReservationID == "" {
		return recipe.Recipe{}, errors.New("independent reservation protocol fields are invalid")
	}
	config, err := m.database.FleetConfig(ctx)
	if err != nil {
		return recipe.Recipe{}, err
	}
	expectedController := config.ControllerNodeID
	if config.Role == "standalone" {
		expectedController = m.identity.NodeID
	}
	if request.FleetID != config.FleetID || request.ControllerNodeID != expectedController {
		return recipe.Recipe{}, errors.New("the placement grant authority does not match this node's fleet")
	}
	if request.ManagerVersion != m.version || request.ManagerBuildIdentity != m.buildIdentity || request.CatalogueDigest != m.digest() {
		return recipe.Recipe{}, errors.New("the target node does not exactly match the controller release and catalogue")
	}
	selected, ok := recipe.FindVersion(m.recipes(), request.RecipeID, request.RecipeVersion)
	if !ok || selected.Topology.SparkCount != 1 {
		return recipe.Recipe{}, errors.New("the target node does not hold that exact independent recipe")
	}
	fingerprint, err := RecipeFingerprint(selected)
	if err != nil || fingerprint != request.RecipeFingerprint {
		return recipe.Recipe{}, errors.New("the target node's exact recipe fingerprint does not match the placement")
	}
	expected := ClaimsForRecipe(selected, RecipeClaimOptions{
		Kind: ClaimKindIndependent, ReserveDisk: true, Runtime: request.Claims.Runtime,
	})
	request.Claims = canonicalClaims(request.Claims)
	expected = canonicalClaims(expected)
	left, _ := json.Marshal(request.Claims)
	right, _ := json.Marshal(expected)
	if string(left) != string(right) {
		return recipe.Recipe{}, errors.New("the target node's resource claims do not match its exact recipe")
	}
	return selected, nil
}

func (m *Manager) verifyGrant(ctx context.Context, grant placementGrant) (recipe.Recipe, error) {
	if grant.Version != ProtocolVersion || grant.NodeID != m.identity.NodeID || grant.ReservationID == "" || grant.DeploymentID == "" || len(grant.Signature) == 0 {
		return recipe.Recipe{}, errors.New("the placement grant is incomplete")
	}
	config, err := m.database.FleetConfig(ctx)
	if err != nil {
		return recipe.Recipe{}, err
	}
	expectedController := config.ControllerNodeID
	if config.Role == "standalone" {
		expectedController = m.identity.NodeID
	}
	if grant.FleetID != config.FleetID || grant.ControllerNodeID != expectedController {
		return recipe.Recipe{}, errors.New("the placement grant names the wrong fleet authority")
	}
	_, controller, err := ParseCertificatePEM(config.ControllerCertificate)
	if err != nil {
		if grant.ControllerNodeID == m.identity.NodeID && config.Role != "member" {
			controller.PublicKey = m.identity.PublicKey
		} else {
			return recipe.Recipe{}, errors.New("the controller certificate is unavailable")
		}
	}
	payload, err := grant.signedBytes()
	if err != nil || !ed25519.Verify(controller.PublicKey, payload, grant.Signature) {
		return recipe.Recipe{}, errors.New("the placement grant signature is invalid")
	}
	expires, err := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	if err != nil || !m.now().Before(expires) {
		return recipe.Recipe{}, errors.New("the placement grant has expired")
	}
	selected, ok := recipe.FindVersion(m.recipes(), grant.RecipeID, grant.RecipeVersion)
	if !ok || selected.Topology.SparkCount != 1 {
		return recipe.Recipe{}, errors.New("the placement grant recipe is not available on this node")
	}
	fingerprint, err := RecipeFingerprint(selected)
	if err != nil || fingerprint != grant.RecipeFingerprint {
		return recipe.Recipe{}, errors.New("the placement grant recipe fingerprint does not match this node")
	}
	return selected, nil
}

func (m *Manager) commitIndependent(ctx context.Context, token string, grant placementGrant) error {
	if _, err := m.verifyGrant(ctx, grant); err != nil {
		return err
	}
	payload, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	_, err = m.allocator.Commit(ctx, grant.ReservationID, token, payload)
	return err
}

func (m *Manager) startIndependent(ctx context.Context, grant placementGrant, intent IndependentIntent) (store.Job, bool, error) {
	selected, err := m.verifyGrant(ctx, grant)
	if err != nil {
		return store.Job{}, false, err
	}
	reservation, err := m.allocator.Reservation(ctx, grant.ReservationID)
	if err != nil || (reservation.State != "committed" && reservation.State != "active") || reservation.DeploymentID != grant.DeploymentID {
		return store.Job{}, false, errors.New("the independent deployment reservation is not committed")
	}
	runtime := m.independentRuntime()
	if runtime == nil {
		return store.Job{}, false, errors.New("the target manager is not ready to create placements")
	}
	return runtime.CreateIndependentJob(ctx, selected, intent, grant.ReservationID, grant.DeploymentID, "fleet:"+grant.DeploymentID)
}

func (m *Manager) Deployment(ctx context.Context, deploymentID string) (Deployment, error) {
	stored, err := m.database.FleetDeployment(ctx, deploymentID)
	if err != nil {
		return Deployment{}, err
	}
	view := Deployment{FleetDeployment: stored}
	if stored.OwnerJobID == "" {
		return view, nil
	}
	target, local, err := m.placementNode(ctx, stored.OwnerNodeID)
	if err != nil {
		view.Stale = true
		return view, nil
	}
	job, err := m.jobOnNode(ctx, target, local, stored.OwnerJobID)
	if err != nil {
		view.Stale = true
		return view, nil
	}
	view.Job = &job
	view.State, view.LastObservedAt = job.State, m.now().UTC().Format(time.RFC3339Nano)
	_ = m.database.ObserveFleetDeployment(ctx, deploymentID, job.State, view.LastObservedAt)
	return view, nil
}

func (m *Manager) Deployments(ctx context.Context) ([]Deployment, error) {
	stored, err := m.database.FleetDeployments(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Deployment, 0, len(stored))
	for _, item := range stored {
		view, err := m.Deployment(ctx, item.DeploymentID)
		if err != nil {
			return nil, err
		}
		result = append(result, view)
	}
	return result, nil
}

func (m *Manager) jobOnNode(ctx context.Context, target store.FleetNode, local bool, jobID string) (store.Job, error) {
	if local {
		runtime := m.independentRuntime()
		if runtime == nil {
			return store.Job{}, errors.New("the local placement runtime is unavailable")
		}
		return runtime.IndependentJob(ctx, jobID)
	}
	client, err := m.clientForNode(target)
	if err != nil {
		return store.Job{}, err
	}
	var job store.Job
	err = callFleetJSON(ctx, client, http.MethodGet, target.NodeURL+"/internal/fleet/v1/jobs/"+jobID, nil, &job)
	return job, err
}

func (m *Manager) ActionDeployment(ctx context.Context, deploymentID, action, idempotencyKey string, intent IndependentIntent) (store.Job, error) {
	allowed := map[string]bool{"start": true, "stop": true, "remove": true, "cancel": true, "smoke-test": true, "benchmark": true}
	if !allowed[action] {
		return store.Job{}, errors.New("the deployment action is not supported")
	}
	config, err := m.database.FleetConfig(ctx)
	if err != nil {
		return store.Job{}, err
	}
	if config.Role == "member" {
		return store.Job{}, errors.New("a fleet member cannot issue deployment actions")
	}
	lock := m.placementLock(deploymentID)
	lock.Lock()
	defer lock.Unlock()
	stored, err := m.database.FleetDeployment(ctx, deploymentID)
	if err != nil {
		return store.Job{}, err
	}
	target, local, err := m.placementNode(ctx, stored.OwnerNodeID)
	if err != nil {
		return store.Job{}, err
	}
	if local {
		runtime := m.independentRuntime()
		if runtime == nil {
			return store.Job{}, errors.New("the local placement runtime is unavailable")
		}
		owner, err := runtime.IndependentJob(ctx, stored.OwnerJobID)
		if err != nil {
			return store.Job{}, err
		}
		job, err := runtime.IndependentAction(ctx, owner, action, idempotencyKey, intent)
		if err != nil {
			return store.Job{}, err
		}
		return job, m.advanceDeploymentJob(ctx, stored, job)
	}
	client, err := m.clientForNode(target)
	if err != nil {
		return store.Job{}, err
	}
	var job store.Job
	err = callFleetJSON(ctx, client, http.MethodPost, target.NodeURL+"/internal/fleet/v1/jobs/"+stored.OwnerJobID+"/"+action,
		remoteJobActionRequest{Action: action, IdempotencyKey: idempotencyKey, Intent: intent}, &job)
	if err != nil {
		return store.Job{}, err
	}
	return job, m.advanceDeploymentJob(ctx, stored, job)
}

func (m *Manager) advanceDeploymentJob(ctx context.Context, deployment store.FleetDeployment, job store.Job) error {
	return m.database.AdvanceFleetDeploymentJob(ctx, deployment.DeploymentID, deployment.OwnerJobID, job.ID, job.State, m.now().UTC().Format(time.RFC3339Nano))
}
