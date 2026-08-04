package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func mediaGeneration() Generation {
	return Generation{
		RecipeID: "media-test-1s", Mode: "text_to_video", Prompt: "a quiet room",
		Blocks: 1, ShortEdge: 768, Width: 768, Height: 768, Frames: 22, Seed: 4242,
	}
}

func TestGenerationLifecycle(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	record, err := database.CreateGeneration(ctx, mediaGeneration())
	if err != nil {
		t.Fatal(err)
	}
	if record.ID == "" || record.Status != "queued" || record.CreatedAt == "" {
		t.Fatalf("new generation=%#v", record)
	}
	// Every field the gallery reproduces a clip from survives the round trip.
	stored, err := database.Generation(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Prompt != "a quiet room" || stored.Seed != 4242 || stored.Frames != 22 || stored.Blocks != 1 || stored.ShortEdge != 768 {
		t.Fatalf("stored=%#v", stored)
	}

	if err := database.StartGeneration(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	// A generation only starts once: a second worker holding the same id must
	// not be able to restart a run that is already going.
	if err := database.StartGeneration(ctx, record.ID); err == nil {
		t.Fatal("starting a running generation must be refused")
	}
	running, _ := database.Generation(ctx, record.ID)
	if running.Status != "running" || running.StartedAt == "" {
		t.Fatalf("running=%#v", running)
	}

	if err := database.CompleteGeneration(ctx, record.ID, "/data/generations/media-test-1s/"+record.ID+"/clip.mp4", 1234); err != nil {
		t.Fatal(err)
	}
	done, _ := database.Generation(ctx, record.ID)
	if done.Status != "completed" || done.Bytes != 1234 || done.FinishedAt == "" || done.OutputPath == "" {
		t.Fatalf("completed=%#v", done)
	}
	// A completed generation is not reopened by a late failure report.
	if err := database.FailGeneration(ctx, record.ID, "failed", "too late"); err == nil {
		t.Fatal("failing a completed generation must be refused")
	}

	if err := database.DeleteGeneration(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Generation(ctx, record.ID); err == nil {
		t.Fatal("a deleted generation must be gone")
	}
	if err := database.DeleteGeneration(ctx, record.ID); err == nil {
		t.Fatal("deleting a generation twice must be refused")
	}
}

func TestGenerationsAreListedNewestFirst(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var ids []string
	for i := 0; i < 3; i++ {
		record, err := database.CreateGeneration(ctx, mediaGeneration())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, record.ID)
	}
	listed, err := database.Generations(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed %d generations", len(listed))
	}
	if listed[0].ID != ids[2] || listed[2].ID != ids[0] {
		t.Fatalf("order=%v, want the newest first", []string{listed[0].ID, listed[1].ID, listed[2].ID})
	}
}

// TestGenerationsInProgressAreRecoveredOnOpen proves a restart does not leave
// a spinner that never resolves. The queue is in memory, so a generation that
// was queued or running when the process stopped is not coming back.
func TestGenerationsInProgressAreRecoveredOnOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := database.CreateGeneration(ctx, mediaGeneration())
	if err != nil {
		t.Fatal(err)
	}
	running, err := database.CreateGeneration(ctx, mediaGeneration())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartGeneration(ctx, running.ID); err != nil {
		t.Fatal(err)
	}
	finished, err := database.CreateGeneration(ctx, mediaGeneration())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartGeneration(ctx, finished.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteGeneration(ctx, finished.ID, "/data/clip.mp4", 10); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, id := range []string{queued.ID, running.ID} {
		record, err := reopened.Generation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if record.Status != "interrupted" || record.Error == "" || record.FinishedAt == "" {
			t.Fatalf("generation %s after restart=%#v", id, record)
		}
	}
	survivor, err := reopened.Generation(ctx, finished.ID)
	if err != nil {
		t.Fatal(err)
	}
	if survivor.Status != "completed" {
		t.Fatalf("a finished generation was disturbed by a restart: %#v", survivor)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
