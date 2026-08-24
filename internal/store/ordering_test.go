package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// now() writes RFC3339Nano, which removes trailing zeros. One stored moment
// can therefore be a text prefix of a later one. The 'Z' byte sorts above
// every digit, so a text comparison puts "...:00.5Z" after "...:00.51Z" even
// though .51 is the later moment. Each test below writes exactly that pair on
// rows that were created in order, then asks the query which row is newer.
// The queries must answer from the insertion order, which is the rowid.
const (
	momentBefore = "2026-08-01T10:00:00.5Z"
	momentAfter  = "2026-08-01T10:00:00.51Z"
	momentFirst  = "2026-08-01T09:00:00Z"
)

// forgeCreatedAt writes a created_at value straight into the table. The normal
// Create path can only write the current time, so this is the only way to make
// a real prefix pair on demand.
func forgeCreatedAt(t *testing.T, database *Store, table, keyColumn, key, value string) {
	t.Helper()
	result, err := database.db.Exec(`UPDATE `+table+` SET created_at=? WHERE `+keyColumn+`=?`, value, key)
	if err != nil {
		t.Fatal(err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		t.Fatalf("forging %s.created_at for %s changed %d rows, want 1", table, key, count)
	}
}

func TestJobsListNewestFirstWhenTheTimestampTextSortsWrong(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	first, _, err := database.CreateJob(ctx, "install", "recipe-one", "first-click", map[string]bool{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := database.CreateJob(ctx, "install", "recipe-two", "second-click", map[string]bool{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	forgeCreatedAt(t, database, "jobs", "id", first.ID, momentBefore)
	forgeCreatedAt(t, database, "jobs", "id", second.ID, momentAfter)

	listed, err := database.ListJobs(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d jobs, want 2", len(listed))
	}
	if listed[0].ID != second.ID || listed[1].ID != first.ID {
		t.Fatalf("order=%v, want the newest job first (%s then %s)", []string{listed[0].ID, listed[1].ID}, second.ID, first.ID)
	}
}

func TestLatestFleetUpgradeRunIsTheNewestWhenTheTimestampTextSortsWrong(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	self := testFleetNode("node_00000000000000000000000000000001", "https://192.168.99.10:7071")
	if err := database.EnsureNodeIdentity(ctx, testNodeIdentity(self.NodeID)); err != nil {
		t.Fatal(err)
	}
	config, err := database.EnsureFleetController(ctx, self)
	if err != nil {
		t.Fatal(err)
	}
	upgradeRun := func(runID, version string) (FleetUpgradeRun, []FleetUpgradeNode) {
		return FleetUpgradeRun{RunID: runID, FleetID: config.FleetID, ControllerNodeID: self.NodeID,
				ReleaseTag: version, TargetVersion: version, ManifestSHA256: strings.Repeat("c", 64),
				ManifestBytes: []byte("manifest"), SignatureBytes: []byte("signature"), AssetURL: "https://example.test/asset"},
			[]FleetUpgradeNode{{NodeID: self.NodeID, DisplayName: "spark-head", Sequence: 0,
				Role: "controller", RunningVersion: "test", TargetVersion: version}}
	}
	first, firstNodes := upgradeRun("run-first", "v2.0.0")
	if _, created, err := database.CreateFleetUpgradeRun(ctx, first, firstNodes); err != nil || !created {
		t.Fatalf("create the first run: created=%v err=%v", created, err)
	}
	// Only a settled run lets another one start, so close the first one before
	// the second run joins the same journal.
	if err := database.UpdateFleetUpgradeRunState(ctx, first.RunID, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	second, secondNodes := upgradeRun("run-second", "v3.0.0")
	if _, created, err := database.CreateFleetUpgradeRun(ctx, second, secondNodes); err != nil || !created {
		t.Fatalf("create the second run: created=%v err=%v", created, err)
	}
	forgeCreatedAt(t, database, "fleet_upgrade_runs", "run_id", first.RunID, momentBefore)
	forgeCreatedAt(t, database, "fleet_upgrade_runs", "run_id", second.RunID, momentAfter)

	latest, err := database.LatestFleetUpgradeRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.RunID != second.RunID {
		t.Fatalf("latest run=%s, want the newest run %s", latest.RunID, second.RunID)
	}
}

func TestFleetNodesListOldestFirstWhenTheTimestampTextSortsWrong(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	self := testFleetNode("node_00000000000000000000000000000001", "https://192.168.99.10:7071")
	if err := database.EnsureNodeIdentity(ctx, testNodeIdentity(self.NodeID)); err != nil {
		t.Fatal(err)
	}
	config, err := database.EnsureFleetController(ctx, self)
	if err != nil {
		t.Fatal(err)
	}
	joined := make([]string, 0, 2)
	for index := 2; index <= 3; index++ {
		member := testFleetNode(fmt.Sprintf("node_%032x", index), fmt.Sprintf("https://192.168.99.%d:7071", 9+index))
		if _, _, err := database.PrepareFleetNode(ctx, self, member); err != nil {
			t.Fatal(err)
		}
		if err := database.CommitFleetNode(ctx, config.FleetID, member.NodeID); err != nil {
			t.Fatal(err)
		}
		joined = append(joined, member.NodeID)
	}
	forgeCreatedAt(t, database, "fleet_nodes", "node_id", self.NodeID, momentFirst)
	forgeCreatedAt(t, database, "fleet_nodes", "node_id", joined[0], momentBefore)
	forgeCreatedAt(t, database, "fleet_nodes", "node_id", joined[1], momentAfter)

	nodes, err := database.FleetNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("listed %d nodes, want 3", len(nodes))
	}
	want := []string{self.NodeID, joined[0], joined[1]}
	for index, nodeID := range want {
		if nodes[index].NodeID != nodeID {
			t.Fatalf("node %d is %s, want the joining order %v", index, nodes[index].NodeID, want)
		}
	}
}

func TestFleetDeploymentsListNewestFirstWhenTheTimestampTextSortsWrong(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	owner := "node_00000000000000000000000000000001"
	place := func(deploymentID, recipeID string) {
		t.Helper()
		deployment := FleetDeployment{DeploymentID: deploymentID, RecipeID: recipeID, RecipeVersion: 1,
			RecipeFingerprint: "fingerprint-" + recipeID, TopologyCount: 1, OwnerNodeID: owner}
		if _, created, err := database.CreateFleetDeployment(ctx, deployment, FleetDeploymentNode{NodeID: owner}); err != nil || !created {
			t.Fatalf("place %s: created=%v err=%v", deploymentID, created, err)
		}
	}
	place("deployment-first", "recipe-one")
	place("deployment-second", "recipe-two")
	forgeCreatedAt(t, database, "fleet_deployments", "deployment_id", "deployment-first", momentBefore)
	forgeCreatedAt(t, database, "fleet_deployments", "deployment_id", "deployment-second", momentAfter)

	listed, err := database.FleetDeployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d deployments, want 2", len(listed))
	}
	if listed[0].DeploymentID != "deployment-second" || listed[1].DeploymentID != "deployment-first" {
		t.Fatalf("order=%v, want the newest deployment first", []string{listed[0].DeploymentID, listed[1].DeploymentID})
	}
}
