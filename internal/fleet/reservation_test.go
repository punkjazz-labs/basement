package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// Every deterministic identity — a recovered model, a delegated worker rank —
// can be prepared again after the work it stood for ended. A settled row holds
// no claim, so it must go; anything still live must stay, because only Prepare
// and Activate may judge a live claim.
func TestClearSettledFreesDeadIdentitiesAndKeepsLiveOnes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		state     string
		wantGone  bool
		wantState string
	}{
		{name: "released", state: "released", wantGone: true},
		{name: "expired", state: "expired", wantGone: true},
		{name: "prepared", state: "prepared", wantState: "prepared"},
		{name: "committed", state: "committed", wantState: "committed"},
		{name: "active", state: "active", wantState: "active"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			allocator := NewAllocator(database, "node-local")
			reservationID := ReservationID(ClaimKindLegacyRank, "settled-"+test.state)
			request := ReservationRequest{
				ReservationID: reservationID, DeploymentID: "legacy-rank:" + test.state,
				RecipeID: "rank-recipe", RecipeVersion: 1,
				Claims:       Claims{Version: ClaimsVersion, Kind: ClaimKindLegacyRank, Runtime: true},
				PrepareToken: LocalPrepareToken(reservationID), ExpiresAt: time.Now().Add(time.Hour),
			}
			if _, _, err := allocator.Prepare(ctx, request); err != nil {
				t.Fatal(err)
			}
			if test.state != "prepared" {
				if _, err := allocator.Commit(ctx, reservationID, LocalPrepareToken(reservationID), []byte(`{"grant":true}`)); err != nil {
					t.Fatal(err)
				}
			}
			switch test.state {
			case "active", "expired":
				if err := allocator.Activate(ctx, reservationID, ""); err != nil {
					t.Fatal(err)
				}
			case "released":
				if err := allocator.Release(ctx, reservationID); err != nil {
					t.Fatal(err)
				}
			}
			if test.state == "expired" {
				// The worker sweep's own path to expired: an unrenewed driver
				// lease is reclaimed, its rank stopped, and the row settles.
				if err := allocator.Renew(ctx, reservationID, time.Now().Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
				if _, err := allocator.BeginReclaim(ctx, reservationID, time.Now()); err != nil {
					t.Fatal(err)
				}
				if err := allocator.FinishReclaim(ctx, reservationID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := allocator.Reservation(ctx, reservationID)
			if err != nil {
				t.Fatal(err)
			}
			if before.State != test.state {
				t.Fatalf("fixture state = %q, want %q", before.State, test.state)
			}

			if err := allocator.ClearSettled(ctx, reservationID); err != nil {
				t.Fatal(err)
			}
			after, err := allocator.Reservation(ctx, reservationID)
			if test.wantGone {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("a %s row survived clearing: %+v err=%v", test.state, after, err)
				}
				// The freed identity is usable again, which is the point.
				if _, _, err := allocator.Prepare(ctx, request); err != nil {
					t.Fatalf("the cleared identity could not be prepared again: %v", err)
				}
				return
			}
			if err != nil || after.State != test.wantState {
				t.Fatalf("a live %s row was disturbed: %+v err=%v", test.state, after, err)
			}
		})
	}

	// A reservation that was never made is already clear.
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := NewAllocator(database, "node-local").ClearSettled(ctx, "reservation-that-never-existed"); err != nil {
		t.Fatalf("clearing an identity nothing ever used: %v", err)
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
