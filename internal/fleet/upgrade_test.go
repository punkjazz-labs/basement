package fleet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/punkjazz-labs/basement/internal/store"
	managerupdate "github.com/punkjazz-labs/basement/internal/update"
)

type fakeUpgradeNode struct {
	mu          sync.Mutex
	state       string
	running     string
	target      string
	busy        bool
	rollback    bool
	stageCalls  int
	applyCalls  int
	events      *[]string
	displayName string
}

func (node *fakeUpgradeNode) stage(runID string) LocalUpgradeStatus {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.stageCalls++
	*node.events = append(*node.events, "stage:"+node.displayName)
	if node.busy {
		node.state = "waiting_for_idle"
		return LocalUpgradeStatus{Version: UpgradeProtocolVersion, RunID: runID, State: node.state, RunningVersion: node.running, TargetVersion: node.target, Failure: "waiting for a running job to finish"}
	}
	node.state = "staged"
	return LocalUpgradeStatus{Version: UpgradeProtocolVersion, RunID: runID, State: node.state, RunningVersion: node.running, TargetVersion: node.target, AttemptID: "attempt-" + node.displayName}
}

func (node *fakeUpgradeNode) apply(runID string) LocalUpgradeStatus {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.applyCalls++
	*node.events = append(*node.events, "apply:"+node.displayName)
	if node.rollback {
		node.state = "rolled_back"
		return LocalUpgradeStatus{Version: UpgradeProtocolVersion, RunID: runID, State: node.state, RunningVersion: node.running, TargetVersion: node.target, AttemptID: "attempt-" + node.displayName, Failure: "target health check failed"}
	}
	node.state = "succeeded"
	node.running = node.target
	return LocalUpgradeStatus{Version: UpgradeProtocolVersion, RunID: runID, State: node.state, RunningVersion: node.running, TargetVersion: node.target, AttemptID: "attempt-" + node.displayName}
}

func (node *fakeUpgradeNode) status(runID string) LocalUpgradeStatus {
	node.mu.Lock()
	defer node.mu.Unlock()
	return LocalUpgradeStatus{Version: UpgradeProtocolVersion, RunID: runID, State: node.state, RunningVersion: node.running, TargetVersion: node.target, AttemptID: "attempt-" + node.displayName}
}

func TestFleetUpgradeBusyNodeDefersAndRolloutWaits(t *testing.T) {
	coordinator, database, run, memberID, nodes, _ := upgradeCoordinatorFixture(t)
	nodes[memberID].busy = true
	done, err := coordinator.AdvanceUpgrade(context.Background())
	if err != nil || done {
		t.Fatalf("advance done=%v err=%v", done, err)
	}
	stored, err := database.FleetUpgradeRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	member := upgradeNodeByID(t, stored, memberID)
	if stored.State != "waiting_for_idle" || member.State != "waiting_for_idle" || nodes[memberID].applyCalls != 0 {
		t.Fatalf("busy rollout=%+v member=%+v applies=%d", stored, member, nodes[memberID].applyCalls)
	}
	nodes[memberID].busy = false
	if done, err := coordinator.AdvanceUpgrade(context.Background()); err != nil || done {
		t.Fatalf("stage after idle done=%v err=%v", done, err)
	}
	stored, _ = database.FleetUpgradeRun(context.Background(), run.RunID)
	if upgradeNodeByID(t, stored, memberID).State != "staged" {
		t.Fatalf("idle node did not stage: %+v", stored.Nodes)
	}
}

func TestFleetUpgradeOrdersControllerLast(t *testing.T) {
	coordinator, database, _, _, _, _ := upgradeCoordinatorFixture(t)
	config, err := database.FleetConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := database.FleetNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := coordinator.upgradeOrder(context.Background(), nodes, config.ControllerNodeID, "v2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 2 || ordered[len(ordered)-1].NodeID != coordinator.identity.NodeID || ordered[len(ordered)-1].Role != "controller" {
		t.Fatalf("upgrade order=%+v", ordered)
	}
}

func TestFleetUpgradeFailedHealthRollsBackNodeAndStops(t *testing.T) {
	coordinator, database, run, memberID, nodes, events := upgradeCoordinatorFixture(t)
	nodes[memberID].rollback = true
	var terminalErr error
	for range 8 {
		done, err := coordinator.AdvanceUpgrade(context.Background())
		if done {
			terminalErr = err
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if terminalErr == nil || !strings.Contains(terminalErr.Error(), "health check") {
		t.Fatalf("rollback did not stop the rollout: %v", terminalErr)
	}
	stored, err := database.FleetUpgradeRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	member := upgradeNodeByID(t, stored, memberID)
	controller := upgradeNodeByID(t, stored, coordinator.identity.NodeID)
	if stored.State != "failed" || member.State != "rolled_back" || member.RunningVersion != "v1.0.0" || nodes[coordinator.identity.NodeID].applyCalls != 0 {
		t.Fatalf("failed rollout=%+v member=%+v controller=%+v", stored, member, controller)
	}
	wantPrefix := []string{"stage:spark-worker", "stage:spark-head", "apply:spark-worker"}
	if len(*events) < len(wantPrefix) {
		t.Fatalf("events=%v", *events)
	}
	for index, want := range wantPrefix {
		if (*events)[index] != want {
			t.Fatalf("events=%v want prefix=%v", *events, wantPrefix)
		}
	}
}

func TestFleetUpgradeStatusReportsMixedVersionsAndPerNodeStates(t *testing.T) {
	coordinator, _, _, memberID, _, _ := upgradeCoordinatorFixture(t)
	for range 4 {
		if done, err := coordinator.AdvanceUpgrade(context.Background()); err != nil || done {
			t.Fatalf("advance done=%v err=%v", done, err)
		}
	}
	status, err := coordinator.LatestUpgrade(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	member := upgradeNodeByID(t, status, memberID)
	controller := upgradeNodeByID(t, status, coordinator.identity.NodeID)
	if member.State != "succeeded" || member.RunningVersion != "v2.0.0" || controller.RunningVersion != "v1.0.0" || status.State != "applying" {
		t.Fatalf("mixed rollout status=%+v", status)
	}
	if _, err := coordinator.PlanIndependent(context.Background(), "any-recipe"); err == nil || !strings.Contains(err.Error(), "fleet maintenance") {
		t.Fatalf("placement was not blocked during mixed versions: %v", err)
	}
}

func TestFleetUpgradeControllerRestartResumesFromStore(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "manager.db")
	database, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	options := testManagerOptions(directory, database, "192.168.99.10")
	options.Version, options.BuildIdentity = "v1.0.0", "v1-build"
	first, err := NewManager(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	config, err := database.EnsureFleetController(ctx, first.selfNode(""))
	if err != nil {
		t.Fatal(err)
	}
	run := store.FleetUpgradeRun{RunID: "restart-run", FleetID: config.FleetID, ControllerNodeID: first.identity.NodeID,
		ReleaseTag: "v2.0.0", TargetVersion: "v2.0.0", ManifestSHA256: strings.Repeat("a", 64),
		ManifestBytes: []byte("manifest"), SignatureBytes: []byte("signature"), AssetURL: "https://github.com/example/asset"}
	if _, _, err := database.CreateFleetUpgradeRun(ctx, run, []store.FleetUpgradeNode{{NodeID: first.identity.NodeID, DisplayName: "spark-head", Sequence: 0, Role: "controller", RunningVersion: "v1.0.0", TargetVersion: "v2.0.0"}}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateFleetUpgradeNode(ctx, run.RunID, first.identity.NodeID, "checking_health", "v1.0.0", "attempt-head", ""); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateFleetUpgradeRunState(ctx, run.RunID, "applying", ""); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	options.Database, options.Version, options.BuildIdentity = database, "v2.0.0", "v2-build"
	restarted, err := NewManager(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	restarted.upgradeStatusCall = func(context.Context, store.FleetUpgradeNode) (LocalUpgradeStatus, error) {
		return LocalUpgradeStatus{Version: UpgradeProtocolVersion, RunID: run.RunID, State: "succeeded", RunningVersion: "v2.0.0", TargetVersion: "v2.0.0", AttemptID: "attempt-head"}, nil
	}
	if done, err := restarted.AdvanceUpgrade(ctx); err != nil || done {
		t.Fatalf("reconcile after restart done=%v err=%v", done, err)
	}
	if done, err := restarted.AdvanceUpgrade(ctx); err != nil || done {
		t.Fatalf("begin finalization after restart done=%v err=%v", done, err)
	}
	restarted.upgradeFinishCall = func(context.Context, store.FleetUpgradeRun, store.FleetUpgradeNode) error { return nil }
	if done, err := restarted.AdvanceUpgrade(ctx); err != nil || !done {
		t.Fatalf("finish after restart done=%v err=%v", done, err)
	}
	stored, err := database.FleetUpgradeRun(ctx, run.RunID)
	if err != nil || stored.State != "succeeded" || upgradeNodeByID(t, stored, restarted.identity.NodeID).RunningVersion != "v2.0.0" {
		t.Fatalf("resumed run=%+v err=%v", stored, err)
	}
}

func TestFleetUpgradeMemberVerifiesManifestWithOwnKey(t *testing.T) {
	headPublic, headPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	memberPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("manager"))
	manifest, err := managerupdate.MarshalManifest(managerupdate.Manifest{SchemaVersion: managerupdate.ManifestSchemaVersion, KeyID: "head-key", ReleaseVersion: "v2.0.0",
		OS: "linux", Arch: "arm64", AssetName: managerupdate.LinuxARM64AssetName, AssetSize: 7, AssetSHA256: hex.EncodeToString(digest[:]),
		UpdaterProtocol: managerupdate.UpdaterProtocol, RollbackFrom: []string{"v1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(headPrivate, manifest))), '\n')
	candidate := managerupdate.Candidate{Release: managerupdate.Release{TagName: "v2.0.0"}, ManifestBytes: manifest, Signature: signature, AssetURL: "https://github.com/example/manager"}
	head := managerupdate.NewStager(t.TempDir(), managerupdate.KeyRing{"head-key": headPublic})
	head.BootstrapCheck = func() error { return nil }
	if _, err := head.Prepare(candidate, "v1.0.0"); err != nil {
		t.Fatalf("controller key did not accept its release: %v", err)
	}
	member := managerupdate.NewStager(t.TempDir(), managerupdate.KeyRing{"member-key": memberPublic})
	member.BootstrapCheck = func() error { return nil }
	if _, err := member.Prepare(candidate, "v1.0.0"); err == nil || !strings.Contains(err.Error(), "unknown release key") {
		t.Fatalf("member accepted the controller's untrusted key: %v", err)
	}
}

func upgradeCoordinatorFixture(t *testing.T) (*Manager, *store.Store, store.FleetUpgradeRun, string, map[string]*fakeUpgradeNode, *[]string) {
	t.Helper()
	ctx := context.Background()
	coordinator, database := newTestManager(t, "spark-head", "192.168.99.10")
	coordinator.version, coordinator.buildIdentity = "v1.0.0", "v1-build"
	config, err := database.EnsureFleetController(ctx, coordinator.selfNode(""))
	if err != nil {
		t.Fatal(err)
	}
	memberID := "node_worker"
	member := store.FleetNode{NodeID: memberID, DisplayName: "spark-worker", ConsoleURL: "http://192.168.99.20:7070", NodeURL: "https://192.168.99.20:7071",
		Certificate: []byte("test-certificate"), ManagerVersion: "v1.0.0", ManagerBuildIdentity: "v1-build", CatalogueDigest: coordinator.digest()}
	if _, _, err := database.PrepareFleetNode(ctx, coordinator.selfNode(config.FleetID), member); err != nil {
		t.Fatal(err)
	}
	if err := database.CommitFleetNode(ctx, config.FleetID, memberID); err != nil {
		t.Fatal(err)
	}
	run := store.FleetUpgradeRun{RunID: "test-run", FleetID: config.FleetID, ControllerNodeID: coordinator.identity.NodeID,
		ReleaseTag: "v2.0.0", TargetVersion: "v2.0.0", ManifestSHA256: strings.Repeat("a", 64),
		ManifestBytes: []byte("manifest"), SignatureBytes: []byte("signature"), AssetURL: "https://github.com/example/asset"}
	ordered := []store.FleetUpgradeNode{
		{NodeID: memberID, DisplayName: "spark-worker", Sequence: 0, Role: "idle", RunningVersion: "v1.0.0", TargetVersion: "v2.0.0"},
		{NodeID: coordinator.identity.NodeID, DisplayName: "spark-head", Sequence: 1, Role: "controller", RunningVersion: "v1.0.0", TargetVersion: "v2.0.0"},
	}
	stored, _, err := database.CreateFleetUpgradeRun(ctx, run, ordered)
	if err != nil {
		t.Fatal(err)
	}
	events := &[]string{}
	nodes := map[string]*fakeUpgradeNode{
		memberID:                    {state: "pending", running: "v1.0.0", target: "v2.0.0", events: events, displayName: "spark-worker"},
		coordinator.identity.NodeID: {state: "pending", running: "v1.0.0", target: "v2.0.0", events: events, displayName: "spark-head"},
	}
	coordinator.upgradeStageCall = func(_ context.Context, run store.FleetUpgradeRun, node store.FleetUpgradeNode) (LocalUpgradeStatus, error) {
		return nodes[node.NodeID].stage(run.RunID), nil
	}
	coordinator.upgradeApplyCall = func(_ context.Context, run store.FleetUpgradeRun, node store.FleetUpgradeNode) (LocalUpgradeStatus, error) {
		return nodes[node.NodeID].apply(run.RunID), nil
	}
	coordinator.upgradeStatusCall = func(_ context.Context, node store.FleetUpgradeNode) (LocalUpgradeStatus, error) {
		return nodes[node.NodeID].status(node.RunID), nil
	}
	coordinator.upgradeFinishCall = func(context.Context, store.FleetUpgradeRun, store.FleetUpgradeNode) error { return nil }
	return coordinator, database, stored, memberID, nodes, events
}

func upgradeNodeByID(t *testing.T, run store.FleetUpgradeRun, nodeID string) store.FleetUpgradeNode {
	t.Helper()
	for _, node := range run.Nodes {
		if node.NodeID == nodeID {
			return node
		}
	}
	t.Fatalf("run has no node %s: %+v", nodeID, run.Nodes)
	return store.FleetUpgradeNode{}
}
