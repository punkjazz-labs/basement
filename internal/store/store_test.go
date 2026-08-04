package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestIdempotentJobsAndRestartRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, created, err := s.CreateJob(ctx, "install", "recipe-one", "same-click", map[string]bool{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first job was not created")
	}
	second, created, err := s.CreateJob(ctx, "install", "recipe-one", "same-click", map[string]bool{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("duplicate produced another job: %#v %#v", first, second)
	}
	if err := s.UpdateJobState(ctx, first.ID, "downloading_models", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginStep(ctx, first.ID, 0, "download_artifact"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStepReceipt(ctx, first.ID, 0, map[string]any{"bytes_complete": 42}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recovered, err := s.GetJob(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != "interrupted" {
		t.Fatalf("state=%s, want interrupted", recovered.State)
	}
	if len(recovered.Steps) != 1 || string(recovered.Steps[0].Receipt) != "{\"bytes_complete\":42}" {
		t.Fatalf("progress receipt was not persisted: %#v", recovered.Steps)
	}
}

func TestOnlyOneModelIsActive(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"one", "two"} {
		if err := s.SetInstalled(ctx, InstalledModel{RecipeID: id, RecipeVersion: 1, Status: "ready", ArtifactPath: "/managed/" + id, Active: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetOnlyActive(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	models, err := s.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, model := range models {
		if model.Active {
			active++
			if model.RecipeID != "two" {
				t.Fatalf("wrong active model: %s", model.RecipeID)
			}
		}
	}
	if active != 1 {
		t.Fatalf("active count=%d", active)
	}
}

func TestActivateExclusivelyDemotesOthersAtomically(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetInstalled(ctx, InstalledModel{RecipeID: "one", RecipeVersion: 1, Status: "ready", ArtifactPath: "/managed/one", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateExclusively(ctx, InstalledModel{RecipeID: "two", RecipeVersion: 1, Status: "ready", ArtifactPath: "/managed/two", Active: true}); err != nil {
		t.Fatal(err)
	}
	models, err := s.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		switch model.RecipeID {
		case "two":
			if !model.Active || model.Status != "ready" {
				t.Fatalf("target model was not activated: %#v", model)
			}
		case "one":
			if model.Active || model.Status != "stopped" {
				t.Fatalf("previous model was not demoted: %#v", model)
			}
		}
	}
}

func TestMarkCancellingNeverOverwritesTerminalStates(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, _, err := s.CreateJob(ctx, "install", "recipe-one", "cancel-intent", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	marked, err := s.MarkCancelling(ctx, job.ID)
	if err != nil || !marked {
		t.Fatalf("MarkCancelling()=%v %v", marked, err)
	}
	current, err := s.GetJob(ctx, job.ID)
	if err != nil || current.State != "cancelling" {
		t.Fatalf("state=%s err=%v", current.State, err)
	}
	if err := s.UpdateJobState(ctx, job.ID, "cancelled", "done"); err != nil {
		t.Fatal(err)
	}
	marked, err = s.MarkCancelling(ctx, job.ID)
	if err != nil || marked {
		t.Fatalf("terminal state was overwritten: %v %v", marked, err)
	}
	final, _ := s.GetJob(ctx, job.ID)
	if final.State != "cancelled" {
		t.Fatalf("state=%s, want cancelled", final.State)
	}
}

func TestRestartDoesNotExposeStaleReadyModel(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetInstalled(ctx, InstalledModel{RecipeID: "one", RecipeVersion: 1, Status: "ready", ArtifactPath: "/managed/one", ContainerID: "container-one", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	model, err := s.Model(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if model.Status != "recovering" || !model.Active {
		t.Fatalf("stale state was exposed after restart: %#v", model)
	}
}

func TestPeerCRUDNeverExposesTheAPIKey(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	created, err := s.CreatePeer(ctx, "edgexpert-beta", "http://edgexpert-beta.local:7070", "rosk_secretvalue")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Name != "edgexpert-beta" || created.BaseURL != "http://edgexpert-beta.local:7070" {
		t.Fatalf("unexpected created peer: %#v", created)
	}

	list, err := s.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("unexpected peer list: %#v", list)
	}

	// Peers() and its Peer struct carry no api_key field at all, so this is
	// a compile-time guarantee, not just a runtime check; still, prove the
	// credential only comes back through the dedicated accessor.
	peer, apiKey, err := s.PeerCredentials(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "rosk_secretvalue" || peer.ID != created.ID {
		t.Fatalf("PeerCredentials returned unexpected data: peer=%#v key=%q", peer, apiKey)
	}

	if err := s.DeletePeer(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PeerCredentials(ctx, created.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist after delete, got %v", err)
	}
	if err := s.DeletePeer(ctx, created.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist deleting an already-removed peer, got %v", err)
	}
}

// Two doors lead to a peer row: a console adoption and the manual add form.
// If they can both be open at once, the fleet can end up with two peers, and
// two peers is not a fleet: cmd/basement/main.go refuses to pick a worker
// from it, so every two-Spark model stops being installable.
func TestCreatePeerAdmitsExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const racers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	won := make([]bool, racers)
	refused := make([]bool, racers)
	other := make([]error, racers)
	for index := 0; index < racers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, err := s.CreatePeer(ctx, fmt.Sprintf("spark-%d", index), fmt.Sprintf("http://192.168.99.%d:7070", 100+index), "rosk_key")
			switch {
			case err == nil:
				won[index] = true
			case errors.Is(err, ErrPeerExists):
				refused[index] = true
			default:
				other[index] = err
			}
		}(index)
	}
	close(start)
	wg.Wait()

	winners := 0
	for index := 0; index < racers; index++ {
		if other[index] != nil {
			t.Errorf("racer %d failed for an unrelated reason: %v", index, other[index])
		}
		if won[index] {
			winners++
		}
		if !won[index] && !refused[index] && other[index] == nil {
			t.Errorf("racer %d neither won nor was told why not", index)
		}
	}
	if winners != 1 {
		t.Errorf("%d concurrent CreatePeer calls succeeded, want exactly 1", winners)
	}
	peers, err := s.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 {
		t.Fatalf("the peers table holds %d rows: %+v", len(peers), peers)
	}
	// And the rule keeps holding once the race is over.
	if _, err := s.CreatePeer(ctx, "one-more", "http://192.168.99.200:7070", "rosk_key"); !errors.Is(err, ErrPeerExists) {
		t.Errorf("a later CreatePeer returned %v, want ErrPeerExists", err)
	}
	// Removing the one peer frees the slot again.
	if err := s.DeletePeer(ctx, peers[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePeer(ctx, "replacement", "http://192.168.99.201:7070", "rosk_key"); err != nil {
		t.Errorf("the slot was not free after removing the peer: %v", err)
	}
}

// A database written before the singleton column exists must open, keep its
// peer, and enforce the rule from then on.
func TestPeersSingletonMigratesAnOlderDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	older, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := older.Exec(`CREATE TABLE peers (
	  id TEXT PRIMARY KEY,
	  name TEXT NOT NULL,
	  base_url TEXT NOT NULL,
	  api_key TEXT NOT NULL,
	  created_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := older.Exec(`INSERT INTO peers(id,name,base_url,api_key,created_at) VALUES('peer_old','spark-worker','http://192.168.99.137:7070','rosk_old','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := older.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("an older database did not open: %v", err)
	}
	defer s.Close()
	peers, err := s.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].ID != "peer_old" {
		t.Fatalf("the existing peer did not survive the migration: %+v", peers)
	}
	if _, err := s.CreatePeer(ctx, "second", "http://192.168.99.200:7070", "rosk_key"); !errors.Is(err, ErrPeerExists) {
		t.Errorf("the migrated database accepted a second peer: %v", err)
	}
}

// A runtime's token counters restart with its container, and the manager's
// own process restarts independently of both. Neither may lose a stretch of
// usage or count one twice.
func TestTokenUsageSurvivesRuntimeAndManagerRestarts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTokenSample(ctx, "qwen", 100, 40); err != nil {
		t.Fatal(err)
	}
	// A rising counter contributes only what it rose by.
	if err := s.RecordTokenSample(ctx, "qwen", 250, 90); err != nil {
		t.Fatal(err)
	}
	// The serving container restarted: the counter is now below the last
	// reading, so the whole of it is new usage.
	if err := s.RecordTokenSample(ctx, "qwen", 30, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The manager restarted, the runtime did not: the counter carries on
	// from where the last reading left it.
	if err := s.RecordTokenSample(ctx, "qwen", 80, 25); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTokenSample(ctx, "nemotron", 7, 3); err != nil {
		t.Fatal(err)
	}
	usage, err := s.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 {
		t.Fatalf("usage rows=%+v", usage)
	}
	if usage[0].RecipeID != "qwen" || usage[0].PromptTokens != 330 || usage[0].GenerationTokens != 115 {
		t.Fatalf("qwen totals are wrong: %+v", usage[0])
	}
	if usage[1].RecipeID != "nemotron" || usage[1].PromptTokens != 7 || usage[1].GenerationTokens != 3 {
		t.Fatalf("nemotron totals are wrong: %+v", usage[1])
	}
	if usage[0].FirstCountedAt == "" || usage[0].UpdatedAt == "" || usage[0].FirstCountedAt == usage[0].UpdatedAt {
		t.Fatalf("first_counted_at was not held from the first reading: %+v", usage[0])
	}
}

// A model nobody has served here yet has nothing to report, and reporting
// zero would claim it served nothing, which is a different statement.
func TestTokenUsageIsEmptyBeforeAnythingIsCounted(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetInstalled(ctx, InstalledModel{RecipeID: "qwen", RecipeVersion: 1, Status: "ready", ArtifactPath: "/managed/qwen"}); err != nil {
		t.Fatal(err)
	}
	usage, err := s.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 0 {
		t.Fatalf("an installed model with no readings reported usage: %+v", usage)
	}
}

// basement resets a model's last-seen counters right after it takes the
// final reading from a container it is about to stop itself, because that
// container can never publish another value for tokenDelta to compare
// against. The reset must leave totals exactly as they were, and the next
// container's first reading (which, being a fresh container, can legitimately
// start above the old counters) must count in full rather than only the
// portion above them.
func TestResetTokenCountersLeavesTotalsAndCountsNextReadingInFull(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecordTokenSample(ctx, "qwen", 100, 40); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetTokenCounters(ctx, "qwen"); err != nil {
		t.Fatal(err)
	}
	usage, err := s.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].PromptTokens != 100 || usage[0].GenerationTokens != 40 {
		t.Fatalf("reset changed totals: %+v", usage)
	}
	// A new container starts publishing above zero immediately. Without the
	// reset, tokenDelta would compare this against the dead container's last
	// counter (100/40) and only count the rise past it; with the counters
	// zeroed, the whole of this first reading is new usage.
	if err := s.RecordTokenSample(ctx, "qwen", 90, 30); err != nil {
		t.Fatal(err)
	}
	usage, err = s.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].PromptTokens != 190 || usage[0].GenerationTokens != 70 {
		t.Fatalf("next reading after reset was not counted in full: %+v", usage)
	}
}

// A role is a stable name an app keeps pointing at while the owner changes
// the model behind it, so reassigning must move the model without the role
// itself looking new, and clearing must leave no row to route to.
func TestRolesAreReassignedAndCleared(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.AssignRole(ctx, "fast", "qwen")
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "fast" || created.RecipeID != "qwen" {
		t.Fatalf("unexpected role: %+v", created)
	}
	if _, err := s.AssignRole(ctx, "reasoning", "nemotron"); err != nil {
		t.Fatal(err)
	}
	moved, err := s.AssignRole(ctx, "fast", "nemotron")
	if err != nil {
		t.Fatal(err)
	}
	if moved.RecipeID != "nemotron" {
		t.Fatalf("reassignment did not move the model: %+v", moved)
	}
	if moved.CreatedAt != created.CreatedAt {
		t.Fatalf("reassignment restarted the role's life: %s then %s", created.CreatedAt, moved.CreatedAt)
	}
	if moved.UpdatedAt == created.UpdatedAt {
		t.Fatalf("reassignment did not record a change: %+v", moved)
	}

	// Assignments outlive the manager process: an app pointing at role/fast
	// must still reach the same model after a restart.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	roles, err := s.Roles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 2 || roles[0].Name != "fast" || roles[1].Name != "reasoning" {
		t.Fatalf("roles did not survive a restart in order: %+v", roles)
	}

	if err := s.ClearRole(ctx, "fast"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Role(ctx, "fast"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a cleared role still resolves: %v", err)
	}
	if err := s.ClearRole(ctx, "fast"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clearing an unassigned role returned %v, want os.ErrNotExist", err)
	}
}

// The name travels to clients as "role/<name>", so what the table accepts is
// exactly what an OpenAI model field can carry back unchanged.
func TestRoleNamesAreLowercaseSlugs(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"", "  ", "Fast Model", "-fast", "fast-", "fast/er", "fast_er", strings.Repeat("f", 33)} {
		if _, err := s.AssignRole(ctx, name, "qwen"); err == nil {
			t.Errorf("role name %q was accepted", name)
		}
	}
	// A name is stored in the one form clients will send, so the same role
	// cannot exist twice in different casings.
	if _, err := s.AssignRole(ctx, "  Code-Review  ", "qwen"); err != nil {
		t.Fatal(err)
	}
	role, err := s.Role(ctx, "code-review")
	if err != nil || role.RecipeID != "qwen" {
		t.Fatalf("role=%+v err=%v", role, err)
	}
	roles, err := s.Roles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles[0].Name != "code-review" {
		t.Fatalf("normalized name was not stored once: %+v", roles)
	}
}

// TestTerritoryEligibilityConfirmationRoundTrips mirrors accepted_licences:
// unconfirmed by default, recorded only on an explicit true, idempotent on a
// repeat confirmation, and keyed by recipe id and version so a later version
// of the same recipe starts unconfirmed again.
func TestTerritoryEligibilityConfirmationRoundTrips(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	confirmed, err := s.TerritoryEligibilityConfirmed(ctx, "minimax-h3-comfyui-1s", 1)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed {
		t.Fatal("a recipe version nobody confirmed must read back unconfirmed")
	}

	if err := s.ConfirmTerritoryEligibility(ctx, "minimax-h3-comfyui-1s", 1); err != nil {
		t.Fatal(err)
	}
	// A repeat confirmation (e.g. a retried install request) must not error
	// or duplicate the row.
	if err := s.ConfirmTerritoryEligibility(ctx, "minimax-h3-comfyui-1s", 1); err != nil {
		t.Fatal(err)
	}
	confirmed, err = s.TerritoryEligibilityConfirmed(ctx, "minimax-h3-comfyui-1s", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed {
		t.Fatal("confirmation did not persist")
	}

	stillUnconfirmed, err := s.TerritoryEligibilityConfirmed(ctx, "minimax-h3-comfyui-1s", 2)
	if err != nil {
		t.Fatal(err)
	}
	if stillUnconfirmed {
		t.Fatal("a different recipe version must not inherit another version's confirmation")
	}
}

// A model with no usage row yet has no counters to reset, and that must not
// be an error: basement's pre-stop path calls this even for a model that was
// never successfully sampled.
func TestResetTokenCountersIsNoOpWithoutARow(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ResetTokenCounters(ctx, "qwen"); err != nil {
		t.Fatal(err)
	}
	usage, err := s.TokenUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 0 {
		t.Fatalf("reset without a row created one: %+v", usage)
	}
}
