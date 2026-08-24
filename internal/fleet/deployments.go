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
	AdoptIndependentJob(context.Context, recipe.Recipe, string, string) (store.Job, bool, error)
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

// Adoption carries no grant and no reservation, because it claims nothing on
// the target node. It only names the exact recipe the controller believes is
// already installed there, and the deployment record it belongs to. The
// recipe version is the controller's reading of the node's own heartbeat, so
// it is a hint: the node's installed_models row stays the authority and
// refuses the request when the two disagree.
type adoptDeploymentRequest struct {
	NodeID         string `json:"node_id"`
	RecipeID       string `json:"recipe_id"`
	RecipeVersion  int    `json:"recipe_version"`
	DeploymentID   string `json:"deployment_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

type adoptDeploymentResponse struct {
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

// adoptPlacementKey builds the key half of an adopted deployment id. The
// lengths keep a colon inside a node id or a recipe id from building the same
// key as some other pair, and the "adopt:" word keeps this id space apart
// from the create path's "key:" space, so one deployment id can never mean
// two different placements.
func adoptPlacementKey(nodeID, recipeID string) string {
	return fmt.Sprintf("adopt:%d:%s:%d:%s", len(nodeID), nodeID, len(recipeID), recipeID)
}

func (m *Manager) CreateIndependentDeployment(ctx context.Context, request CreateDeploymentRequest) (Deployment, bool, error) {
	if err := m.requireFleetMutationAllowed(ctx); err != nil {
		return Deployment{}, false, err
	}
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
		return Deployment{}, false, m.nodeFailure(ctx, request.NodeID, err)
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
	// The "key:" word keeps this id space apart from the adopt path's
	// "adopt:" space. Changing how a deployment id is derived is safe exactly
	// once, and this is that moment: no console release has ever called this
	// API, so no deployment record exists in the field to be orphaned. After
	// the first shipped console writes one, this derivation is frozen.
	deploymentID := stablePlacementID("deployment_", authority, "key:"+request.IdempotencyKey)
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
	// One model on one Spark is one live record, on this path exactly as on the
	// adoption path. Without this, a second install of a model a Spark already
	// serves would write a second record under a second idempotency key, and
	// the console would then hold two rows and two owner jobs for one serving
	// model. A record whose work has finished holds nothing, so it is
	// superseded rather than refused: an install that failed, and a model this
	// fleet placed and now updates, both have to be able to place it again. The
	// record this key already owns is not a duplicate of itself, so a retry
	// still finishes here.
	if err := m.supersedeDuplicateDeployment(ctx, deploymentID, request.NodeID, selected.ID); err != nil {
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
		return Deployment{}, false, m.nodeFailure(ctx, request.NodeID, err)
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
		return Deployment{}, false, m.nodeFailure(ctx, request.NodeID, err)
	}
	job, jobCreated, err := m.startOnNode(ctx, target, local, grant, request.Intent)
	if err != nil {
		_ = m.abortOnNode(context.Background(), target, local, reservationID)
		_ = m.database.ObserveFleetDeployment(context.Background(), deploymentID, "failed", m.now().UTC().Format(time.RFC3339Nano))
		return Deployment{}, false, m.nodeFailure(ctx, request.NodeID, err)
	}
	observedAt := m.now().UTC().Format(time.RFC3339Nano)
	if err := m.database.SetFleetDeploymentJob(ctx, deploymentID, job.ID, job.State, observedAt); err != nil {
		return Deployment{}, false, err
	}
	createdDeployment.OwnerJobID, createdDeployment.State, createdDeployment.LastObservedAt = job.ID, job.State, observedAt
	return Deployment{FleetDeployment: createdDeployment, Job: &job}, created && jobCreated, nil
}

// AdoptIndependentDeployment writes the deployment record that a model
// installed before fleet placement never got. Such a model has no
// deployment_id anywhere, so the controller cannot reach it through
// /api/v1/fleet/deployments/{id}/{action}. Adoption creates the record and a
// terminal carrier job on the owner node. It starts nothing, stops nothing,
// and reserves nothing: the running container is left exactly as it is.
func (m *Manager) AdoptIndependentDeployment(ctx context.Context, nodeID, recipeID, idempotencyKey string) (Deployment, bool, error) {
	if err := m.requireFleetMutationAllowed(ctx); err != nil {
		return Deployment{}, false, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return Deployment{}, false, errors.New("a valid idempotency key is required")
	}
	// The effective catalogue answers "is this a model this controller knows,
	// and is it a one-Spark model" with the clearest words. It cannot answer
	// "which version runs there", so the exact recipe is resolved below from
	// the node's own report against the catalogue that retains history.
	known, ok := recipe.Find(m.effectiveRecipes(), recipeID)
	if !ok {
		return Deployment{}, false, errors.New("the model is not in this controller's current catalogue")
	}
	if known.Topology.SparkCount != 1 {
		return Deployment{}, false, fmt.Errorf("%s requires %d nodes and cannot use independent placement", known.DisplayName, known.Topology.SparkCount)
	}
	config, err := m.database.FleetConfig(ctx)
	if err != nil {
		return Deployment{}, false, err
	}
	if config.Role == "member" {
		return Deployment{}, false, errors.New("a fleet member cannot adopt a deployment")
	}
	running, err := m.runningModelVersion(ctx, nodeID, recipeID)
	if err != nil {
		return Deployment{}, false, err
	}
	selected, ok := recipe.FindVersion(m.recipes(), recipeID, running)
	if !ok {
		return Deployment{}, false, fmt.Errorf("%s runs version %d of that model, which is not in this controller's catalogue history", m.nodeName(ctx, nodeID), running)
	}
	if selected.Topology.SparkCount != 1 {
		return Deployment{}, false, fmt.Errorf("%s requires %d nodes and cannot use independent placement", selected.DisplayName, selected.Topology.SparkCount)
	}
	fingerprint, err := RecipeFingerprint(selected)
	if err != nil {
		return Deployment{}, false, err
	}
	authority := config.FleetID
	if authority == "" {
		authority = m.identity.NodeID
	}
	// The deployment id comes from the fleet, the node, and the recipe. It
	// never comes from the caller's idempotency key, so two consoles that
	// adopt the same model on the same node reach the same single record.
	// The version is deliberately absent: one model on one node is one
	// deployment, and updating it must not fork the record.
	deploymentID := stablePlacementID("deployment_", authority, adoptPlacementKey(nodeID, recipeID))
	lock := m.placementLock(deploymentID)
	lock.Lock()
	defer lock.Unlock()
	if existing, err := m.database.FleetDeployment(ctx, deploymentID); err == nil {
		// A record that already owns a job is what the caller wants, even if
		// the catalogue moved on since. Hand it back. A record without one is
		// a half-written earlier attempt: fall through and finish it, because
		// refusing here would wedge this model on this node for good.
		if existing.OwnerJobID != "" {
			view, viewErr := m.Deployment(ctx, deploymentID)
			return view, false, viewErr
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Deployment{}, false, err
	}
	if err := m.supersedeDuplicateDeployment(ctx, deploymentID, nodeID, recipeID); err != nil {
		return Deployment{}, false, err
	}
	target, local, err := m.placementNode(ctx, nodeID)
	if err != nil {
		return Deployment{}, false, m.nodeFailure(ctx, nodeID, err)
	}
	job, jobCreated, err := m.adoptOnNode(ctx, target, local, adoptDeploymentRequest{
		NodeID: nodeID, RecipeID: selected.ID, RecipeVersion: selected.Version,
		DeploymentID: deploymentID, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return Deployment{}, false, m.nodeFailure(ctx, nodeID, err)
	}
	adopted, created, err := m.database.CreateFleetDeployment(ctx, store.FleetDeployment{
		DeploymentID: deploymentID, RecipeID: selected.ID, RecipeVersion: selected.Version,
		RecipeFingerprint: fingerprint, TopologyCount: 1, OwnerNodeID: nodeID, State: "adopting",
	}, store.FleetDeploymentNode{NodeID: nodeID})
	if err != nil {
		return Deployment{}, false, err
	}
	observedAt := m.now().UTC().Format(time.RFC3339Nano)
	if err := m.database.SetFleetDeploymentJob(ctx, deploymentID, job.ID, job.State, observedAt); err != nil {
		return Deployment{}, false, err
	}
	adopted.OwnerJobID, adopted.State, adopted.LastObservedAt = job.ID, job.State, observedAt
	return Deployment{FleetDeployment: adopted, Job: &job}, created && jobCreated, nil
}

// runningModelVersion reads the version a node actually serves out of the
// membership overview, which carries that node's own signed heartbeat. The
// effective catalogue holds only the newest version of each model. That is
// the right answer for a fresh install or an update, and the wrong one for a
// model that already runs: its container name, port, and config were built
// from the recipe as it was at install time, not from whatever the catalogue
// has moved on to since (see recipe.FindVersion). Adoption therefore records
// what runs. The reading is still only a hint, because the node's own
// installed_models row is the authority and refuses a version it disagrees
// with.
func (m *Manager) runningModelVersion(ctx context.Context, nodeID, recipeID string) (int, error) {
	summary, err := m.Summary(ctx)
	if err != nil {
		return 0, err
	}
	for _, node := range summary.Nodes {
		if node.NodeID != nodeID {
			continue
		}
		for _, model := range node.InstalledModels {
			if model.RecipeID == recipeID {
				return model.RecipeVersion, nil
			}
		}
		return 0, fmt.Errorf("%s does not report that model as installed", m.nodeName(ctx, nodeID))
	}
	return 0, fmt.Errorf("%s is not in this fleet", m.nodeName(ctx, nodeID))
}

// supersedeDuplicateDeployment keeps one model on one node to one live record.
// Both placement paths call it, because either one can reach a pair the other
// already owns: the create path derives its id from an idempotency key and the
// adopt path from the pair itself, so two ids can name one model on one Spark.
// Two live records would give the console two rows and two owner jobs for one
// serving model.
//
// Only work that is still running holds the pair. A record whose owner job has
// finished, and a record that never got an owner job at all, hold nothing on
// that Spark: the record is this controller's bookkeeping, while the Spark's
// own heartbeat is what says which model really runs there. Such a record is
// therefore marked removed and the new one takes the pair. Refusing instead
// would wedge the model on that Spark after a failed install, with no way out
// of it from the console.
//
// The caller holds the placement lock for the new deployment id, not for the
// one being superseded. Two placements of one model onto one Spark, started at
// the same moment under different idempotency keys, can therefore still both
// go through. That window is what it was before this check existed; every
// other route into it is closed.
func (m *Manager) supersedeDuplicateDeployment(ctx context.Context, deploymentID, nodeID, recipeID string) error {
	stored, err := m.database.FleetDeployments(ctx)
	if err != nil {
		return err
	}
	for _, item := range stored {
		if item.DeploymentID == deploymentID || item.State == "removed" {
			continue
		}
		if item.OwnerNodeID != nodeID || item.RecipeID != recipeID {
			continue
		}
		if m.deploymentWorkIsLive(ctx, item) {
			return fmt.Errorf("%s already has a deployment record for that model", m.nodeName(ctx, nodeID))
		}
		if err := m.database.ObserveFleetDeployment(ctx, item.DeploymentID, "removed", m.now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}

// deploymentWorkIsLive reports whether a record still has work running on its
// Spark. A record with no owner job never reached one, so nothing runs for it.
// Otherwise the owner Spark's own job is the authority, and it is read now
// rather than trusted from the last projection. A Spark that cannot be read
// leaves the last state this controller saw as the best evidence there is.
func (m *Manager) deploymentWorkIsLive(ctx context.Context, stored store.FleetDeployment) bool {
	if stored.OwnerJobID == "" {
		return false
	}
	state := stored.State
	if view, err := m.Deployment(ctx, stored.DeploymentID); err == nil && !view.Stale && view.Job != nil {
		state = view.Job.State
	}
	return !reservationJobTerminal(state)
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

func (m *Manager) adoptOnNode(ctx context.Context, target store.FleetNode, local bool, request adoptDeploymentRequest) (store.Job, bool, error) {
	if local {
		return m.adoptIndependent(ctx, request)
	}
	client, err := m.clientForNode(target)
	if err != nil {
		return store.Job{}, false, err
	}
	var response adoptDeploymentResponse
	err = callFleetJSON(ctx, client, http.MethodPost, target.NodeURL+"/internal/fleet/v1/deployments/adopt", request, &response)
	return response.Job, response.Created, err
}

// adoptIndependent runs on the node that already holds the model. It confirms
// the exact recipe the controller named, then asks the local runtime for the
// carrier job. There is no grant to verify and no reservation to commit,
// because adoption takes no resource this node has not already given.
func (m *Manager) adoptIndependent(ctx context.Context, request adoptDeploymentRequest) (store.Job, bool, error) {
	if request.DeploymentID == "" || request.RecipeID == "" || request.RecipeVersion <= 0 {
		return store.Job{}, false, errors.New("the adoption request is incomplete")
	}
	// The request must name this node, exactly as a reservation prepare must
	// (see validateIndependentPrepare). A record aimed at another node must
	// never be answered here.
	if request.NodeID != m.identity.NodeID {
		return store.Job{}, false, errors.New("the adoption request names another Spark")
	}
	selected, ok := recipe.FindVersion(m.recipes(), request.RecipeID, request.RecipeVersion)
	if !ok || selected.Topology.SparkCount != 1 {
		return store.Job{}, false, errors.New("the model catalogue on this Spark does not hold that exact one-Spark recipe")
	}
	runtime := m.independentRuntime()
	if runtime == nil {
		return store.Job{}, false, errors.New("the manager on this Spark is not ready to adopt placements")
	}
	return runtime.AdoptIndependentJob(ctx, selected, request.DeploymentID, request.IdempotencyKey)
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
	// Read after "<name> could not do this:" as well as on its own, so it
	// names the fleet rather than naming the Spark a second time.
	return store.FleetNode{}, false, errors.New("the fleet does not hold this Spark as an active member")
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
		return reservationPrepareResponse{}, errors.New("the manager on this Spark is not ready to create placements")
	}
	report, ready, err := runtime.PreflightIndependent(ctx, selected, request.ReservationID)
	if err != nil || !ready {
		_ = m.allocator.Abort(ctx, request.ReservationID)
		if err != nil {
			return reservationPrepareResponse{}, err
		}
		return reservationPrepareResponse{}, errors.New("the preflight on this Spark did not pass")
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
		return recipe.Recipe{}, errors.New("the placement request names the wrong fleet authority")
	}
	if request.ManagerVersion != m.version || request.ManagerBuildIdentity != m.buildIdentity || request.CatalogueDigest != m.digest() {
		return recipe.Recipe{}, errors.New(nodeReleaseSkew)
	}
	selected, ok := recipe.FindVersion(m.recipes(), request.RecipeID, request.RecipeVersion)
	if !ok || selected.Topology.SparkCount != 1 {
		return recipe.Recipe{}, errors.New("the model catalogue on this Spark does not hold that exact one-Spark recipe")
	}
	fingerprint, err := RecipeFingerprint(selected)
	if err != nil || fingerprint != request.RecipeFingerprint {
		return recipe.Recipe{}, errors.New("the exact recipe fingerprint on this Spark does not match the placement")
	}
	expected := ClaimsForRecipe(selected, RecipeClaimOptions{
		Kind: ClaimKindIndependent, ReserveDisk: true, Runtime: request.Claims.Runtime,
	})
	request.Claims = canonicalClaims(request.Claims)
	expected = canonicalClaims(expected)
	left, _ := json.Marshal(request.Claims)
	right, _ := json.Marshal(expected)
	if string(left) != string(right) {
		return recipe.Recipe{}, errors.New("the resource claims in the placement do not match the exact recipe on this Spark")
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
		return recipe.Recipe{}, errors.New("the placement grant recipe is not available on this Spark")
	}
	fingerprint, err := RecipeFingerprint(selected)
	if err != nil || fingerprint != grant.RecipeFingerprint {
		return recipe.Recipe{}, errors.New("the placement grant recipe fingerprint does not match the recipe on this Spark")
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
		return store.Job{}, false, errors.New("the manager on this Spark is not ready to create placements")
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
	if err := m.requireFleetMutationAllowed(ctx); err != nil {
		return store.Job{}, err
	}
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
		return store.Job{}, m.nodeFailure(ctx, stored.OwnerNodeID, err)
	}
	var job store.Job
	err = callFleetJSON(ctx, client, http.MethodPost, target.NodeURL+"/internal/fleet/v1/jobs/"+stored.OwnerJobID+"/"+action,
		remoteJobActionRequest{Action: action, IdempotencyKey: idempotencyKey, Intent: intent}, &job)
	if err != nil {
		return store.Job{}, m.nodeFailure(ctx, stored.OwnerNodeID, err)
	}
	return job, m.advanceDeploymentJob(ctx, stored, job)
}

// ReleaseDeployment ends a record this fleet can no longer act on. It is the
// fallback behind the console's Clear tool, and it is the only operation here
// that touches nothing but this controller's own bookkeeping: no Spark is
// asked to do anything, and no model is stopped or removed anywhere.
//
// It exists because a record can outlive the job it names. The owner Spark can
// lose the job row, leave the fleet, or never have been given a job at all
// after a half-finished create. The record then pins its console row to "No
// answer" with every button on it dead, and adoption cannot write a fresh
// record while it stands. Ending it lets adopt-on-demand rebuild the row from
// what that Spark reports, which is the authority anyway.
//
// A record whose job this controller can still read is refused. That one is
// not stranded, so the owner is told to remove the model rather than throw
// away a record the fleet still uses.
func (m *Manager) ReleaseDeployment(ctx context.Context, deploymentID string) (Deployment, bool, error) {
	if err := m.requireFleetMutationAllowed(ctx); err != nil {
		return Deployment{}, false, err
	}
	config, err := m.database.FleetConfig(ctx)
	if err != nil {
		return Deployment{}, false, err
	}
	if config.Role == "member" {
		return Deployment{}, false, errors.New("a fleet member cannot clear a deployment record")
	}
	lock := m.placementLock(deploymentID)
	lock.Lock()
	defer lock.Unlock()
	// Deployment reads the owner job from the Spark that holds it, so this one
	// call answers both questions: whether the record is still reachable, and
	// what to hand back.
	view, err := m.Deployment(ctx, deploymentID)
	if err != nil {
		return Deployment{}, false, err
	}
	if view.State == "removed" {
		return view, false, nil
	}
	if view.Job != nil && !view.Stale {
		return Deployment{}, false, fmt.Errorf("%s answers for this model, so remove the model rather than clear the record", m.nodeName(ctx, view.OwnerNodeID))
	}
	observedAt := m.now().UTC().Format(time.RFC3339Nano)
	if err := m.database.ObserveFleetDeployment(ctx, deploymentID, "removed", observedAt); err != nil {
		return Deployment{}, false, err
	}
	stored, err := m.database.FleetDeployment(ctx, deploymentID)
	if err != nil {
		return Deployment{}, false, err
	}
	return Deployment{FleetDeployment: stored}, true, nil
}

func (m *Manager) advanceDeploymentJob(ctx context.Context, deployment store.FleetDeployment, job store.Job) error {
	return m.database.AdvanceFleetDeploymentJob(ctx, deployment.DeploymentID, deployment.OwnerJobID, job.ID, job.State, m.now().UTC().Format(time.RFC3339Nano))
}
