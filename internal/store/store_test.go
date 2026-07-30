package store

import (
	"context"
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
