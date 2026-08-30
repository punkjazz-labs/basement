package fleet

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/punkjazz-labs/basement/internal/inventory"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

const (
	joinCodeLifetime        = 10 * time.Minute
	joinPreparationLifetime = 5 * time.Minute
)

type Options struct {
	DataDir          string
	Database         *store.Store
	Inventory        inventory.Provider
	Version          string
	BuildIdentity    string
	DisplayName      string
	ConsoleURL       string
	NodeURL          string
	Recipes          []recipe.Recipe
	EffectiveRecipes []recipe.Recipe
}

type Manager struct {
	identity      *Identity
	database      *store.Store
	allocator     *Allocator
	inventory     inventory.Provider
	version       string
	buildIdentity string
	displayName   string
	consoleURL    string
	nodeURL       string
	now           func() time.Time
	// newClient is the pinned fleet transport constructor. Production keeps
	// the TLS implementation assigned by NewManager; tests replace it with an
	// in-memory round trip so they never bind a socket or contact a machine.
	newClient func(string) *http.Client
	// clients are retained per certificate fingerprint so the controller's
	// ten-second heartbeat does not allocate a new transport and abandon its
	// connection pool on every poll. The fingerprint is part of the key: a
	// changed member certificate must get a new pinned transport.
	clientMu sync.Mutex
	clients  map[string]*http.Client
	// newFirstContactClient is the same seam for the one call that has no pin
	// yet: the invitation that opens an addition. It reports the fingerprint
	// the address presented, which becomes the pin for everything after it.
	newFirstContactClient func(func(string)) *http.Client

	// invitations are the additions this node has been asked to accept, and
	// attempts are the additions it is driving. Both are in memory: they are
	// one conversation a person is watching, not a record. See invitation.go.
	invitationMu sync.Mutex
	invitations  map[string]*invitation
	attemptMu    sync.Mutex
	attempts     map[string]*inviteAttempt

	catalogueMu     sync.RWMutex
	catalogueDigest string
	catalogue       []recipe.Recipe
	effective       []recipe.Recipe
	runtime         IndependentRuntime
	power           PowerRuntime
	placementMu     sync.Mutex
	placementLocks  map[string]*sync.Mutex
	// The pair locks sit one level above the id locks. See placementPairLock
	// for what a pair is and for the order the two locks are taken in.
	placementPairMu    sync.Mutex
	placementPairLocks map[string]*sync.Mutex

	upgradeRuntime UpgradeRuntime
	upgradeMu      sync.Mutex
	upgradeRunning bool
	upgradeRetry   time.Duration
	// These calls are seams for deterministic rolling-upgrade tests. Production
	// assigns the mutual TLS and local-runtime implementations once in
	// NewManager and never reassigns them.
	upgradeStageCall   func(context.Context, store.FleetUpgradeRun, store.FleetUpgradeNode) (LocalUpgradeStatus, error)
	upgradeApplyCall   func(context.Context, store.FleetUpgradeRun, store.FleetUpgradeNode) (LocalUpgradeStatus, error)
	upgradeStatusCall  func(context.Context, store.FleetUpgradeNode) (LocalUpgradeStatus, error)
	upgradeFinishCall  func(context.Context, store.FleetUpgradeRun, store.FleetUpgradeNode) error
	upgradeResolveCall func(context.Context, store.FleetUpgradeRun, store.FleetUpgradeNode) error
}

func NewManager(ctx context.Context, options Options) (*Manager, error) {
	if options.Database == nil || options.Inventory == nil {
		return nil, errors.New("fleet manager requires a store and inventory provider")
	}
	identity, err := OpenIdentity(ctx, options.DataDir, options.Database)
	if err != nil {
		return nil, err
	}
	consoleURL, err := normalizeOrigin(options.ConsoleURL, false)
	if err != nil {
		return nil, fmt.Errorf("console URL: %w", err)
	}
	nodeURL, err := normalizeOrigin(options.NodeURL, true)
	if err != nil {
		return nil, fmt.Errorf("node URL: %w", err)
	}
	digest, err := CatalogueDigest(options.Recipes)
	if err != nil {
		return nil, fmt.Errorf("catalogue digest: %w", err)
	}
	displayName := strings.TrimSpace(options.DisplayName)
	if displayName == "" {
		displayName = "this node"
	}
	manager := &Manager{
		identity: identity, database: options.Database, allocator: NewAllocator(options.Database, identity.NodeID), inventory: options.Inventory,
		version: options.Version, buildIdentity: options.BuildIdentity, displayName: displayName,
		consoleURL: consoleURL, nodeURL: nodeURL, now: time.Now, catalogueDigest: digest,
		catalogue:          append([]recipe.Recipe(nil), options.Recipes...),
		effective:          append([]recipe.Recipe(nil), options.EffectiveRecipes...),
		placementLocks:     make(map[string]*sync.Mutex),
		placementPairLocks: make(map[string]*sync.Mutex),
		upgradeRetry:       time.Second,
		invitations:        make(map[string]*invitation),
		attempts:           make(map[string]*inviteAttempt),
		clients:            make(map[string]*http.Client),
	}
	if len(manager.effective) == 0 {
		manager.effective = append([]recipe.Recipe(nil), options.Recipes...)
	}
	manager.newClient = manager.clientForFingerprint
	manager.newFirstContactClient = manager.firstContactClient
	manager.upgradeStageCall = manager.stageUpgradeNode
	manager.upgradeApplyCall = manager.applyUpgradeNode
	manager.upgradeStatusCall = manager.statusUpgradeNode
	manager.upgradeFinishCall = manager.finishUpgradeNode
	manager.upgradeResolveCall = manager.resolveUpgradeNode
	if err := manager.initializeLegacyState(ctx, options.Recipes); err != nil {
		return nil, err
	}
	return manager, nil
}

func BinaryBuildIdentity(version string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return version + ":" + hex.EncodeToString(digest.Sum(nil)), nil
}

func RecipeFingerprint(value recipe.Recipe) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func (m *Manager) initializeLegacyState(ctx context.Context, recipes []recipe.Recipe) error {
	models, err := m.database.Models(ctx)
	if err != nil {
		return err
	}
	var candidates []store.LegacyDeploymentCandidate
	for _, model := range models {
		if !model.Active {
			continue
		}
		selected, ok := recipe.FindVersion(recipes, model.RecipeID, model.RecipeVersion)
		if !ok || !selected.Distributed() {
			continue
		}
		fingerprint, err := RecipeFingerprint(selected)
		if err != nil {
			return err
		}
		candidates = append(candidates, store.LegacyDeploymentCandidate{RecipeID: selected.ID, RecipeVersion: selected.Version, RecipeFingerprint: fingerprint, TopologyCount: selected.Topology.SparkCount})
	}
	return m.database.InitializeFleetMigration(ctx, m.selfNode(""), candidates)
}

func (m *Manager) Identity() *Identity { return m.identity }

func (m *Manager) Allocator() *Allocator { return m.allocator }

func (m *Manager) SetRecipes(all, effective []recipe.Recipe) error {
	digest, err := CatalogueDigest(all)
	if err != nil {
		return err
	}
	m.catalogueMu.Lock()
	m.catalogueDigest = digest
	m.catalogue = append([]recipe.Recipe(nil), all...)
	m.effective = append([]recipe.Recipe(nil), effective...)
	m.catalogueMu.Unlock()
	return nil
}

func (m *Manager) SetIndependentRuntime(runtime IndependentRuntime) {
	m.catalogueMu.Lock()
	m.runtime = runtime
	m.catalogueMu.Unlock()
}

func (m *Manager) SetUpgradeRuntime(runtime UpgradeRuntime) {
	m.catalogueMu.Lock()
	m.upgradeRuntime = runtime
	m.catalogueMu.Unlock()
}

// SetPowerRuntime wires this node's own GPU power boundary (see power.go). A
// manager without one still reports every node's mode from the heartbeat; it
// only cannot change its own.
func (m *Manager) SetPowerRuntime(runtime PowerRuntime) {
	m.catalogueMu.Lock()
	m.power = runtime
	m.catalogueMu.Unlock()
}

func (m *Manager) powerRuntime() PowerRuntime {
	m.catalogueMu.RLock()
	defer m.catalogueMu.RUnlock()
	return m.power
}

func (m *Manager) upgradeRuntimeValue() UpgradeRuntime {
	m.catalogueMu.RLock()
	defer m.catalogueMu.RUnlock()
	return m.upgradeRuntime
}

func (m *Manager) independentRuntime() IndependentRuntime {
	m.catalogueMu.RLock()
	defer m.catalogueMu.RUnlock()
	return m.runtime
}

func (m *Manager) recipes() []recipe.Recipe {
	m.catalogueMu.RLock()
	defer m.catalogueMu.RUnlock()
	return append([]recipe.Recipe(nil), m.catalogue...)
}

func (m *Manager) effectiveRecipes() []recipe.Recipe {
	m.catalogueMu.RLock()
	defer m.catalogueMu.RUnlock()
	return append([]recipe.Recipe(nil), m.effective...)
}

func (m *Manager) digest() string {
	m.catalogueMu.RLock()
	defer m.catalogueMu.RUnlock()
	return m.catalogueDigest
}

func (m *Manager) selfNode(fleetID string) store.FleetNode {
	return store.FleetNode{
		FleetID: fleetID, NodeID: m.identity.NodeID, DisplayName: m.displayName,
		ConsoleURL: m.consoleURL, NodeURL: m.nodeURL, Certificate: append([]byte(nil), m.identity.CertificatePEM...),
		ManagerVersion: m.version, ManagerBuildIdentity: m.buildIdentity, CatalogueDigest: m.digest(),
	}
}

type JoinCode struct {
	Code      string `json:"code"`
	ExpiresAt string `json:"expires_at"`
}

func (m *Manager) CreateJoinCode(ctx context.Context) (JoinCode, error) {
	if err := m.requireFleetMutationAllowed(ctx); err != nil {
		return JoinCode{}, err
	}
	secret, err := randomSecret(24)
	if err != nil {
		return JoinCode{}, err
	}
	expires := m.now().Add(joinCodeLifetime)
	if err := m.database.CreateFleetJoinCode(ctx, hashSecret(secret), expires); err != nil {
		return JoinCode{}, err
	}
	return JoinCode{Code: "v1." + m.identity.CertificateFingerprint + "." + secret, ExpiresAt: expires.UTC().Format(time.RFC3339Nano)}, nil
}

type AdoptRequest struct {
	DisplayName string `json:"display_name"`
	ConsoleURL  string `json:"console_url"`
	NodeURL     string `json:"node_url"`
	JoinCode    string `json:"join_code"`
}

type AdoptResult struct {
	Node store.FleetNode `json:"node"`
}

func (m *Manager) Adopt(ctx context.Context, request AdoptRequest) (AdoptResult, error) {
	if err := m.requireFleetMutationAllowed(ctx); err != nil {
		return AdoptResult{}, err
	}
	displayName := strings.TrimSpace(request.DisplayName)
	if displayName == "" || len(displayName) > 64 {
		return AdoptResult{}, errors.New("a node name between 1 and 64 characters is required")
	}
	consoleURL, err := normalizeOrigin(request.ConsoleURL, false)
	if err != nil {
		return AdoptResult{}, fmt.Errorf("console URL: %w", err)
	}
	nodeURL, err := normalizeOrigin(request.NodeURL, true)
	if err != nil {
		return AdoptResult{}, fmt.Errorf("node URL: %w", err)
	}
	expectedFingerprint, secret, err := parseJoinCode(request.JoinCode)
	if err != nil {
		return AdoptResult{}, err
	}
	config, err := m.database.EnsureFleetController(ctx, m.selfNode(""))
	if err != nil {
		return AdoptResult{}, err
	}
	prepareRequest := joinPrepareRequest{
		Version: ProtocolVersion, FleetID: config.FleetID, MembershipEpoch: config.MembershipEpoch,
		ControllerNodeID: m.identity.NodeID, ControllerConsoleURL: m.consoleURL, ControllerNodeURL: m.nodeURL,
		ControllerCertificate: m.identity.CertificatePEM, JoinSecret: secret,
	}
	var prepared joinPrepareResponse
	client := m.newClient(expectedFingerprint)
	if err := callFleetJSON(ctx, client, http.MethodPost, nodeURL+"/internal/fleet/v1/join/prepare", prepareRequest, &prepared); err != nil {
		return AdoptResult{}, fmt.Errorf("prepare fleet membership: %w", err)
	}
	_, details, err := ParseCertificatePEM(prepared.Certificate)
	if err != nil {
		return AdoptResult{}, fmt.Errorf("member certificate: %w", err)
	}
	if details.Fingerprint != expectedFingerprint || details.NodeID != prepared.NodeID {
		return AdoptResult{}, errors.New("the adopted node identity does not match its join code")
	}
	if prepared.PrepareToken == "" {
		return AdoptResult{}, errors.New("the adopted node did not return a membership preparation token")
	}
	node := store.FleetNode{
		NodeID: prepared.NodeID, DisplayName: displayName, ConsoleURL: consoleURL, NodeURL: nodeURL,
		Certificate: prepared.Certificate, ManagerVersion: prepared.ManagerVersion,
		ManagerBuildIdentity: prepared.ManagerBuildIdentity, CatalogueDigest: prepared.CatalogueDigest,
	}
	fleetID, idempotent, err := m.database.PrepareFleetNode(ctx, m.selfNode(config.FleetID), node)
	if err != nil {
		m.abortRemoteJoin(ctx, client, nodeURL, prepared.PrepareToken)
		return AdoptResult{}, err
	}
	if idempotent {
		m.abortRemoteJoin(ctx, client, nodeURL, prepared.PrepareToken)
		node.FleetID, node.MembershipState = fleetID, "active"
		return AdoptResult{Node: node}, nil
	}
	commitErr := callFleetJSON(ctx, client, http.MethodPost, nodeURL+"/internal/fleet/v1/join/commit", joinTokenRequest{PrepareToken: prepared.PrepareToken}, &struct{}{})
	if commitErr != nil {
		// A failed response after the request was sent cannot prove whether the
		// member committed. Keep the row visible as uncertain instead of
		// deleting the only record of an identity that may now trust us.
		_ = m.database.MarkFleetNodeAdoptionUncertain(ctx, fleetID, node.NodeID)
		return AdoptResult{}, fmt.Errorf("membership commit was not confirmed; the node remains visible for reconciliation: %w", commitErr)
	}
	if err := m.database.CommitFleetNode(ctx, fleetID, node.NodeID); err != nil {
		_ = m.database.MarkFleetNodeAdoptionUncertain(ctx, fleetID, node.NodeID)
		return AdoptResult{}, fmt.Errorf("record committed fleet member: %w", err)
	}
	node.FleetID, node.MembershipState = fleetID, "active"
	return AdoptResult{Node: node}, nil
}

func (m *Manager) abortRemoteJoin(ctx context.Context, client *http.Client, nodeURL, token string) {
	_ = callFleetJSON(ctx, client, http.MethodPost, nodeURL+"/internal/fleet/v1/join/abort", joinTokenRequest{PrepareToken: token}, &struct{}{})
}

func normalizeOrigin(raw string, tlsOnly bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("enter a bare manager URL")
	}
	if tlsOnly && parsed.Scheme != "https" {
		return "", errors.New("the fleet node URL must use https")
	}
	if !tlsOnly && parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("the console URL must use http or https")
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func parseJoinCode(code string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(code), ".")
	if len(parts) != 3 || parts[0] != "v1" || len(parts[1]) != 64 || parts[2] == "" {
		return "", "", errors.New("enter a valid fleet join code")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", "", errors.New("enter a valid fleet join code")
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[2]); err != nil {
		return "", "", errors.New("enter a valid fleet join code")
	}
	return parts[1], parts[2], nil
}

func randomSecret(size int) (string, error) {
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func hashSecret(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}

type FleetNodeSummary struct {
	NodeID               string            `json:"node_id"`
	DisplayName          string            `json:"display_name"`
	Role                 string            `json:"role"`
	Status               string            `json:"status"`
	ConsoleURL           string            `json:"console_url"`
	NodeURL              string            `json:"node_url"`
	ManagerVersion       string            `json:"manager_version"`
	ManagerBuildIdentity string            `json:"manager_build_identity"`
	CatalogueDigest      string            `json:"catalogue_digest"`
	LastHeartbeatAt      string            `json:"last_heartbeat_at"`
	ClockSkew            bool              `json:"clock_skew"`
	Inventory            *inventory.System `json:"inventory,omitempty"`
	InstalledModels      []ModelSnapshot   `json:"installed_models"`
	// The GPU power mode this node reported for itself. Both fields are always
	// present, and both are empty for a node whose heartbeat has not arrived
	// yet: an empty mode means "not reported", which is not the same as full
	// speed and must not be drawn as if it were.
	PowerMode        string `json:"power_mode"`
	PowerModeFailure string `json:"power_mode_failure"`
}

type Summary struct {
	FleetID              string             `json:"fleet_id"`
	Role                 string             `json:"role"`
	ControllerNodeID     string             `json:"controller_node_id"`
	ControllerConsoleURL string             `json:"controller_console_url"`
	MigrationState       string             `json:"migration_state"`
	Nodes                []FleetNodeSummary `json:"nodes"`
}

func (m *Manager) Summary(ctx context.Context) (Summary, error) {
	config, err := m.database.FleetConfig(ctx)
	if err != nil {
		return Summary{}, err
	}
	result := Summary{FleetID: config.FleetID, Role: config.Role, ControllerNodeID: config.ControllerNodeID,
		ControllerConsoleURL: config.ControllerConsoleURL, MigrationState: config.MigrationState}
	if config.Role == "standalone" {
		models, modelErr := m.database.Models(ctx)
		if modelErr != nil {
			return Summary{}, modelErr
		}
		snapshots := make([]ModelSnapshot, 0, len(models))
		for _, model := range models {
			snapshots = append(snapshots, ModelSnapshot{RecipeID: model.RecipeID, RecipeVersion: model.RecipeVersion, Status: model.Status, Active: model.Active})
		}
		system, inspectErr := m.inventory.Inspect(ctx)
		if inspectErr != nil {
			return Summary{}, inspectErr
		}
		// A Spark that belongs to no fleet sends itself no heartbeat, so its
		// own power mode is read straight from its own store.
		power, powerErr := m.database.PowerMode(ctx)
		if powerErr != nil {
			return Summary{}, powerErr
		}
		result.Nodes = []FleetNodeSummary{{NodeID: m.identity.NodeID, DisplayName: m.displayName, Role: "standalone", Status: "fresh", ConsoleURL: m.consoleURL, NodeURL: m.nodeURL, ManagerVersion: m.version, ManagerBuildIdentity: m.buildIdentity, CatalogueDigest: m.digest(), InstalledModels: snapshots, Inventory: &system, PowerMode: power.Mode, PowerModeFailure: power.Failure}}
		return result, nil
	}
	nodes, err := m.database.FleetNodes(ctx)
	if err != nil {
		return Summary{}, err
	}
	now := m.now()
	for _, node := range nodes {
		summary := FleetNodeSummary{NodeID: node.NodeID, DisplayName: node.DisplayName, ConsoleURL: node.ConsoleURL, NodeURL: node.NodeURL,
			ManagerVersion: node.ManagerVersion, ManagerBuildIdentity: node.ManagerBuildIdentity, CatalogueDigest: node.CatalogueDigest,
			LastHeartbeatAt: node.HeartbeatReceivedAt, Status: node.MembershipState, InstalledModels: []ModelSnapshot{}}
		if node.NodeID == config.ControllerNodeID {
			summary.Role = "controller"
		} else {
			summary.Role = "member"
		}
		if node.MembershipState == "active" {
			summary.Status = "unreachable"
			if received, err := time.Parse(time.RFC3339Nano, node.HeartbeatReceivedAt); err == nil {
				if now.Sub(received) <= HeartbeatFreshness {
					summary.Status = "fresh"
				} else {
					summary.Status = "stale"
				}
			}
			if node.ManagerVersion != "" && node.ManagerVersion != m.version {
				summary.Status = "version-mismatch"
			}
		}
		if len(node.HeartbeatPayload) > 0 {
			var payload HeartbeatPayload
			if err := strictJSON(node.HeartbeatPayload, &payload); err == nil {
				summary.Inventory = &payload.Inventory
				summary.InstalledModels = payload.InstalledModels
				summary.PowerMode = payload.PowerMode
				summary.PowerModeFailure = payload.PowerModeFailure
				if remoteTime, err := time.Parse(time.RFC3339Nano, payload.LocalTime); err == nil {
					received, _ := time.Parse(time.RFC3339Nano, node.HeartbeatReceivedAt)
					delta := received.Sub(remoteTime)
					summary.ClockSkew = delta > ClockSkewBound || delta < -ClockSkewBound
				}
			}
		}
		result.Nodes = append(result.Nodes, summary)
	}
	sort.Slice(result.Nodes, func(i, j int) bool {
		if result.Nodes[i].Role != result.Nodes[j].Role {
			return result.Nodes[i].Role == "controller"
		}
		return result.Nodes[i].DisplayName < result.Nodes[j].DisplayName
	})
	return result, nil
}

func strictJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON")
		}
		return err
	}
	return nil
}
