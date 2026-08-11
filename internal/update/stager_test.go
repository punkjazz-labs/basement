package update

import (
	"os"
	"path/filepath"
	"testing"
)

// A staging attempt only moves while the process that wrote it is alive, so
// whatever ReconcileStartup finds in a manager-owned state at startup was
// abandoned by a crash or restart. Left alone it reads as an update in
// progress forever, and that refusal covers installs and generations too.
func TestReconcileStartupSettlesAbandonedStagingAttempt(t *testing.T) {
	for _, state := range []string{"checking_signature", "downloading", "verifying"} {
		t.Run(state, func(t *testing.T) {
			stager := stagerFixture(t)
			writeAttempt(t, stager, AttemptStatus{
				SchemaVersion: 1, AttemptID: "update-abandoned", State: state,
				RunningVersion: "v1.0.0", TargetVersion: "v1.1.0", UpdatedAt: "2026-08-11T00:00:00Z",
			})

			if err := stager.ReconcileStartup(); err != nil {
				t.Fatal(err)
			}

			status, found, err := stager.Status()
			if err != nil || !found {
				t.Fatalf("status found=%v err=%v", found, err)
			}
			if status.State != "failed_before_handoff" {
				t.Fatalf("state = %q, want failed_before_handoff", status.State)
			}
			if status.Failure == "" || status.AttemptID != "update-abandoned" {
				t.Fatalf("settled status lost its identity or its explanation: %+v", status)
			}
		})
	}
}

func TestReconcileStartupLeavesRootOwnedAttemptAlone(t *testing.T) {
	stager := stagerFixture(t)
	writeAttempt(t, stager, AttemptStatus{
		SchemaVersion: 1, AttemptID: "update-handed-off", State: "verifying",
		RunningVersion: "v1.0.0", TargetVersion: "v1.1.0", UpdatedAt: "2026-08-11T00:00:00Z",
	})
	pending := filepath.Join(stager.DataDir, "updates", "staging", "pending")
	if err := os.MkdirAll(pending, 0o750); err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{SchemaVersion: 1, AttemptID: "update-handed-off", RunningVersion: "v1.0.0", TargetVersion: "v1.1.0"}
	if err := writeJSONFile(filepath.Join(pending, requestFileName), request, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := stager.ReconcileStartup(); err != nil {
		t.Fatal(err)
	}

	status, _, err := stager.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "verifying" {
		t.Fatalf("state = %q, the root updater owns this attempt and settles it itself", status.State)
	}
}

func TestReconcileStartupPrefersAnExistingRootReceipt(t *testing.T) {
	stager := stagerFixture(t)
	writeAttempt(t, stager, AttemptStatus{
		SchemaVersion: 1, AttemptID: "update-received", State: "downloading",
		RunningVersion: "v1.0.0", TargetVersion: "v1.1.0", UpdatedAt: "2026-08-11T00:00:00Z",
	})
	receipt := Receipt{SchemaVersion: 1, AttemptID: "update-received", State: "succeeded", RunningVersion: "v1.1.0", UpdatedAt: "2026-08-11T00:01:00Z"}
	if err := writeJSONFile(stager.RootStatusPath, receipt, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := stager.ReconcileStartup(); err != nil {
		t.Fatal(err)
	}

	status, _, err := stager.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "succeeded" {
		t.Fatalf("state = %q, the root receipt already settled this attempt", status.State)
	}
}

func TestReconcileStartupLeavesSettledAndAbsentStatesAlone(t *testing.T) {
	for _, state := range []string{"waiting_for_root", "succeeded", "rolled_back", "recovery_required", "failed_before_handoff"} {
		t.Run(state, func(t *testing.T) {
			stager := stagerFixture(t)
			writeAttempt(t, stager, AttemptStatus{
				SchemaVersion: 1, AttemptID: "update-settled", State: state,
				RunningVersion: "v1.0.0", TargetVersion: "v1.1.0", UpdatedAt: "2026-08-11T00:00:00Z",
			})

			if err := stager.ReconcileStartup(); err != nil {
				t.Fatal(err)
			}

			status, _, err := stager.Status()
			if err != nil {
				t.Fatal(err)
			}
			if status.State != state {
				t.Fatalf("state = %q, want untouched %q", status.State, state)
			}
		})
	}
	t.Run("no attempt at all", func(t *testing.T) {
		stager := stagerFixture(t)
		if err := stager.ReconcileStartup(); err != nil {
			t.Fatal(err)
		}
		if _, found, err := stager.Status(); err != nil || found {
			t.Fatalf("found=%v err=%v, want no attempt", found, err)
		}
	})
}

func stagerFixture(t *testing.T) *Stager {
	t.Helper()
	root := t.TempDir()
	stager := NewStager(root, nil)
	stager.RootStatusPath = filepath.Join(root, "root-status.json")
	return stager
}

func writeAttempt(t *testing.T, stager *Stager, status AttemptStatus) {
	t.Helper()
	if err := writeJSONFile(stager.statusPath(), status, 0o600); err != nil {
		t.Fatal(err)
	}
}
