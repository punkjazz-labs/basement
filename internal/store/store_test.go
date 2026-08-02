package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
