package store

import (
	"context"
	"errors"
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
