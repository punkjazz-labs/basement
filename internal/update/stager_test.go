package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
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

// A resolved fleet upgrade will never apply what this node staged, so resolve
// may settle one state startup reconciliation must not touch: 'staged'. Both
// paths leave anything the root updater owns strictly alone.
func TestSettleResolvedSettlesManagerOwnedStatesIncludingStaged(t *testing.T) {
	for _, state := range []string{"checking_signature", "downloading", "verifying", "staged"} {
		t.Run(state, func(t *testing.T) {
			stager := stagerFixture(t)
			writeAttempt(t, stager, AttemptStatus{
				SchemaVersion: 1, AttemptID: "update-resolved", State: state,
				RunningVersion: "v1.0.0", TargetVersion: "v1.1.0", UpdatedAt: "2026-08-11T00:00:00Z",
			})

			if err := stager.SettleResolved("the fleet upgrade was resolved"); err != nil {
				t.Fatal(err)
			}

			status, found, err := stager.Status()
			if err != nil || !found {
				t.Fatalf("status found=%v err=%v", found, err)
			}
			if status.State != "failed_before_handoff" || status.Failure != "the fleet upgrade was resolved" {
				t.Fatalf("settled status=%+v", status)
			}
		})
	}
}

func TestSettleResolvedLeavesRootOwnedAttemptsAlone(t *testing.T) {
	t.Run("pending root request", func(t *testing.T) {
		stager := stagerFixture(t)
		writeAttempt(t, stager, AttemptStatus{
			SchemaVersion: 1, AttemptID: "update-handed-off", State: "staged",
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

		if err := stager.SettleResolved("the fleet upgrade was resolved"); err != nil {
			t.Fatal(err)
		}
		status, _, err := stager.Status()
		if err != nil || status.State != "staged" {
			t.Fatalf("state=%q err=%v, the root updater owns this attempt", status.State, err)
		}
	})
	for _, state := range []string{"waiting_for_root", "succeeded", "rolled_back", "recovery_required", "failed_before_handoff"} {
		t.Run(state, func(t *testing.T) {
			stager := stagerFixture(t)
			writeAttempt(t, stager, AttemptStatus{
				SchemaVersion: 1, AttemptID: "update-settled", State: state,
				RunningVersion: "v1.0.0", TargetVersion: "v1.1.0", UpdatedAt: "2026-08-11T00:00:00Z",
			})

			if err := stager.SettleResolved("the fleet upgrade was resolved"); err != nil {
				t.Fatal(err)
			}
			status, _, err := stager.Status()
			if err != nil || status.State != state {
				t.Fatalf("state=%q err=%v, want untouched %q", status.State, err, state)
			}
		})
	}
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

func TestStagerSeparatesFleetVerificationFromUpdaterHandoff(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manager := arm64ELF([]byte("fleet target"))
	digest := sha256.Sum256(manager)
	manifestBytes, err := MarshalManifest(Manifest{SchemaVersion: ManifestSchemaVersion, KeyID: "release-test", ReleaseVersion: "v2.0.0",
		OS: "linux", Arch: "arm64", AssetName: LinuxARM64AssetName, AssetSize: int64(len(manager)), AssetSHA256: hex.EncodeToString(digest[:]),
		UpdaterProtocol: UpdaterProtocol, RollbackFrom: []string{"v1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifestBytes))), '\n')
	candidate := Candidate{Release: Release{TagName: "v2.0.0"}, ManifestBytes: manifestBytes, Signature: signature,
		AssetURL: "https://github.com/example/releases/download/v2.0.0/" + LinuxARM64AssetName}
	root := t.TempDir()
	stager := NewStager(root, KeyRing{"release-test": publicKey})
	stager.BootstrapCheck = func() error { return nil }
	stager.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(manager)), Body: io.NopCloser(bytes.NewReader(manager)), Header: make(http.Header)}, nil
	})}
	status, err := stager.Prepare(candidate, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	status, err = stager.StageOnly(context.Background(), candidate, status)
	if err != nil || status.State != "staged" {
		t.Fatalf("stage status=%+v err=%v", status, err)
	}
	requestPath := filepath.Join(root, "updates", "staging", "pending", requestFileName)
	if _, err := os.Stat(requestPath); !os.IsNotExist(err) {
		t.Fatalf("verification barrier created an updater request: %v", err)
	}
	status, err = stager.ApplyStaged(status)
	if err != nil || status.State != "waiting_for_root" {
		t.Fatalf("handoff status=%+v err=%v", status, err)
	}
	if _, err := os.Stat(requestPath); err != nil {
		t.Fatalf("handoff did not create updater request: %v", err)
	}
}
