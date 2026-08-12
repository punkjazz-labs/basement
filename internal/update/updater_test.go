package update

import (
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
