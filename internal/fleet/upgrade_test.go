package fleet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/punkjazz-labs/basement/internal/store"
	managerupdate "github.com/punkjazz-labs/basement/internal/update"
)

type fakeUpgradeNode struct {
	mu           sync.Mutex
	state        string
	running      string
	target       string
	busy         bool
	rollback     bool
	stageCalls   int
	applyCalls   int
	finishCalls  int
	resolveCalls int
	resolveErr   error
	// locked models the node's local maintenance hold: staging takes it, and
	// only a finish or resolve instruction from the controller releases it.
	locked      bool
	events      *[]string
	displayName string
}

func (node *fakeUpgradeNode) stage(runID string) LocalUpgradeStatus {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.stageCalls++
	node.locked = true
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

func (node *fakeUpgradeNode) finish() error {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.finishCalls++
	node.locked = false
	*node.events = append(*node.events, "finish:"+node.displayName)
	return nil
}

func (node *fakeUpgradeNode) resolve() error {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.resolveCalls++
	*node.events = append(*node.events, "resolve:"+node.displayName)
	if node.resolveErr != nil {
		return node.resolveErr
	}
	node.locked = false
	return nil
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

// upgradeRecoveryFixture is a three node fleet: two members and the
// controller last. It exists to prove the recovery path: a middle node that
// fails must not leave the node before it locked, and the owner's resolve
// must settle everything the failure left behind.
func upgradeRecoveryFixture(t *testing.T) (*Manager, *store.Store, store.FleetUpgradeRun, map[string]*fakeUpgradeNode, *[]string) {
	t.Helper()
	ctx := context.Background()
	coordinator, database := newTestManager(t, "spark-head", "192.168.99.10")
	coordinator.version, coordinator.buildIdentity = "v1.0.0", "v1-build"
	config, err := database.EnsureFleetController(ctx, coordinator.selfNode(""))
	if err != nil {
		t.Fatal(err)
	}
	members := []struct{ id, name, host string }{
		{"node_w1", "spark-w1", "192.168.99.20"},
		{"node_w2", "spark-w2", "192.168.99.21"},
	}
	for _, member := range members {
		node := store.FleetNode{NodeID: member.id, DisplayName: member.name,
			ConsoleURL: "http://" + member.host + ":7070", NodeURL: "https://" + member.host + ":7071",
			Certificate: []byte("test-certificate"), ManagerVersion: "v1.0.0", ManagerBuildIdentity: "v1-build", CatalogueDigest: coordinator.digest()}
		if _, _, err := database.PrepareFleetNode(ctx, coordinator.selfNode(config.FleetID), node); err != nil {
			t.Fatal(err)
		}
		if err := database.CommitFleetNode(ctx, config.FleetID, member.id); err != nil {
			t.Fatal(err)
		}
	}
	run := store.FleetUpgradeRun{RunID: "recovery-run", FleetID: config.FleetID, ControllerNodeID: coordinator.identity.NodeID,
		ReleaseTag: "v2.0.0", TargetVersion: "v2.0.0", ManifestSHA256: strings.Repeat("a", 64),
		ManifestBytes: []byte("manifest"), SignatureBytes: []byte("signature"), AssetURL: "https://github.com/example/asset"}
	ordered := []store.FleetUpgradeNode{
		{NodeID: "node_w1", DisplayName: "spark-w1", Sequence: 0, Role: "idle", RunningVersion: "v1.0.0", TargetVersion: "v2.0.0"},
		{NodeID: "node_w2", DisplayName: "spark-w2", Sequence: 1, Role: "idle", RunningVersion: "v1.0.0", TargetVersion: "v2.0.0"},
		{NodeID: coordinator.identity.NodeID, DisplayName: "spark-head", Sequence: 2, Role: "controller", RunningVersion: "v1.0.0", TargetVersion: "v2.0.0"},
	}
	stored, _, err := database.CreateFleetUpgradeRun(ctx, run, ordered)
	if err != nil {
		t.Fatal(err)
	}
	events := &[]string{}
	nodes := map[string]*fakeUpgradeNode{
		"node_w1":                   {state: "pending", running: "v1.0.0", target: "v2.0.0", events: events, displayName: "spark-w1"},
		"node_w2":                   {state: "pending", running: "v1.0.0", target: "v2.0.0", events: events, displayName: "spark-w2"},
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
	coordinator.upgradeFinishCall = func(_ context.Context, _ store.FleetUpgradeRun, node store.FleetUpgradeNode) error {
		return nodes[node.NodeID].finish()
	}
	coordinator.upgradeResolveCall = func(_ context.Context, _ store.FleetUpgradeRun, node store.FleetUpgradeNode) error {
		return nodes[node.NodeID].resolve()
	}
	return coordinator, database, stored, nodes, events
}

func driveUpgradeToFailure(t *testing.T, coordinator *Manager) error {
	t.Helper()
	for range 16 {
		done, err := coordinator.AdvanceUpgrade(context.Background())
		if done {
			return err
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("the rollout never reached a terminal outcome")
	return nil
}

func TestFleetUpgradeReleasesSucceededNodeBeforeRunEnds(t *testing.T) {
	coordinator, database, run, nodes, events := upgradeRecoveryFixture(t)
	nodes["node_w2"].rollback = true
	if err := driveUpgradeToFailure(t, coordinator); err == nil {
		t.Fatal("a rolled back node did not stop the rollout")
	}
	stored, err := database.FleetUpgradeRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != "failed" || upgradeNodeByID(t, stored, "node_w1").State != "succeeded" || upgradeNodeByID(t, stored, "node_w2").State != "rolled_back" {
		t.Fatalf("failed rollout=%+v", stored)
	}
	// (a) The node that already upgraded got its finish instruction the moment
	// it proved healthy, before the later node failed, so its local
	// maintenance hold is gone and it can serve local work again.
	if nodes["node_w1"].finishCalls == 0 || nodes["node_w1"].locked {
		t.Fatalf("succeeded node kept its local lock: finish=%d locked=%v events=%v", nodes["node_w1"].finishCalls, nodes["node_w1"].locked, *events)
	}
	finishIndex, applyIndex := -1, -1
	for index, event := range *events {
		switch event {
		case "finish:spark-w1":
			if finishIndex < 0 {
				finishIndex = index
			}
		case "apply:spark-w2":
			applyIndex = index
		}
	}
	if finishIndex < 0 || applyIndex < 0 || finishIndex > applyIndex {
		t.Fatalf("finish for the succeeded node did not precede the next apply: %v", *events)
	}
	if nodes[coordinator.identity.NodeID].finishCalls != 0 {
		t.Fatalf("the controller was finished before its own upgrade: %v", *events)
	}
}

func TestFleetUpgradeResolveSettlesRunAndEveryNode(t *testing.T) {
	ctx := context.Background()
	coordinator, database, run, nodes, _ := upgradeRecoveryFixture(t)
	nodes["node_w2"].rollback = true
	if err := driveUpgradeToFailure(t, coordinator); err == nil {
		t.Fatal("a rolled back node did not stop the rollout")
	}
	if _, err := coordinator.PlanIndependent(ctx, "any-recipe"); err == nil || !strings.Contains(err.Error(), "resolve it from the update screen") {
		t.Fatalf("the refusal for a failed run does not name the fix: %v", err)
	}
	resolved, err := coordinator.ResolveUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// (b) Resolve settles the run and every reachable node, succeeded ones
	// included, and each node released its local hold.
	if resolved.State != "resolved" {
		t.Fatalf("resolved run=%+v", resolved)
	}
	for _, node := range resolved.Nodes {
		if node.ResolveState != "resolved" {
			t.Fatalf("node %s was not resolved: %+v", node.DisplayName, node)
		}
	}
	for id, node := range nodes {
		if node.resolveCalls != 1 || node.locked {
			t.Fatalf("node %s resolve=%d locked=%v", id, node.resolveCalls, node.locked)
		}
	}
	if active, err := database.FleetUpgradeMaintenanceActive(ctx); err != nil || active {
		t.Fatalf("maintenance still active after resolve: active=%v err=%v", active, err)
	}
	if _, err := coordinator.PlanIndependent(ctx, "any-recipe"); err != nil && strings.Contains(err.Error(), "fleet") {
		t.Fatalf("placement is still blocked after resolve: %v", err)
	}
	// (c) The controller accepts a fresh run after resolve.
	fresh := store.FleetUpgradeRun{RunID: "recovery-run-2", FleetID: run.FleetID, ControllerNodeID: coordinator.identity.NodeID,
		ReleaseTag: "v2.0.1", TargetVersion: "v2.0.1", ManifestSHA256: strings.Repeat("b", 64),
		ManifestBytes: []byte("manifest"), SignatureBytes: []byte("signature"), AssetURL: "https://github.com/example/asset"}
	_, created, err := database.CreateFleetUpgradeRun(ctx, fresh, []store.FleetUpgradeNode{
		{NodeID: "node_w1", DisplayName: "spark-w1", Sequence: 0, Role: "idle", RunningVersion: "v2.0.0", TargetVersion: "v2.0.1"},
	})
	if err != nil || !created {
		t.Fatalf("a fresh run was refused after resolve: created=%v err=%v", created, err)
	}
}

func TestFleetUpgradeResolveToleratesUnreachableNodeAndRetries(t *testing.T) {
	ctx := context.Background()
	coordinator, database, run, nodes, _ := upgradeRecoveryFixture(t)
	nodes["node_w2"].rollback = true
	if err := driveUpgradeToFailure(t, coordinator); err == nil {
		t.Fatal("a rolled back node did not stop the rollout")
	}
	nodes["node_w2"].resolveErr = errors.New("the node did not answer")
	resolved, err := coordinator.ResolveUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// (d) The unreachable node does not block the others; its outcome is
	// recorded so the console can point at the machine that needs attention.
	if resolved.State != "resolved" {
		t.Fatalf("resolved run=%+v", resolved)
	}
	holdout := upgradeNodeByID(t, resolved, "node_w2")
	if holdout.ResolveState != "unreachable" || !strings.Contains(holdout.ResolveFailure, "did not answer") {
		t.Fatalf("holdout outcome=%+v", holdout)
	}
	if upgradeNodeByID(t, resolved, "node_w1").ResolveState != "resolved" || upgradeNodeByID(t, resolved, coordinator.identity.NodeID).ResolveState != "resolved" {
		t.Fatalf("reachable nodes were not resolved: %+v", resolved.Nodes)
	}
	if active, err := database.FleetUpgradeMaintenanceActive(ctx); err != nil || active {
		t.Fatalf("one unreachable node kept the fleet locked: active=%v err=%v", active, err)
	}
	// (e) Resolve is idempotent and retryable: a second attempt with the node
	// still dark changes nothing for settled nodes, and once the node answers
	// a retry settles only the holdout.
	if _, err := coordinator.ResolveUpgrade(ctx); err != nil {
		t.Fatal(err)
	}
	if nodes["node_w1"].resolveCalls != 1 || nodes[coordinator.identity.NodeID].resolveCalls != 1 {
		t.Fatalf("settled nodes were resolved again: w1=%d head=%d", nodes["node_w1"].resolveCalls, nodes[coordinator.identity.NodeID].resolveCalls)
	}
	nodes["node_w2"].resolveErr = nil
	retried, err := coordinator.ResolveUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if upgradeNodeByID(t, retried, "node_w2").ResolveState != "resolved" || nodes["node_w2"].locked {
		t.Fatalf("retry did not settle the holdout: %+v", retried.Nodes)
	}
	if _, err := database.FleetUpgradeRun(ctx, run.RunID); err != nil {
		t.Fatal(err)
	}
}

func TestFleetUpgradeResolveRefusesRunsThatNeedNoResolving(t *testing.T) {
	ctx := context.Background()
	coordinator, _, _, nodes, _ := upgradeRecoveryFixture(t)
	if done, err := coordinator.AdvanceUpgrade(ctx); err != nil || done {
		t.Fatalf("advance done=%v err=%v", done, err)
	}
	if _, err := coordinator.ResolveUpgrade(ctx); err == nil || !strings.Contains(err.Error(), "still in progress") {
		t.Fatalf("a live run was resolved: %v", err)
	}
	for range 16 {
		done, err := coordinator.AdvanceUpgrade(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	for id, node := range nodes {
		if node.state != "succeeded" {
			t.Fatalf("node %s did not finish the clean rollout: %+v", id, node.state)
		}
	}
	if _, err := coordinator.ResolveUpgrade(ctx); err == nil || !strings.Contains(err.Error(), "nothing to resolve") {
		t.Fatalf("a succeeded run was resolved: %v", err)
	}
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
