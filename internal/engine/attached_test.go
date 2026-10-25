package engine

import (
	"context"
	"strings"
	"testing"
)

// An attached service the head's declaration places on the worker Spark keeps
// two-Spark models from starting there. The worker's own allocator would only
// refuse if the declaration had been put on that machine as well.
func TestTwoSparkStartRefusesAWorkerAnAttachedServiceHolds(t *testing.T) {
	ctx := context.Background()
	fake := newFleetExecutor()
	runner, s, r := newTwoSparkEngine(t, fake)
	planned, err := fake.Plan(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	worker := planned.Peer
	if worker.ID == "" || worker.Name == "" {
		t.Fatalf("the two-Spark fixture names no worker: %+v", worker)
	}
	runner.SetRuntimeHolder(func(nodeID, displayName string) (string, bool) {
		return "Video pair", nodeID == worker.ID && displayName == worker.Name
	})
	want := "the attached service Video pair has " + worker.Name + " reserved outside Basement, so this two-Spark model cannot start"

	job, _, err := s.CreateJob(ctx, "install", r.ID, "held-worker-install", map[string]any{"confirmed": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.PrepareJob(ctx, job.ID); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("the console must be refused before a job runs: %v", err)
	}
	runner.Start(job.ID)
	failed := waitJob(t, s, job.ID, "failed")
	if !strings.Contains(failed.Error, want) {
		t.Fatalf("job error = %q", failed.Error)
	}
	if events := fake.recorded(); len(events) != 0 {
		t.Fatalf("a refused job touched a node: %v", events)
	}
	reservations, err := runner.Reservations().AllReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, reservation := range reservations {
		if reservation.State == "committed" || reservation.State == "active" || reservation.State == "prepared" {
			t.Fatalf("a refused job kept a claim: %+v", reservation)
		}
	}

	// Downloading both halves claims no memory on either Spark, so it passes.
	download, _, err := s.CreateJob(ctx, "install", r.ID, "held-worker-download", map[string]any{"confirmed": true, "activate": false})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.PrepareJob(ctx, download.ID); err != nil {
		t.Fatalf("a download-only install was refused: %v", err)
	}
	runner.Start(download.ID)
	waitJob(t, s, download.ID, "ready")

	// Releasing the worker lets the model start.
	runner.SetRuntimeHolder(nil)
	start, _, err := s.CreateJob(ctx, "start", r.ID, "after-release", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.PrepareJob(ctx, start.ID); err != nil {
		t.Fatal(err)
	}
	runner.Start(start.ID)
	waitJob(t, s, start.ID, "ready")

	// A stop is never refused, so a model that serves can always be taken down.
	runner.SetRuntimeHolder(func(string, string) (string, bool) { return "Video pair", true })
	stop, _, err := s.CreateJob(ctx, "stop", r.ID, "stop-while-held", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(stop.ID)
	waitJob(t, s, stop.ID, "stopped")
}
