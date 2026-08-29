package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNodeReservationStateTransitionsAndIdempotentRetries(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	request := NodeReservation{ReservationID: "reservation-one", DeploymentID: "deployment-one", RecipeID: "recipe-one", RecipeVersion: 1,
		RecipeFingerprint: "fingerprint-one", ClaimsJSON: []byte(`{"version":1,"kind":"local-job","disk_bytes":100,"runtime":true,"ports":[8000]}`),
		PrepareTokenHash: "token-hash", ExpiresAt: expires}
	prepared, created, err := database.PrepareNodeReservation(ctx, request)
	if err != nil || !created || prepared.State != "prepared" {
		t.Fatalf("prepare=%+v created=%v err=%v", prepared, created, err)
	}
	retried, created, err := database.PrepareNodeReservation(ctx, request)
	if err != nil || created || retried.ReservationID != prepared.ReservationID {
		t.Fatalf("idempotent prepare=%+v created=%v err=%v", retried, created, err)
	}
	changed := request
	changed.ClaimsJSON = []byte(`{"version":1,"kind":"local-job","disk_bytes":101,"runtime":true,"ports":[8000]}`)
	if _, _, err := database.PrepareNodeReservation(ctx, changed); !errors.Is(err, ErrReservationRetryConflict) {
		t.Fatalf("conflicting prepare error=%v", err)
	}
	grant := []byte(`{"grant":"one"}`)
	committed, err := database.CommitNodeReservation(ctx, request.ReservationID, request.PrepareTokenHash, grant)
	if err != nil || committed.State != "committed" {
		t.Fatalf("commit=%+v err=%v", committed, err)
	}
	if _, err := database.CommitNodeReservation(ctx, request.ReservationID, request.PrepareTokenHash, grant); err != nil {
		t.Fatalf("idempotent commit: %v", err)
	}
	if _, err := database.CommitNodeReservation(ctx, request.ReservationID, request.PrepareTokenHash, []byte(`{"grant":"two"}`)); !errors.Is(err, ErrReservationRetryConflict) {
		t.Fatalf("conflicting commit error=%v", err)
	}
	job, _, err := database.CreateJob(ctx, "install", request.RecipeID, "reservation-owner", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AttachNodeReservationJob(ctx, request.ReservationID, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.AttachNodeReservationJob(ctx, request.ReservationID, "another-job"); !errors.Is(err, ErrReservationRetryConflict) {
		t.Fatalf("conflicting job attachment error=%v", err)
	}
	if _, err := database.ExpireNodeReservations(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	committed, err = database.NodeReservation(ctx, request.ReservationID)
	if err != nil || committed.State != "committed" || committed.JobID != job.ID {
		t.Fatalf("owned committed reservation expired: %+v err=%v", committed, err)
	}
	if err := database.ActivateNodeReservation(ctx, request.ReservationID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExpireNodeReservations(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	active, err := database.NodeReservation(ctx, request.ReservationID)
	if err != nil || active.State != "active" {
		t.Fatalf("active reservation expired: %+v err=%v", active, err)
	}
	if err := database.AbortNodeReservation(ctx, request.ReservationID); err == nil {
		t.Fatal("an active reservation was aborted")
	}
	if err := database.ReleaseNodeReservation(ctx, request.ReservationID); err != nil {
		t.Fatal(err)
	}
	released, _ := database.NodeReservation(ctx, request.ReservationID)
	if released.State != "released" {
		t.Fatalf("released state=%s", released.State)
	}
}

func TestPreparedAndUnstartedCommittedReservationsExpire(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	expires := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	for _, id := range []string{"prepared", "committed"} {
		request := NodeReservation{ReservationID: id, DeploymentID: "deployment-" + id, RecipeID: "recipe-" + id, RecipeVersion: 1,
			ClaimsJSON: []byte(`{"version":1,"kind":"independent","disk_bytes":10,"runtime":false,"ports":[]}`), PrepareTokenHash: "token-" + id, ExpiresAt: expires}
		if _, _, err := database.PrepareNodeReservation(ctx, request); err != nil {
			t.Fatal(err)
		}
		if id == "committed" {
			if _, err := database.CommitNodeReservation(ctx, id, request.PrepareTokenHash, []byte(`{"grant":true}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	count, err := database.ExpireNodeReservations(ctx, time.Now())
	if err != nil || count != 2 {
		t.Fatalf("expired=%d err=%v", count, err)
	}
	for _, id := range []string{"prepared", "committed"} {
		reservation, _ := database.NodeReservation(ctx, id)
		if reservation.State != "expired" {
			t.Fatalf("%s state=%s", id, reservation.State)
		}
	}
}

func TestReclaimingReservationBlocksRuntimeUntilContainerStopFinishes(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	prepare := func(id string) {
		t.Helper()
		request := NodeReservation{
			ReservationID: id, DeploymentID: "deployment-" + id, RecipeID: "recipe-" + id, RecipeVersion: 1,
			ClaimsJSON:       []byte(`{"version":1,"kind":"legacy-rank","disk_bytes":0,"memory_bytes":0,"runtime":true,"ports":[],"fabric_interfaces":[]}`),
			PrepareTokenHash: "token-" + id, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		}
		if _, _, err := database.PrepareNodeReservation(ctx, request); err != nil {
			t.Fatal(err)
		}
		if _, err := database.CommitNodeReservation(ctx, id, request.PrepareTokenHash, []byte(`{"grant":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	prepare("expired-driver")
	prepare("next-driver")
	if err := database.ActivateNodeReservation(ctx, "expired-driver", ""); err != nil {
		t.Fatal(err)
	}
	if err := database.RenewNodeReservation(ctx, "expired-driver", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	reclaiming, err := database.BeginNodeReservationReclaim(ctx, "expired-driver", time.Now())
	if err != nil || reclaiming.State != "reclaiming" {
		t.Fatalf("begin reclaim=%+v err=%v", reclaiming, err)
	}
	if err := database.ActivateNodeReservation(ctx, "next-driver", ""); !errors.Is(err, ErrReservationConflict) {
		t.Fatalf("runtime entered before container stop finished: %v", err)
	}
	if err := database.FinishNodeReservationReclaim(ctx, "expired-driver"); err != nil {
		t.Fatal(err)
	}
	if err := database.ActivateNodeReservation(ctx, "next-driver", ""); err != nil {
		t.Fatalf("runtime remained blocked after reclaim finished: %v", err)
	}
}

// The delete guard is what the doc comment promises: a live reservation is
// never removed, whoever calls this. The allocator has a guard of its own, so
// this one is the second half of a duplicated list, and only a test at this
// layer can say whether it still holds. A row that is settled holds no claim
// and its identity has to become free again; a row that is not settled is the
// only record that this node is spoken for, and deleting it would let a second
// deployment onto a machine that is already busy.
func TestSettledDeleteNeverRemovesALiveReservation(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		state   string
		wantOut bool
	}{
		{state: "released", wantOut: true},
		{state: "expired", wantOut: true},
		{state: "aborted", wantOut: true},
		{state: "prepared"},
		{state: "committed"},
		{state: "active"},
		{state: "reclaiming"},
		{state: "maintenance"},
	} {
		t.Run(test.state, func(t *testing.T) {
			database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			const id = "reservation-under-test"
			grant := []byte(`{"grant":"one"}`)
			if _, _, err := database.PrepareNodeReservation(ctx, NodeReservation{
				ReservationID: id, DeploymentID: "deployment-one", RecipeID: "recipe-one", RecipeVersion: 1,
				RecipeFingerprint: "fingerprint-one", ClaimsJSON: []byte(`{"version":1,"kind":"local-job","runtime":true}`),
				PrepareTokenHash: "token-hash", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
			}); err != nil {
				t.Fatal(err)
			}
			if test.state != "prepared" {
				if _, err := database.CommitNodeReservation(ctx, id, "token-hash", grant); err != nil {
					t.Fatal(err)
				}
			}
			switch test.state {
			case "aborted":
				if err := database.AbortNodeReservation(ctx, id); err != nil {
					t.Fatal(err)
				}
			case "maintenance":
				if err := database.ActivateNodeMaintenanceReservation(ctx, id); err != nil {
					t.Fatal(err)
				}
			case "active", "reclaiming", "expired", "released":
				if err := database.ActivateNodeReservation(ctx, id, ""); err != nil {
					t.Fatal(err)
				}
			}
			switch test.state {
			case "released":
				if err := database.ReleaseNodeReservation(ctx, id); err != nil {
					t.Fatal(err)
				}
			case "reclaiming", "expired":
				// The sweep's own road to these two: a lease nobody renewed.
				if err := database.RenewNodeReservation(ctx, id, time.Now().Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
				if _, err := database.BeginNodeReservationReclaim(ctx, id, time.Now()); err != nil {
					t.Fatal(err)
				}
				if test.state == "expired" {
					if err := database.FinishNodeReservationReclaim(ctx, id); err != nil {
						t.Fatal(err)
					}
				}
			}
			staged, err := database.NodeReservation(ctx, id)
			if err != nil || staged.State != test.state {
				t.Fatalf("the fixture is in state %q, want %q: err=%v", staged.State, test.state, err)
			}

			if err := database.DeleteSettledNodeReservation(ctx, id); err != nil {
				t.Fatal(err)
			}
			after, err := database.NodeReservation(ctx, id)
			if test.wantOut {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("a %s row survived, so its identity stays blocked: %+v err=%v", test.state, after, err)
				}
				return
			}
			if err != nil || after.State != test.state {
				t.Fatalf("a live %s row was deleted, so this node's only record of being busy is gone: %+v err=%v", test.state, after, err)
			}
		})
	}
}
