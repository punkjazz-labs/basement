package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// A Spark that nobody has ever asked to run cool runs at full speed, and the
// choice the owner does make survives every read after it. The failure
// sentence lives beside the mode rather than instead of it: a cap the driver
// refused today is still the cap this machine tries for tomorrow.
func TestPowerModeRoundTripAndFailureSentence(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	initial, err := database.PowerMode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Mode != PowerModeFull || initial.Failure != "" {
		t.Fatalf("a fresh Spark reads %+v, want full speed and no failure", initial)
	}

	cool, err := database.SetPowerMode(ctx, PowerModeCool)
	if err != nil {
		t.Fatal(err)
	}
	if cool.Mode != PowerModeCool || cool.UpdatedAt == "" {
		t.Fatalf("setting cool returned %+v", cool)
	}
	stored, err := database.PowerMode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Mode != PowerModeCool {
		t.Fatalf("the stored mode reads %q, want %q", stored.Mode, PowerModeCool)
	}

	const sentence = "The nvidia-smi command failed, so the GPU clock did not change."
	recorded, err := database.RecordPowerModeFailure(ctx, sentence)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Failure != sentence || recorded.Mode != PowerModeCool {
		t.Fatalf("a recorded failure reads %+v, want the sentence beside the cool mode", recorded)
	}
	persisted, err := database.PowerMode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Failure != sentence || persisted.Mode != PowerModeCool {
		t.Fatalf("the failure did not persist: %+v", persisted)
	}

	// Success clears the sentence, because a mode that is now in force must
	// not keep telling the owner that it is not.
	cleared, err := database.RecordPowerModeFailure(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Failure != "" || cleared.Mode != PowerModeCool {
		t.Fatalf("a success left %+v behind", cleared)
	}

	// So does a new choice. The old sentence belonged to the old mode.
	if _, err := database.RecordPowerModeFailure(ctx, sentence); err != nil {
		t.Fatal(err)
	}
	back, err := database.SetPowerMode(ctx, "FULL")
	if err != nil {
		t.Fatal(err)
	}
	if back.Mode != PowerModeFull || back.Failure != "" {
		t.Fatalf("a new choice reads %+v, want full speed with no inherited failure", back)
	}

	if _, err := database.SetPowerMode(ctx, "quiet"); !errors.Is(err, ErrPowerMode) {
		t.Fatalf("an unknown power mode was accepted: %v", err)
	}
	unchanged, err := database.PowerMode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Mode != PowerModeFull {
		t.Fatalf("a refused write changed the setting to %q", unchanged.Mode)
	}
}
