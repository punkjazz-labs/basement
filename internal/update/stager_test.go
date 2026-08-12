package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
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

// An orphaned waiting_for_root is the state a quarantined-but-unreadable
// handoff leaves behind: the root updater removed the request, its receipt
// names no attempt, and nothing will ever settle the manager's status. Found
// on hardware 2026-08-12, where it read as an update in progress forever.
func TestReconcileStartupSettlesWaitingForRootNobodyOwns(t *testing.T) {
	stager := stagerFixture(t)
	writeAttempt(t, stager, AttemptStatus{
		SchemaVersion: 1, AttemptID: "update-orphaned", State: "waiting_for_root",
		RunningVersion: "v1.0.0", TargetVersion: "v1.1.0", UpdatedAt: "2026-08-12T00:00:00Z",
	})
	receipt := Receipt{SchemaVersion: 1, State: "failed_before_handoff", Failure: "copy update request: permission denied", UpdatedAt: "2026-08-12T00:01:00Z"}
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
	if status.State != "failed_before_handoff" || status.AttemptID != "update-orphaned" {
		t.Fatalf("status = %+v, want the orphaned attempt settled", status)
	}
}

func TestReconcileStartupLeavesWaitingForRootWithALiveHandoffAlone(t *testing.T) {
	stager := stagerFixture(t)
	writeAttempt(t, stager, AttemptStatus{
		SchemaVersion: 1, AttemptID: "update-handed-off", State: "waiting_for_root",
		RunningVersion: "v1.0.0", TargetVersion: "v1.1.0", UpdatedAt: "2026-08-12T00:00:00Z",
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
	if err != nil || status.State != "waiting_for_root" {
		t.Fatalf("state=%q err=%v, the root updater owns this attempt", status.State, err)
	}
}

func TestReconcileStartupLeavesSettledAndAbsentStatesAlone(t *testing.T) {
	for _, state := range []string{"staged", "succeeded", "rolled_back", "recovery_required", "failed_before_handoff"} {
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

// A schema-2 release whose helper differs from the installed one is the case
// generation 1 exists for: the helper rides the same signature as the
// manager, is verified against the signed size and digest, and reaches the
// root updater at the same mode as the manager payload.
func TestStagingASchemaTwoReleaseHandsOffTheSignedHelper(t *testing.T) {
	fixture := helperReleaseFixture(t, arm64ELF([]byte("new helper")))
	stager := fixture.stager(t, arm64ELF([]byte("installed helper")), 2)

	status := stageAndApply(t, stager, fixture.candidate)
	if status.State != "waiting_for_root" {
		t.Fatalf("status = %+v", status)
	}
	pendingDir := filepath.Join(stager.DataDir, "updates", "staging", "pending")
	info, err := os.Stat(filepath.Join(pendingDir, helperFileName))
	if err != nil {
		t.Fatalf("the signed helper was not handed off: %v", err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("helper mode = %o, want 0750 for the group-running updater", info.Mode().Perm())
	}
	staged, err := os.ReadFile(filepath.Join(pendingDir, helperFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(staged, fixture.helper) {
		t.Fatal("the handed-off helper is not the release's helper")
	}
	var request ApplyRequest
	if err := readJSONFile(filepath.Join(pendingDir, requestFileName), 64<<10, &request); err != nil {
		t.Fatal(err)
	}
	if request.SchemaVersion != RequestHelperSchemaVersion || request.HelperSHA256 != fixture.helperDigest {
		t.Fatalf("request = %+v, want schema 2 naming the signed helper digest", request)
	}
}

// When the installed helper already matches, nothing is downloaded and
// nothing is staged, but the request still says schema 2 so the root updater
// can report unchanged rather than silence.
func TestStagingSkipsTheHelperWhenTheInstalledOneMatches(t *testing.T) {
	current := arm64ELF([]byte("current helper"))
	fixture := helperReleaseFixture(t, current)
	stager := fixture.stager(t, current, 2)
	requested := 0
	stager.Client = fixture.transport(&requested)

	stageAndApply(t, stager, fixture.candidate)
	if requested != 1 {
		t.Fatalf("downloads = %d, want only the manager payload", requested)
	}
	pendingDir := filepath.Join(stager.DataDir, "updates", "staging", "pending")
	if _, err := os.Stat(filepath.Join(pendingDir, helperFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper bytes were staged for a helper that is already current: %v", err)
	}
	var request ApplyRequest
	if err := readJSONFile(filepath.Join(pendingDir, requestFileName), 64<<10, &request); err != nil {
		t.Fatal(err)
	}
	if request.SchemaVersion != RequestHelperSchemaVersion || request.HelperSHA256 != fixture.helperDigest {
		t.Fatalf("request = %+v, want schema 2 even with nothing to stage", request)
	}
}

// The hard rule: a helper that cannot be read is unknown, never stale, and
// must never cause a helper download. Treating unreadable as stale would
// download and hand off a helper on every single check.
func TestStagingNeverDownloadsAHelperItCannotCompare(t *testing.T) {
	fixture := helperReleaseFixture(t, arm64ELF([]byte("new helper")))
	stager := fixture.stager(t, arm64ELF([]byte("installed helper")), 2)
	stager.UpdaterBinaryPath = filepath.Join(t.TempDir(), "unreadable-basement-updater")
	requested := 0
	stager.Client = fixture.transport(&requested)

	stageAndApply(t, stager, fixture.candidate)
	if requested != 1 {
		t.Fatalf("downloads = %d, an unreadable helper triggered a helper download", requested)
	}
	pendingDir := filepath.Join(stager.DataDir, "updates", "staging", "pending")
	if _, err := os.Stat(filepath.Join(pendingDir, helperFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper bytes were staged from an unknown comparison: %v", err)
	}
}

// A helper that predates protocol 2 decodes its request strictly and would
// refuse a schema-2 one outright, failing a manager update over a helper that
// must never fail one. Such a machine takes the manager release now.
func TestStagingFallsBackToSchemaOneForAnOlderHelper(t *testing.T) {
	fixture := helperReleaseFixture(t, arm64ELF([]byte("new helper")))
	stager := fixture.stager(t, arm64ELF([]byte("installed helper")), 0)
	stager.HelperVersionRunner = func(context.Context, string) (string, error) {
		return "", errors.New("exit status 2")
	}
	requested := 0
	stager.Client = fixture.transport(&requested)

	stageAndApply(t, stager, fixture.candidate)
	if requested != 1 {
		t.Fatalf("downloads = %d, want only the manager payload", requested)
	}
	pendingDir := filepath.Join(stager.DataDir, "updates", "staging", "pending")
	var request ApplyRequest
	if err := readJSONFile(filepath.Join(pendingDir, requestFileName), 64<<10, &request); err != nil {
		t.Fatal(err)
	}
	if request.SchemaVersion != RequestSchemaVersion || request.HelperSHA256 != "" {
		t.Fatalf("request = %+v, want a schema 1 handoff an older helper can read", request)
	}
}

// A schema-1 release evaluates no helper at all, whatever is installed.
func TestStagingASchemaOneReleaseEvaluatesNoHelper(t *testing.T) {
	fixture := helperReleaseFixture(t, arm64ELF([]byte("new helper")))
	fixture.reSign(t, false)
	stager := fixture.stager(t, arm64ELF([]byte("installed helper")), 2)
	requested := 0
	stager.Client = fixture.transport(&requested)

	stageAndApply(t, stager, fixture.candidate)
	if requested != 1 {
		t.Fatalf("downloads = %d, a schema 1 release staged a helper", requested)
	}
	pendingDir := filepath.Join(stager.DataDir, "updates", "staging", "pending")
	var request ApplyRequest
	if err := readJSONFile(filepath.Join(pendingDir, requestFileName), 64<<10, &request); err != nil {
		t.Fatal(err)
	}
	if request.SchemaVersion != RequestSchemaVersion || request.HelperSHA256 != "" {
		t.Fatalf("request = %+v, want an unchanged schema 1 handoff", request)
	}
}

// Bytes that do not match the signed helper digest never reach the root
// updater, whatever the release host served.
func TestStagingRefusesAHelperThatDoesNotMatchItsSignedDigest(t *testing.T) {
	fixture := helperReleaseFixture(t, arm64ELF([]byte("new helper")))
	stager := fixture.stager(t, arm64ELF([]byte("installed helper")), 2)
	served := map[string][]byte{
		fixture.candidate.AssetURL:       fixture.manager,
		fixture.candidate.HelperAssetURL: arm64ELF([]byte("substituted helper")),
	}
	stager.Client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		payload, ok := served[request.URL.String()]
		if !ok {
			return nil, fmt.Errorf("unexpected download %s", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(payload)), Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
	})}
	status, err := stager.Prepare(fixture.candidate, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stager.StageOnly(context.Background(), fixture.candidate, status); err == nil {
		t.Fatal("a helper that does not match its signed digest was staged")
	}
}

type helperFixture struct {
	candidate    Candidate
	manager      []byte
	helper       []byte
	helperDigest string
	keys         KeyRing
	privateKey   ed25519.PrivateKey
}

func helperReleaseFixture(t *testing.T, helper []byte) *helperFixture {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	helperDigest := sha256.Sum256(helper)
	fixture := &helperFixture{
		manager: arm64ELF([]byte("target manager")), helper: helper,
		helperDigest: hex.EncodeToString(helperDigest[:]),
		keys:         KeyRing{"release-test": publicKey}, privateKey: privateKey,
	}
	fixture.reSign(t, true)
	return fixture
}

func (fixture *helperFixture) reSign(t *testing.T, withHelper bool) {
	t.Helper()
	digest := sha256.Sum256(fixture.manager)
	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion, KeyID: "release-test", ReleaseVersion: "v2.0.0",
		OS: "linux", Arch: "arm64", AssetName: LinuxARM64AssetName,
		AssetSize: int64(len(fixture.manager)), AssetSHA256: hex.EncodeToString(digest[:]),
		UpdaterProtocol: 1, RollbackFrom: []string{"v1.0.0"},
	}
	if withHelper {
		manifest.SchemaVersion = ManifestHelperSchemaVersion
		manifest.UpdaterProtocol = 2
		manifest.HelperAssetName = LinuxARM64HelperAssetName
		manifest.HelperSize = int64(len(fixture.helper))
		manifest.HelperSHA256 = fixture.helperDigest
	}
	manifestBytes, err := MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(fixture.privateKey, manifestBytes))), '\n')
	prefix := "https://github.com/example/releases/download/v2.0.0/"
	fixture.candidate = Candidate{
		Release: Release{TagName: "v2.0.0"}, ManifestBytes: manifestBytes, Signature: signature,
		AssetURL: prefix + LinuxARM64AssetName, HelperAssetURL: prefix + LinuxARM64HelperAssetName,
	}
}

func (fixture *helperFixture) stager(t *testing.T, installedHelper []byte, protocol int) *Stager {
	t.Helper()
	stager := NewStager(t.TempDir(), fixture.keys)
	stager.RootStatusPath = filepath.Join(t.TempDir(), "root-status.json")
	stager.BootstrapCheck = func() error { return nil }
	installHelper(t, stager, installedHelper)
	digest := sha256.Sum256(installedHelper)
	stager.HelperVersionRunner = fixedHelperVersion("v1.0.0", protocol, hex.EncodeToString(digest[:]))
	requested := 0
	stager.Client = fixture.transport(&requested)
	return stager
}

func (fixture *helperFixture) transport(requested *int) *http.Client {
	served := map[string][]byte{
		fixture.candidate.AssetURL:       fixture.manager,
		fixture.candidate.HelperAssetURL: fixture.helper,
	}
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		payload, ok := served[request.URL.String()]
		if !ok {
			return nil, fmt.Errorf("unexpected download %s", request.URL)
		}
		*requested++
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(payload)), Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
	})}
}

func stageAndApply(t *testing.T, stager *Stager, candidate Candidate) AttemptStatus {
	t.Helper()
	status, err := stager.Prepare(candidate, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	status, err = stager.StageOnly(context.Background(), candidate, status)
	if err != nil {
		t.Fatal(err)
	}
	status, err = stager.ApplyStaged(status)
	if err != nil {
		t.Fatal(err)
	}
	return status
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

	// The root updater has an empty capability set and reaches this handoff
	// only through its membership in the manager's group. Hardware proved
	// (2026-08-12) that a handoff staged without group access strands the
	// update in waiting_for_root with no receipt at all: the pending
	// directory needs group write for quarantine and cleanup, and every
	// handoff file needs its group-read bit.
	pendingDir := filepath.Dir(requestPath)
	info, err := os.Stat(pendingDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o770 {
		t.Fatalf("pending dir mode = %o, the group-running updater needs rwx", info.Mode().Perm())
	}
	for name, want := range map[string]os.FileMode{
		requestFileName: 0o640, ManifestAssetName: 0o640, SignatureAssetName: 0o640, managerFileName: 0o750,
	} {
		info, err := os.Stat(filepath.Join(pendingDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %o, want %o for the group-running updater", name, info.Mode().Perm(), want)
		}
	}
}

// An older manager created the pending directory without group access, and a
// directory that already exists is exactly the case MkdirAll silently leaves
// alone. The next staging must repair it, not inherit the strand.
func TestApplyStagedRepairsANarrowPendingDirectory(t *testing.T) {
	stager := stagerFixture(t)
	pendingDir := filepath.Join(stager.DataDir, "updates", "staging", "pending")
	if err := os.MkdirAll(pendingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pendingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manager := arm64ELF([]byte("repair target"))
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
	stager.Keys = KeyRing{"release-test": publicKey}
	stager.BootstrapCheck = func() error { return nil }
	stager.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(manager)), Body: io.NopCloser(bytes.NewReader(manager)), Header: make(http.Header)}, nil
	})}
	status, err := stager.Prepare(candidate, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if status, err = stager.StageOnly(context.Background(), candidate, status); err != nil {
		t.Fatal(err)
	}
	if status, err = stager.ApplyStaged(status); err != nil || status.State != "waiting_for_root" {
		t.Fatalf("handoff status=%+v err=%v", status, err)
	}
	info, err := os.Stat(pendingDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o770 {
		t.Fatalf("pending dir mode = %o after staging, want the repair to 770", info.Mode().Perm())
	}
}
