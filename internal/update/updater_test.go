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
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type fakeServiceController struct {
	starts int
	stops  int
}

func (service *fakeServiceController) Stop(context.Context) error {
	service.stops++
	return nil
}

func (service *fakeServiceController) Start(context.Context) error {
	service.starts++
	return nil
}

func (*fakeServiceController) MainPID(context.Context) (int, error) { return 1, nil }

type healthCheckFunc func(context.Context, string, string) error

func (check healthCheckFunc) Check(ctx context.Context, version, executable string) error {
	return check(ctx, version, executable)
}

func TestUpdaterSwitchesToHealthyTarget(t *testing.T) {
	updater, paths := updaterFixture(t, nil)
	updater.Health = healthCheckFunc(func(_ context.Context, version, executable string) error {
		if version != "v2.0.0" || executable != filepath.Join(paths.VersionsDir, "v2.0.0", managerFileName) {
			return errors.New("unexpected target health request")
		}
		return nil
	})
	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSlot(t, paths, "v2.0.0")
	var journal Journal
	if err := readJSONFile(paths.journalPath(), 64<<10, &journal); err != nil {
		t.Fatal(err)
	}
	if journal.State != JournalTargetHealthy {
		t.Fatalf("journal state = %q", journal.State)
	}
}

// rehomeToBootstrapSlot moves the fixture's previous slot to the
// content-addressed name the installer really creates. Every machine starts
// on one of these, because the installer is how the manager arrives.
func rehomeToBootstrapSlot(t *testing.T, paths Paths) string {
	t.Helper()
	slot := "bootstrap-" + strings.Repeat("ab12", 16)
	if err := os.Rename(filepath.Join(paths.VersionsDir, "v1.0.0"), filepath.Join(paths.VersionsDir, slot)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths.CurrentLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", slot), paths.CurrentLink); err != nil {
		t.Fatal(err)
	}
	return slot
}

func TestUpdaterUpdatesFromAnInstallerBootstrapSlot(t *testing.T) {
	// The exact hardware state of 2026-08-12: a machine installed by the
	// real installer, whose current slot is content-addressed. The updater
	// refused it as "not a stable release", which meant no freshly
	// installed machine could ever take its first console update.
	updater, paths := updaterFixture(t, nil)
	rehomeToBootstrapSlot(t, paths)
	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSlot(t, paths, "v2.0.0")
	var journal Journal
	if err := readJSONFile(paths.journalPath(), 64<<10, &journal); err != nil {
		t.Fatal(err)
	}
	if journal.State != JournalTargetHealthy {
		t.Fatalf("journal state = %q", journal.State)
	}
}

func TestUpdaterRollsBackToTheBootstrapSlotWhenTargetIsUnhealthy(t *testing.T) {
	updater, paths := updaterFixture(t, nil)
	slot := rehomeToBootstrapSlot(t, paths)
	updater.Health = healthCheckFunc(func(_ context.Context, version, _ string) error {
		if version == "v2.0.0" {
			return errors.New("target refuses to serve")
		}
		return nil
	})
	// A completed rollback reports through the receipt, not an error: the
	// machine is healthy again, and that is the outcome that counts.
	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSlot(t, paths, slot)
	var receipt Receipt
	if err := readJSONFile(paths.receiptPath(), 64<<10, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "rolled_back" {
		t.Fatalf("receipt state = %q, want rolled_back onto the bootstrap slot", receipt.State)
	}
}

// The updater unit runs under UMask=0077, and umask silently strips the
// requested slot modes to owner-only. The manager runs as its own user, so a
// root-owned slot it cannot execute crash loops the service into rollback
// (hardware, 2026-08-12). The slot must come out world-readable regardless
// of the process umask.
func TestPreparedSlotIsExecutableWhateverTheUmask(t *testing.T) {
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)
	updater, paths := updaterFixture(t, nil)
	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	slotDir := filepath.Join(paths.VersionsDir, "v2.0.0")
	info, err := os.Stat(slotDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("slot dir mode = %o, the service user cannot enter it", info.Mode().Perm())
	}
	binary, err := os.Stat(filepath.Join(slotDir, managerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if binary.Mode().Perm() != 0o755 {
		t.Fatalf("slot binary mode = %o, the service user cannot execute it", binary.Mode().Perm())
	}
}

func TestUpdaterRollsBackWhenTargetIsUnhealthy(t *testing.T) {
	updater, paths := updaterFixture(t, nil)
	updater.Health = healthCheckFunc(func(_ context.Context, version, _ string) error {
		if version == "v2.0.0" {
			return errors.New("target health failed")
		}
		return nil
	})
	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSlot(t, paths, "v1.0.0")
	var journal Journal
	if err := readJSONFile(paths.journalPath(), 64<<10, &journal); err != nil {
		t.Fatal(err)
	}
	if journal.State != JournalRolledBack {
		t.Fatalf("journal state = %q", journal.State)
	}
	var receipt Receipt
	if err := readJSONFile(paths.receiptPath(), 64<<10, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "rolled_back" || receipt.RunningVersion != "v1.0.0" {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestUpdaterRecordsRecoveryRequiredWhenRollbackIsUnhealthy(t *testing.T) {
	updater, paths := updaterFixture(t, healthCheckFunc(func(context.Context, string, string) error {
		return errors.New("health failed")
	}))
	if err := updater.Apply(context.Background()); err == nil {
		t.Fatal("unhealthy target and rollback did not require recovery")
	}
	assertSelectedSlot(t, paths, "v1.0.0")
	var journal Journal
	if err := readJSONFile(paths.journalPath(), 64<<10, &journal); err != nil {
		t.Fatal(err)
	}
	if journal.State != JournalRecoveryRequired {
		t.Fatalf("journal state = %q", journal.State)
	}
	if err := writeJSONFile(paths.requestPath(), ApplyRequest{
		SchemaVersion: 1, AttemptID: "attempt-retry", RunningVersion: "v1.0.0", TargetVersion: "v2.0.0",
		ManifestSHA256: journal.ManifestSHA256,
	}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.requestPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery request marker was not consumed: %v", err)
	}
	if err := readJSONFile(paths.journalPath(), 64<<10, &journal); err != nil {
		t.Fatal(err)
	}
	if journal.State != JournalRecoveryRequired {
		t.Fatalf("recovery retry changed journal state to %q", journal.State)
	}
}

func TestUpdaterRejectsSymlinkRequest(t *testing.T) {
	updater, paths := updaterFixture(t, nil)
	requestBytes, err := os.ReadFile(paths.requestPath())
	if err != nil {
		t.Fatal(err)
	}
	realRequest := filepath.Join(paths.StagingDir, "request-source.json")
	if err := os.WriteFile(realRequest, requestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths.requestPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRequest, paths.requestPath()); err != nil {
		t.Fatal(err)
	}
	if err := updater.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "too many levels of symbolic links") {
		t.Fatalf("Apply() error = %v", err)
	}
	assertSelectedSlot(t, paths, "v1.0.0")
}

// The swap is the tail of a healthy transaction and nothing else. Every mode
// below is set with an explicit Chmod, because the unit runs this process
// under UMask=0077: a helper left at 0700 still executes as root, but the
// manager can no longer hash it and staleness detection silently degrades to
// unknown.
func TestHelperSwapsItselfAfterTheTargetIsHealthy(t *testing.T) {
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)
	release := arm64ELF([]byte("released helper"))
	installed := arm64ELF([]byte("installed helper"))
	updater, paths := helperUpdaterFixture(t, installed, release, true)

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSlot(t, paths, "v2.0.0")
	live, err := os.ReadFile(paths.helperPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(live, release) {
		t.Fatal("the live helper is not the released helper")
	}
	backup, err := os.ReadFile(paths.helperBackup())
	if err != nil {
		t.Fatalf("no forensic copy of the replaced helper: %v", err)
	}
	if !bytes.Equal(backup, installed) {
		t.Fatal(".previous does not hold the helper that was replaced")
	}
	for _, path := range []string{paths.helperPath(), paths.helperBackup()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode = %o, the manager can no longer hash it", filepath.Base(path), info.Mode().Perm())
		}
	}
	if _, err := os.Stat(paths.helperNext()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".next survived the rename: %v", err)
	}
	receipt := readReceipt(t, paths)
	if receipt.State != "succeeded" || receipt.HelperState != HelperStateUpdated {
		t.Fatalf("receipt = %+v, want a succeeded update reporting the swap", receipt)
	}
	if receipt.SchemaVersion != RequestHelperSchemaVersion {
		t.Fatalf("receipt schema = %d, want the schema of the request it served", receipt.SchemaVersion)
	}
}

func TestHelperReportsUnchangedWhenItAlreadyMatches(t *testing.T) {
	current := arm64ELF([]byte("current helper"))
	updater, paths := helperUpdaterFixture(t, current, current, false)

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt := readReceipt(t, paths)
	if receipt.State != "succeeded" || receipt.HelperState != HelperStateUnchanged {
		t.Fatalf("receipt = %+v, want unchanged", receipt)
	}
	if _, err := os.Stat(paths.helperBackup()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a helper that did not change was still backed up: %v", err)
	}
}

// A rolled-back update leaves the helper exactly where it was. The swap is
// never on a rollback path, and the receipt says nothing about a swap that
// did not happen.
func TestNoHelperSwapWhenTheUpdateRollsBack(t *testing.T) {
	release := arm64ELF([]byte("released helper"))
	installed := arm64ELF([]byte("installed helper"))
	updater, paths := helperUpdaterFixture(t, installed, release, true)
	updater.Health = healthCheckFunc(func(_ context.Context, version, _ string) error {
		if version == "v2.0.0" {
			return errors.New("target refuses to serve")
		}
		return nil
	})

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSlot(t, paths, "v1.0.0")
	live, err := os.ReadFile(paths.helperPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(live, installed) {
		t.Fatal("a rolled-back update replaced the helper")
	}
	if _, err := os.Stat(paths.helperBackup()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rolled-back update wrote a helper backup: %v", err)
	}
	receipt := readReceipt(t, paths)
	if receipt.State != "rolled_back" || receipt.HelperState != "" {
		t.Fatalf("receipt = %+v, want a rollback carrying no helper state", receipt)
	}
}

// Corrupting one staged byte is the tamper case. The helper refuses the swap
// on the signed digest, and the manager update still reports success, because
// the manager did become healthy and that is the honest outcome.
func TestSwapFailureLeavesTheUpdateSucceeded(t *testing.T) {
	release := arm64ELF([]byte("released helper"))
	installed := arm64ELF([]byte("installed helper"))
	updater, paths := helperUpdaterFixture(t, installed, release, true)
	staged := filepath.Join(paths.pendingDir(), helperFileName)
	tampered := append([]byte(nil), release...)
	tampered[len(tampered)-1] ^= 1
	if err := os.WriteFile(staged, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSlot(t, paths, "v2.0.0")
	live, err := os.ReadFile(paths.helperPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(live, installed) {
		t.Fatal("tampered helper bytes were installed")
	}
	receipt := readReceipt(t, paths)
	if receipt.State != "succeeded" {
		t.Fatalf("receipt state = %q, a failed swap must not fail the update", receipt.State)
	}
	if !strings.HasPrefix(receipt.HelperState, "swap_failed:") {
		t.Fatalf("helper state = %q, want a named swap failure", receipt.HelperState)
	}
}

// A schema-2 request whose helper bytes never arrived is the same shape: the
// update succeeds and the receipt names what did not happen.
func TestMissingStagedHelperIsRecordedAsASwapFailure(t *testing.T) {
	updater, paths := helperUpdaterFixture(t, arm64ELF([]byte("installed helper")), arm64ELF([]byte("released helper")), false)

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt := readReceipt(t, paths)
	if receipt.State != "succeeded" || !strings.HasPrefix(receipt.HelperState, "swap_failed:") {
		t.Fatalf("receipt = %+v", receipt)
	}
}

// The mixed case decision 6 exists for: a schema-1 request from an older
// manager produces a schema-1 receipt with no helper_state, whatever this
// helper is capable of. That older manager decodes strictly and a field it
// does not know would be a refusal, on the rollback path where it is reading.
func TestSchemaOneRequestProducesASchemaOneReceipt(t *testing.T) {
	updater, paths := helperUpdaterFixture(t, arm64ELF([]byte("installed helper")), arm64ELF([]byte("released helper")), true)
	downgradeRequestToSchemaOne(t, paths)

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt := readReceipt(t, paths)
	if receipt.SchemaVersion != RequestSchemaVersion || receipt.HelperState != "" {
		t.Fatalf("receipt = %+v, want a schema 1 receipt an older manager can read", receipt)
	}
	live, err := os.ReadFile(paths.helperPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(live, arm64ELF([]byte("installed helper"))) {
		t.Fatal("a schema 1 request swapped the helper")
	}
}

// A resumed transaction is one a crash or a power cut interrupted. The helper
// never swaps itself on that path, and it says so rather than reporting a
// swap that never ran.
func TestBootRecoveryNeverSwapsTheHelper(t *testing.T) {
	installed := arm64ELF([]byte("installed helper"))
	updater, paths := helperUpdaterFixture(t, installed, arm64ELF([]byte("released helper")), true)
	// Carry the transaction to the point a power cut would leave it: the
	// target slot is selected, the journal says switched, and the request
	// has already been consumed.
	if err := os.MkdirAll(filepath.Join(paths.VersionsDir, "v2.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.VersionsDir, "v2.0.0", managerFileName), arm64ELF([]byte("target")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths.CurrentLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "v2.0.0"), paths.CurrentLink); err != nil {
		t.Fatal(err)
	}
	transactionDir := paths.transactionDir("attempt-helper")
	if err := os.MkdirAll(transactionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var request ApplyRequest
	if err := readJSONFile(paths.requestPath(), 64<<10, &request); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ManifestAssetName, SignatureAssetName, helperFileName} {
		payload, err := os.ReadFile(filepath.Join(paths.pendingDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(transactionDir, name), payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(paths.requestPath()); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(paths.journalPath(), Journal{
		SchemaVersion: journalSchemaVersion, AttemptID: "attempt-helper", State: JournalSwitched,
		TargetVersion: "v2.0.0", PreviousSlot: "v1.0.0", PreviousVersion: "v1.0.0",
		ManifestSHA256: request.ManifestSHA256, UpdatedAt: "2026-08-12T00:00:00Z",
		RequestSchema: RequestHelperSchemaVersion, HelperSHA256: request.HelperSHA256,
	}, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(paths.helperPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(live, installed) {
		t.Fatal("boot recovery swapped the helper")
	}
	receipt := readReceipt(t, paths)
	if receipt.State != "succeeded" || !strings.Contains(receipt.HelperState, "recovered transaction") {
		t.Fatalf("receipt = %+v, want a succeeded update naming the swap it did not run", receipt)
	}
}

func downgradeRequestToSchemaOne(t *testing.T, paths Paths) {
	t.Helper()
	var request ApplyRequest
	if err := readJSONFile(paths.requestPath(), 64<<10, &request); err != nil {
		t.Fatal(err)
	}
	request.SchemaVersion = RequestSchemaVersion
	request.HelperSHA256 = ""
	if err := writeJSONFile(paths.requestPath(), request, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readReceipt(t *testing.T, paths Paths) Receipt {
	t.Helper()
	var receipt Receipt
	if err := readJSONFile(paths.receiptPath(), 64<<10, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

// helperUpdaterFixture builds the machine a schema-2 release lands on: an
// installed helper inside the updater directory that its own unit already
// makes writable, and a handoff carrying the manager payload plus, when the
// manager staged one, the signed helper.
func helperUpdaterFixture(t *testing.T, installedHelper, releaseHelper []byte, stageHelper bool) (*Updater, Paths) {
	t.Helper()
	root := t.TempDir()
	paths := Paths{
		InstallRoot: filepath.Join(root, "usr", "lib", "basement"),
		VersionsDir: filepath.Join(root, "usr", "lib", "basement", "versions"),
		CurrentLink: filepath.Join(root, "usr", "lib", "basement", "current"),
		LegacyLink:  filepath.Join(root, "usr", "lib", "basement", "basement"),
		StagingDir:  filepath.Join(root, "var", "lib", "basement", "updates", "staging"),
		StateDir:    filepath.Join(root, "var", "lib", "basement-updater"),
	}
	if err := os.MkdirAll(filepath.Join(paths.VersionsDir, "v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.VersionsDir, "v1.0.0", managerFileName), arm64ELF([]byte("previous")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "v1.0.0"), paths.CurrentLink); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.updaterDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.helperPath(), installedHelper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.pendingDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manager := arm64ELF([]byte("target"))
	digest := sha256.Sum256(manager)
	helperDigest := sha256.Sum256(releaseHelper)
	manifestBytes, err := MarshalManifest(Manifest{
		SchemaVersion: ManifestHelperSchemaVersion, KeyID: "release-test", ReleaseVersion: "v2.0.0",
		OS: "linux", Arch: "arm64", AssetName: LinuxARM64AssetName,
		AssetSize: int64(len(manager)), AssetSHA256: hex.EncodeToString(digest[:]), UpdaterProtocol: 2,
		RollbackFrom:    []string{"v1.0.0"},
		HelperAssetName: LinuxARM64HelperAssetName, HelperSize: int64(len(releaseHelper)), HelperSHA256: hex.EncodeToString(helperDigest[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifestBytes))), '\n')
	staged := map[string][]byte{
		ManifestAssetName: manifestBytes, SignatureAssetName: signature, managerFileName: manager,
	}
	if stageHelper {
		staged[helperFileName] = releaseHelper
	}
	for name, payload := range staged {
		if err := os.WriteFile(filepath.Join(paths.pendingDir(), name), payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJSONFile(paths.requestPath(), ApplyRequest{
		SchemaVersion: RequestHelperSchemaVersion, AttemptID: "attempt-helper", RunningVersion: "v1.0.0", TargetVersion: "v2.0.0",
		ManifestSHA256: ManifestDigest(manifestBytes), HelperSHA256: hex.EncodeToString(helperDigest[:]),
	}, 0o600); err != nil {
		t.Fatal(err)
	}
	health := healthCheckFunc(func(context.Context, string, string) error { return nil })
	return &Updater{Paths: paths, Keys: KeyRing{"release-test": publicKey}, Service: &fakeServiceController{}, Health: health}, paths
}

func updaterFixture(t *testing.T, health HealthChecker) (*Updater, Paths) {
	t.Helper()
	root := t.TempDir()
	paths := Paths{
		InstallRoot: filepath.Join(root, "usr", "lib", "basement"),
		VersionsDir: filepath.Join(root, "usr", "lib", "basement", "versions"),
		CurrentLink: filepath.Join(root, "usr", "lib", "basement", "current"),
		LegacyLink:  filepath.Join(root, "usr", "lib", "basement", "basement"),
		StagingDir:  filepath.Join(root, "var", "lib", "basement", "updates", "staging"),
		StateDir:    filepath.Join(root, "var", "lib", "basement-updater"),
	}
	if err := os.MkdirAll(filepath.Join(paths.VersionsDir, "v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.VersionsDir, "v1.0.0", managerFileName), arm64ELF([]byte("previous")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "v1.0.0"), paths.CurrentLink); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.pendingDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manager := arm64ELF([]byte("target"))
	digest := sha256.Sum256(manager)
	manifestBytes, err := MarshalManifest(Manifest{
		SchemaVersion: ManifestSchemaVersion, KeyID: "release-test", ReleaseVersion: "v2.0.0",
		OS: "linux", Arch: "arm64", AssetName: LinuxARM64AssetName,
		AssetSize: int64(len(manager)), AssetSHA256: hex.EncodeToString(digest[:]), UpdaterProtocol: UpdaterProtocol,
		RollbackFrom: []string{"v1.0.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifestBytes))), '\n')
	for name, payload := range map[string][]byte{
		ManifestAssetName:  manifestBytes,
		SignatureAssetName: signature,
		managerFileName:    manager,
	} {
		if err := os.WriteFile(filepath.Join(paths.pendingDir(), name), payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJSONFile(paths.requestPath(), ApplyRequest{
		SchemaVersion: 1, AttemptID: "attempt-test", RunningVersion: "v1.0.0", TargetVersion: "v2.0.0",
		ManifestSHA256: ManifestDigest(manifestBytes),
	}, 0o600); err != nil {
		t.Fatal(err)
	}
	if health == nil {
		health = healthCheckFunc(func(context.Context, string, string) error { return nil })
	}
	return &Updater{Paths: paths, Keys: KeyRing{"release-test": publicKey}, Service: &fakeServiceController{}, Health: health}, paths
}

func assertSelectedSlot(t *testing.T, paths Paths, want string) {
	t.Helper()
	target, err := os.Readlink(paths.CurrentLink)
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join("versions", want) {
		t.Fatalf("current = %q, want %q", target, filepath.Join("versions", want))
	}
}

func arm64ELF(suffix []byte) []byte {
	payload := make([]byte, 64+len(suffix))
	copy(payload[:4], []byte{0x7f, 'E', 'L', 'F'})
	payload[18] = 183
	copy(payload[64:], suffix)
	return payload
}
