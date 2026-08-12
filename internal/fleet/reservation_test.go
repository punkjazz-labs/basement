package fleet

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/punkjazz-labs/basement/internal/store"
)

func TestExactRecipeReservationIDUsesOnlyPinnedIdentity(t *testing.T) {
	base := ExactRecipeReservationID(ClaimKindLegacyRank, "node-one", "job-one", "recipe-one", 1)
	if retry := ExactRecipeReservationID(ClaimKindLegacyRank, "node-one", "job-one", "recipe-one", 1); retry != base {
		t.Fatalf("identical pinned identity produced %q after %q", retry, base)
	}
	variants := []string{
		ExactRecipeReservationID(ClaimKindLegacyRank, "node-two", "job-one", "recipe-one", 1),
		ExactRecipeReservationID(ClaimKindLegacyRank, "node-one", "job-two", "recipe-one", 1),
		ExactRecipeReservationID(ClaimKindLegacyRank, "node-one", "job-one", "recipe-two", 1),
		ExactRecipeReservationID(ClaimKindLegacyRank, "node-one", "job-one", "recipe-one", 2),
	}
	for _, variant := range variants {
		if variant == base {
			t.Fatalf("different pinned identity reused reservation id %q", base)
		}
	}
}

func TestLocalStartAndRemoteRankRaceAdmitsOneRuntimeOwner(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	allocator := NewAllocator(database, "node-local")
	type runtimeContender struct {
		id     string
		kind   string
		recipe string
	}
	contenders := []runtimeContender{
		{id: ReservationID(ClaimKindLocalJob, "local-start"), kind: ClaimKindLocalJob, recipe: "local-model"},
		{id: ReservationID(ClaimKindLegacyRank, "remote-rank"), kind: ClaimKindLegacyRank, recipe: "remote-model"},
	}
	for _, contender := range contenders {
		if _, _, err := allocator.Prepare(ctx, ReservationRequest{ReservationID: contender.id, DeploymentID: "deployment:" + contender.id,
			RecipeID: contender.recipe, RecipeVersion: 1, Claims: Claims{Version: ClaimsVersion, Kind: contender.kind, Runtime: true},
			PrepareToken: LocalPrepareToken(contender.id)}); err != nil {
			t.Fatal(err)
		}
		if _, err := allocator.Commit(ctx, contender.id, LocalPrepareToken(contender.id), []byte(`{"grant":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, len(contenders))
	var wait sync.WaitGroup
	for _, contender := range contenders {
		wait.Add(1)
		go func(contender runtimeContender) {
			defer wait.Done()
			<-start
			results <- allocator.Activate(ctx, contender.id, "")
		}(contender)
	}
	close(start)
	wait.Wait()
	close(results)
	winners, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, store.ErrReservationConflict):
			conflicts++
		default:
			t.Fatalf("unexpected runtime claim error: %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("runtime winners=%d conflicts=%d", winners, conflicts)
	}
}

// The state a real machine was found in on 2026-08-12: an installed model
// still marked active and recovering, while every reservation for it,
// including the deterministic recovered-model one, had been released. Startup
// recovery prepared that same identity, got the released row back unchanged,
// failed to activate it, and the manager exited: a crash loop with the
// console dead. Reconcile must clear the settled row and claim fresh.
func TestReconcileRecoversActiveModelWhoseRecoveryReservationWasReleased(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	allocator := NewAllocator(database, "node-local")

	reservationID := ReservationID(ClaimKindRecovered, "wedged-model")
	if _, _, err := allocator.Prepare(ctx, ReservationRequest{
		ReservationID: reservationID, DeploymentID: "recovered:wedged-model",
		RecipeID: "wedged-model", RecipeVersion: 1,
		Claims:       Claims{Version: ClaimsVersion, Kind: ClaimKindRecovered, Runtime: true},
		PrepareToken: LocalPrepareToken(reservationID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.Commit(ctx, reservationID, LocalPrepareToken(reservationID), []byte(`{"grant":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := allocator.Activate(ctx, reservationID, ""); err != nil {
		t.Fatal(err)
	}
	if err := allocator.Release(ctx, reservationID); err != nil {
		t.Fatal(err)
	}
	if err := database.SetInstalled(ctx, store.InstalledModel{RecipeID: "wedged-model", RecipeVersion: 1, Status: "recovering", Active: true}); err != nil {
		t.Fatal(err)
	}

	if err := allocator.Reconcile(ctx, nil); err != nil {
		t.Fatalf("startup reconciliation must recover, not refuse to start: %v", err)
	}

	recovered, err := allocator.Reservation(ctx, reservationID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != "active" {
		t.Fatalf("recovered reservation state = %q, want active", recovered.State)
	}

	// Running it again with the reservation now genuinely active must keep
	// it, not clear it: only settled rows may be deleted.
	if err := allocator.Reconcile(ctx, nil); err != nil {
		t.Fatal(err)
	}
	kept, err := allocator.Reservation(ctx, reservationID)
	if err != nil || kept.State != "active" {
		t.Fatalf("second reconcile state=%q err=%v, want the active claim kept", kept.State, err)
	}
}

func TestActiveLegacyRankSurvivesRestartAndRejectsSameRecipeFromAnotherDriver(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := NewAllocator(database, "node-local")
	firstID := ReservationID(ClaimKindLegacyRank, "driver-one")
	if _, _, err := first.Prepare(ctx, ReservationRequest{
		ReservationID: firstID, DeploymentID: "deployment-one", DriverNodeID: "driver-one",
		RecipeID: "shared-recipe", RecipeVersion: 1,
		Claims:       Claims{Version: ClaimsVersion, Kind: ClaimKindLegacyRank, Runtime: true},
		PrepareToken: LocalPrepareToken(firstID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Commit(ctx, firstID, LocalPrepareToken(firstID), []byte(`{"grant":"one"}`)); err != nil {
		t.Fatal(err)
	}
	if err := first.Activate(ctx, firstID, ""); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	restarted := NewAllocator(database, "node-local")
	secondID := ReservationID(ClaimKindLegacyRank, "driver-two")
	if _, _, err := restarted.Prepare(ctx, ReservationRequest{
		ReservationID: secondID, DeploymentID: "deployment-two", DriverNodeID: "driver-two",
		RecipeID: "shared-recipe", RecipeVersion: 1,
		Claims:       Claims{Version: ClaimsVersion, Kind: ClaimKindLegacyRank, Runtime: true},
		PrepareToken: LocalPrepareToken(secondID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Commit(ctx, secondID, LocalPrepareToken(secondID), []byte(`{"grant":"two"}`)); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Activate(ctx, secondID, ""); !errors.Is(err, store.ErrReservationConflict) {
		t.Fatalf("second driver activation error=%v", err)
	}
	persisted, err := restarted.Reservation(ctx, firstID)
	if err != nil || persisted.State != "active" {
		t.Fatalf("first driver reservation=%+v err=%v", persisted, err)
	}
}

func TestUpdateMaintenanceReservationBlocksNewRuntimeWithoutStoppingServingModel(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	allocator := NewAllocator(database, "node-test")
	prepareRuntime := func(id, kind string) {
		t.Helper()
		if _, _, err := allocator.Prepare(ctx, ReservationRequest{ReservationID: id, DeploymentID: "deployment-" + id,
			RecipeID: "recipe-" + id, RecipeVersion: 1, RecipeFingerprint: "fingerprint-" + id,
			Claims: Claims{Version: ClaimsVersion, Kind: kind, Runtime: true, Ports: []int{}, FabricInterfaces: []string{}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := allocator.Commit(ctx, id, LocalPrepareToken(id), []byte(`{"grant":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	prepareRuntime("serving", ClaimKindRecovered)
	if err := allocator.Activate(ctx, "serving", ""); err != nil {
		t.Fatal(err)
	}
	prepareRuntime("update", ClaimKindUpdate)
	if err := allocator.ActivateMaintenance(ctx, "update"); err != nil {
		t.Fatal(err)
	}
	serving, err := allocator.Reservation(ctx, "serving")
	if err != nil || serving.State != "active" {
		t.Fatalf("serving reservation=%+v err=%v", serving, err)
	}
	if _, _, err := allocator.Prepare(ctx, ReservationRequest{ReservationID: "new-work", DeploymentID: "deployment-new",
		RecipeID: "recipe-new", RecipeVersion: 1, Claims: Claims{Version: ClaimsVersion, Kind: ClaimKindLocalJob, Runtime: true, Ports: []int{}, FabricInterfaces: []string{}}}); !errors.Is(err, store.ErrReservationConflict) {
		t.Fatalf("new runtime work entered maintenance: %v", err)
	}
	if err := allocator.Release(ctx, "update"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := allocator.Prepare(ctx, ReservationRequest{ReservationID: "after-update", DeploymentID: "deployment-after",
		RecipeID: "recipe-after", RecipeVersion: 1, Claims: Claims{Version: ClaimsVersion, Kind: ClaimKindLocalJob, Runtime: true, Ports: []int{}, FabricInterfaces: []string{}}}); err != nil {
		t.Fatalf("runtime admission stayed closed after update: %v", err)
	}
}
