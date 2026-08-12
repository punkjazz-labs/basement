package update

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The embedded unit texts must stay identical to the packaged ones, or the
// updater would reconcile a machine towards units nobody reviewed. This is the
// same guarantee internal/setup already carries for the installer, and both
// copies answer to packaging/systemd as the single source of truth.
func TestEmbeddedUnitMatchesPackagedUnit(t *testing.T) {
	for _, unit := range embeddedUnits() {
		packaged, err := os.ReadFile("../../packaging/systemd/" + unit.name)
		if err != nil {
			t.Fatal(err)
		}
		if string(packaged) != unit.text {
			t.Fatalf("embedded updater asset %s differs from its packaged systemd unit", unit.name)
		}
	}
}

// The updater unit this release ships is the one that grants generation 2. It
// arrives through one more installer run, never through an update, which is
// the chicken-and-egg ADR 0020 states openly.
func TestPackagedUpdaterUnitGrantsTheUnitDirectory(t *testing.T) {
	if !strings.Contains(updaterServiceUnitText, "ReadWritePaths=") {
		t.Fatal("the updater unit no longer declares ReadWritePaths")
	}
	for _, directive := range []string{
		"ReadWritePaths=/usr/lib/basement /var/lib/basement/updates /var/lib/basement-updater /etc/systemd/system",
		"UMask=0077", "ProtectSystem=strict", "NoNewPrivileges=yes", "MemoryDenyWriteExecute=yes",
		"CapabilityBoundingSet=", "AmbientCapabilities=",
	} {
		if !strings.Contains(updaterServiceUnitText, directive) {
			t.Fatalf("the updater unit no longer carries %q", directive)
		}
	}
}

func TestUnitWriteProbeAnswersWhatTheSandboxAllows(t *testing.T) {
	directory := t.TempDir()
	if err := probeUnitWrite(directory); err != nil {
		t.Fatalf("probe refused a writable directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, unitProbeName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe left its file behind: %v", err)
	}

	// The crash case: a process killed between the create and the remove
	// leaves the fixed-name file behind, and the next run clears exactly it.
	if err := os.WriteFile(filepath.Join(directory, unitProbeName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeUnitWrite(directory); err != nil {
		t.Fatalf("probe refused a directory holding its own debris: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, unitProbeName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe did not clear earlier debris: %v", err)
	}

	if err := probeUnitWrite(filepath.Join(directory, "absent")); err == nil {
		t.Fatal("probe accepted a directory that does not exist")
	}
	if os.Geteuid() == 0 {
		// Root ignores the mode bits below, and the real refusal on a
		// machine is the unit sandbox rather than a permission bit.
		return
	}
	refused := filepath.Join(directory, "refused")
	if err := os.Mkdir(refused, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := probeUnitWrite(refused); err == nil {
		t.Fatal("probe accepted a directory it cannot write")
	}
}

func TestReconcileWritesOnlyTheUnitsThatDiffer(t *testing.T) {
	// The unit runs this process under UMask=0077, which would leave a unit
	// file root-only. Every write chmods explicitly, so the mode below is
	// 0644 whatever the umask says.
	previousUmask := syscall.Umask(0o077)
	defer syscall.Umask(previousUmask)

	updater, paths, units, reloader := generationTwoFixture(t, map[string]string{
		"basement.service":         embeddedText(t, "basement.service"),
		"basement-updater.service": "[Unit]\nDescription=a unit an older release installed\n",
		"basement-updater.path":    embeddedText(t, "basement-updater.path"),
	})
	// An untouched file keeps the mode it had. A rewrite would land on 0644,
	// so this is how the test proves nothing was rewritten for its own sake.
	if err := os.Chmod(filepath.Join(units, "basement.service"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSlot(t, paths, "v2.0.0")
	receipt := readReceipt(t, paths)
	if receipt.State != "succeeded" || receipt.Units != UnitsReconciled {
		t.Fatalf("receipt = %+v, want a succeeded update reporting reconciled units", receipt)
	}
	if reloader.reloads != 1 {
		t.Fatalf("daemon reloads = %d, want exactly one for the whole set", reloader.reloads)
	}
	assertUnitFile(t, units, "basement-updater.service", embeddedText(t, "basement-updater.service"), 0o644)
	assertUnitFile(t, units, "basement-updater.service.previous", "[Unit]\nDescription=a unit an older release installed\n", 0o644)
	assertUnitMode(t, units, "basement.service", 0o600)
	for _, name := range []string{"basement.service.previous", "basement-updater.path.previous"} {
		if _, err := os.Stat(filepath.Join(units, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a unit that already matched was backed up as %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(units, unitProbeName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the write probe left a file in the unit directory: %v", err)
	}
}

func TestReconcileDoesNotReloadWhenNothingDiffers(t *testing.T) {
	updater, paths, units, reloader := generationTwoFixture(t, currentUnits(t))

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt := readReceipt(t, paths)
	if receipt.Units != UnitsUnchanged {
		t.Fatalf("units = %q, want %q", receipt.Units, UnitsUnchanged)
	}
	if reloader.reloads != 0 {
		t.Fatalf("daemon reloads = %d, want none when nothing was written", reloader.reloads)
	}
	for _, name := range []string{"basement.service.previous", "basement-updater.service.previous", "basement-updater.path.previous"} {
		if _, err := os.Stat(filepath.Join(units, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("nothing differed yet %s was written: %v", name, err)
		}
	}
}

// A machine whose updater unit does not grant the directory is generation 1.
// That is not an error and never fails an update; it is the state most
// machines are in until one more installer run.
func TestUnitReconcileIsNotPermittedWithoutSandboxAccess(t *testing.T) {
	updater, paths, units, reloader := generationTwoFixture(t, currentUnits(t))
	updater.Paths.UnitDir = filepath.Join(units, "not-granted")

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSlot(t, paths, "v2.0.0")
	receipt := readReceipt(t, paths)
	if receipt.State != "succeeded" || receipt.Units != UnitsNotPermitted {
		t.Fatalf("receipt = %+v, want a succeeded update reporting %q", receipt, UnitsNotPermitted)
	}
	if reloader.reloads != 0 {
		t.Fatalf("daemon reloads = %d, want none when the probe refused", reloader.reloads)
	}
}

func TestRollbackNeverReconcilesUnits(t *testing.T) {
	stale := "[Unit]\nDescription=a unit an older release installed\n"
	updater, paths, units, reloader := generationTwoFixture(t, map[string]string{
		"basement.service":         stale,
		"basement-updater.service": stale,
		"basement-updater.path":    stale,
	})
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
	receipt := readReceipt(t, paths)
	if receipt.State != "rolled_back" || receipt.Units != "" {
		t.Fatalf("receipt = %+v, want a rollback carrying no units field", receipt)
	}
	if reloader.reloads != 0 {
		t.Fatalf("daemon reloads = %d, want none on a rollback", reloader.reloads)
	}
	assertUnitFile(t, units, "basement.service", stale, 0o644)
}

// A resumed transaction is one a crash or a power cut interrupted. Units are
// reconciled only in the window this process carried from the request through
// to a healthy manager, and the receipt says so rather than reporting work
// that never ran.
func TestBootRecoveryNeverReconcilesUnits(t *testing.T) {
	stale := "[Unit]\nDescription=a unit an older release installed\n"
	updater, paths, units, reloader := generationTwoFixture(t, map[string]string{
		"basement.service":         stale,
		"basement-updater.service": stale,
		"basement-updater.path":    stale,
	})
	carryToBootRecovery(t, paths)

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt := readReceipt(t, paths)
	if receipt.State != "succeeded" || !strings.Contains(receipt.Units, "recovered transaction") {
		t.Fatalf("receipt = %+v, want a succeeded update naming the reconcile it did not run", receipt)
	}
	if reloader.reloads != 0 {
		t.Fatalf("daemon reloads = %d, want none at boot recovery", reloader.reloads)
	}
	assertUnitFile(t, units, "basement-updater.service", stale, 0o644)
}

// The mixed case: an older manager's schema-1 request produces a schema-1
// receipt, and that manager decodes strictly, so the field must be absent from
// the bytes rather than merely empty.
func TestSchemaOneRequestCarriesNoUnitsField(t *testing.T) {
	stale := "[Unit]\nDescription=a unit an older release installed\n"
	updater, paths, units, reloader := generationTwoFixture(t, map[string]string{
		"basement.service":         stale,
		"basement-updater.service": stale,
		"basement-updater.path":    stale,
	})
	downgradeRequestToSchemaOne(t, paths)

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt := readReceipt(t, paths)
	if receipt.SchemaVersion != RequestSchemaVersion || receipt.Units != "" {
		t.Fatalf("receipt = %+v, want a schema 1 receipt an older manager can read", receipt)
	}
	payload, err := os.ReadFile(paths.receiptPath())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["units"]; present {
		t.Fatalf("a schema 1 receipt carries a units field: %s", payload)
	}
	if reloader.reloads != 0 {
		t.Fatalf("daemon reloads = %d, want none for a schema 1 request", reloader.reloads)
	}
	assertUnitFile(t, units, "basement-updater.service", stale, 0o644)
}

// One unit that cannot be written is recorded and nothing more. The manager
// became healthy, which is what the update was for, and the units that could
// be reconciled still were.
func TestOneFailedUnitWriteDoesNotFailTheUpdate(t *testing.T) {
	stale := "[Unit]\nDescription=a unit an older release installed\n"
	updater, paths, units, reloader := generationTwoFixture(t, map[string]string{
		"basement.service":         stale,
		"basement-updater.service": stale,
		"basement-updater.path":    stale,
	})
	// A directory where the recovery copy has to go: the backup fails, so
	// that unit is left exactly as it was.
	if err := os.Mkdir(filepath.Join(units, "basement.service.previous"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSlot(t, paths, "v2.0.0")
	receipt := readReceipt(t, paths)
	if receipt.State != "succeeded" {
		t.Fatalf("receipt state = %q, a failed unit write must not fail the update", receipt.State)
	}
	if !strings.HasPrefix(receipt.Units, unitsFailPrefix) {
		t.Fatalf("units = %q, want a named failure", receipt.Units)
	}
	assertUnitFile(t, units, "basement.service", stale, 0o644)
	assertUnitFile(t, units, "basement-updater.service", embeddedText(t, "basement-updater.service"), 0o644)
	if reloader.reloads != 1 {
		t.Fatalf("daemon reloads = %d, want one for the units that were written", reloader.reloads)
	}
}

// A reload that fails after a good write is recorded, never treated as a
// rollback trigger: the new text is on disk and systemd picks it up at the
// next reload or boot.
func TestFailedDaemonReloadIsRecordedNotRolledBack(t *testing.T) {
	stale := "[Unit]\nDescription=a unit an older release installed\n"
	updater, paths, units, reloader := generationTwoFixture(t, map[string]string{
		"basement.service":         stale,
		"basement-updater.service": stale,
		"basement-updater.path":    stale,
	})
	reloader.err = errors.New("systemd refused the reload")

	if err := updater.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSlot(t, paths, "v2.0.0")
	receipt := readReceipt(t, paths)
	if receipt.State != "succeeded" || !strings.HasPrefix(receipt.Units, unitsFailPrefix) {
		t.Fatalf("receipt = %+v, want a succeeded update naming the reload failure", receipt)
	}
	assertUnitFile(t, units, "basement.service", embeddedText(t, "basement.service"), 0o644)
}

// The report the console reads. Nothing has probed anything until a schema-2
// receipt exists, and claiming a machine needs an installer run before then
// would be a guess.
func TestUnitUpdateReportFollowsTheReceipt(t *testing.T) {
	directory := t.TempDir()
	stager := &Stager{RootStatusPath: filepath.Join(directory, "status.json")}
	if report := stager.UnitUpdateReport(); report.State != unitUpdatesUnknown || report.Note != "" {
		t.Fatalf("report with no receipt = %+v, want unknown", report)
	}
	for _, fixture := range []struct {
		units string
		state string
		note  string
	}{
		{units: UnitsNotPermitted, state: unitUpdatesUnavailable, note: InstallerEnablesUnitUpdates},
		{units: UnitsReconciled, state: unitUpdatesAvailable},
		{units: UnitsUnchanged, state: unitUpdatesAvailable},
		{units: unitsFailed("systemd refused the reload"), state: unitUpdatesAvailable},
	} {
		if err := writeJSONFile(stager.RootStatusPath, Receipt{
			SchemaVersion: RequestHelperSchemaVersion, AttemptID: "attempt-helper", State: "succeeded",
			UpdatedAt: "2026-08-12T00:00:00Z", Units: fixture.units,
		}, 0o644); err != nil {
			t.Fatal(err)
		}
		report := stager.UnitUpdateReport()
		if report.State != fixture.state || report.Note != fixture.note {
			t.Fatalf("report for units %q = %+v, want state %q note %q", fixture.units, report, fixture.state, fixture.note)
		}
	}
}

type fakeReloader struct {
	reloads int
	err     error
}

func (reloader *fakeReloader) DaemonReload(context.Context) error {
	reloader.reloads++
	return reloader.err
}

// generationTwoFixture is the machine one installer run past generation 1: a
// schema-2 handoff whose helper already matches, so the units are the only
// thing left for the post-target_healthy window to do, plus a unit directory
// this process can actually write.
func generationTwoFixture(t *testing.T, installed map[string]string) (*Updater, Paths, string, *fakeReloader) {
	t.Helper()
	helper := arm64ELF([]byte("installed helper"))
	updater, paths := helperUpdaterFixture(t, helper, helper, false)
	units := filepath.Join(t.TempDir(), "systemd")
	if err := os.Mkdir(units, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range installed {
		if err := os.WriteFile(filepath.Join(units, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	updater.Paths.UnitDir = units
	paths.UnitDir = units
	reloader := &fakeReloader{}
	updater.Reloader = reloader
	return updater, paths, units, reloader
}

func currentUnits(t *testing.T) map[string]string {
	t.Helper()
	installed := map[string]string{}
	for _, unit := range embeddedUnits() {
		installed[unit.name] = unit.text
	}
	return installed
}

func embeddedText(t *testing.T, name string) string {
	t.Helper()
	for _, unit := range embeddedUnits() {
		if unit.name == name {
			return unit.text
		}
	}
	t.Fatalf("no embedded unit named %s", name)
	return ""
}

func assertUnitFile(t *testing.T, directory, name, want string, mode os.FileMode) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != want {
		t.Fatalf("%s = %q, want %q", name, payload, want)
	}
	assertUnitMode(t, directory, name, mode)
}

func assertUnitMode(t *testing.T, directory, name string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %o, want %o", name, info.Mode().Perm(), mode)
	}
}

// carryToBootRecovery leaves the transaction where a power cut would: the
// target slot selected, the journal at switched, and the request already
// consumed, so the next run resumes rather than starts.
func carryToBootRecovery(t *testing.T, paths Paths) {
	t.Helper()
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
	for _, name := range []string{ManifestAssetName, SignatureAssetName} {
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
}
