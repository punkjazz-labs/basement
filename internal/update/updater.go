package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	requestFileName   = "request.json"
	managerFileName   = "basement"
	helperFileName    = "basement-updater"
	journalFileName   = "journal.json"
	receiptFileName   = "status.json"
	transactionFolder = "transactions"
)

type Paths struct {
	InstallRoot string
	VersionsDir string
	CurrentLink string
	LegacyLink  string
	StagingDir  string
	StateDir    string
}

func SystemPaths() Paths {
	return Paths{
		InstallRoot: "/usr/lib/basement", VersionsDir: "/usr/lib/basement/versions",
		CurrentLink: "/usr/lib/basement/current", LegacyLink: "/usr/lib/basement/basement",
		StagingDir: "/var/lib/basement/updates/staging", StateDir: "/var/lib/basement-updater",
	}
}

func (paths Paths) pendingDir() string  { return filepath.Join(paths.StagingDir, "pending") }
func (paths Paths) requestPath() string { return filepath.Join(paths.pendingDir(), requestFileName) }
func (paths Paths) journalPath() string { return filepath.Join(paths.StateDir, journalFileName) }
func (paths Paths) receiptPath() string { return filepath.Join(paths.StateDir, receiptFileName) }
func (paths Paths) lockPath() string    { return filepath.Join(paths.StateDir, "apply.lock") }
func (paths Paths) transactionDir(id string) string {
	return filepath.Join(paths.StateDir, transactionFolder, id)
}

type ServiceController interface {
	Stop(context.Context) error
	Start(context.Context) error
	MainPID(context.Context) (int, error)
}

type SystemServiceController struct{}

func (SystemServiceController) Stop(ctx context.Context) error {
	return fixedSystemctl(ctx, "stop", "basement.service")
}

func (SystemServiceController) Start(ctx context.Context) error {
	return fixedSystemctl(ctx, "start", "basement.service")
}

func (SystemServiceController) MainPID(ctx context.Context) (int, error) {
	output, err := exec.CommandContext(ctx, "systemctl", "show", "--property=MainPID", "--value", "basement.service").Output()
	if err != nil {
		return 0, fmt.Errorf("read basement.service main process: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || pid <= 0 {
		return 0, errors.New("basement.service has no running main process")
	}
	return pid, nil
}

func fixedSystemctl(ctx context.Context, action, unit string) error {
	command := exec.CommandContext(ctx, "systemctl", action, unit)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s basement.service: %s", action, strings.TrimSpace(string(output)))
	}
	return nil
}

type Updater struct {
	Paths   Paths
	Keys    KeyRing
	Service ServiceController
	Health  HealthChecker
	Now     func() time.Time
}

func NewSystemUpdater(keys KeyRing) *Updater {
	service := SystemServiceController{}
	return &Updater{Paths: SystemPaths(), Keys: keys, Service: service, Health: NewSystemHealthChecker(service), Now: time.Now}
}

func (updater *Updater) Apply(ctx context.Context) error {
	if updater == nil || updater.Service == nil || updater.Health == nil {
		return errors.New("updater dependencies are incomplete")
	}
	if err := os.MkdirAll(updater.Paths.StateDir, 0o755); err != nil {
		return fmt.Errorf("create updater state directory: %w", err)
	}
	lock, err := acquireUpdateLock(updater.Paths.lockPath())
	if err != nil {
		return err
	}
	defer lock.Close()

	journal, err := updater.loadJournal()
	if err == nil {
		switch journal.State {
		case JournalRecoveryRequired:
			// Recovery needs an administrator. Consume a marker written by the
			// manager so the path unit cannot retry this transaction in a loop,
			// but preserve the recovery receipt that explains what happened.
			if removeErr := os.Remove(updater.Paths.requestPath()); removeErr == nil {
				return syncDirectory(updater.Paths.pendingDir())
			} else if !errors.Is(removeErr, os.ErrNotExist) {
				return removeErr
			}
			return nil
		case JournalTargetHealthy, JournalRolledBack:
			// A completed transaction does not block a later update.
		default:
			return updater.resume(ctx, journal)
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(updater.Paths.requestPath()); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return updater.prepareAndApply(ctx)
}

func (updater *Updater) prepareAndApply(ctx context.Context) (resultErr error) {
	prepared := false
	defer func() {
		if resultErr != nil && !prepared {
			updater.quarantineRequest(resultErr)
		}
	}()
	rootRequestPath := filepath.Join(updater.Paths.StateDir, requestFileName)
	if err := copyRegularFile(updater.Paths.requestPath(), rootRequestPath, 0o600); err != nil {
		return fmt.Errorf("copy update request: %w", err)
	}
	var request ApplyRequest
	if err := readJSONFile(rootRequestPath, 64<<10, &request); err != nil {
		return fmt.Errorf("read update request: %w", err)
	}
	if err := validateApplyRequest(request); err != nil {
		return err
	}
	transactionDir := updater.Paths.transactionDir(request.AttemptID)
	if err := os.RemoveAll(transactionDir); err != nil {
		return err
	}
	if err := os.MkdirAll(transactionDir, 0o700); err != nil {
		return err
	}
	for _, name := range []string{ManifestAssetName, SignatureAssetName, managerFileName} {
		mode := os.FileMode(0o600)
		if name == managerFileName {
			mode = 0o700
		}
		if err := copyRegularFile(filepath.Join(updater.Paths.pendingDir(), name), filepath.Join(transactionDir, name), mode); err != nil {
			return fmt.Errorf("copy staged %s: %w", name, err)
		}
	}
	// The staged helper is optional even under a schema-2 request: the
	// manager stages nothing when the installed helper already matches, and
	// nothing it could not download. A missing helper is recorded later as a
	// swap that did not happen, never as a failed update.
	if request.SchemaVersion == RequestHelperSchemaVersion {
		if err := copyRegularFile(filepath.Join(updater.Paths.pendingDir(), helperFileName), filepath.Join(transactionDir, helperFileName), 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("copy staged %s: %w", helperFileName, err)
		}
	}
	manifestBytes, err := os.ReadFile(filepath.Join(transactionDir, ManifestAssetName))
	if err != nil {
		return err
	}
	signature, err := os.ReadFile(filepath.Join(transactionDir, SignatureAssetName))
	if err != nil {
		return err
	}
	if ManifestDigest(manifestBytes) != request.ManifestSHA256 {
		return errors.New("staged manifest digest does not match the update request")
	}
	manifest, err := VerifySignedManifest(manifestBytes, signature, updater.Keys)
	if err != nil {
		return err
	}
	if manifest.ReleaseVersion != request.TargetVersion {
		return errors.New("staged manifest target does not match the update request")
	}
	if err := ValidateCandidate(manifest, request.TargetVersion, request.RunningVersion); err != nil {
		return err
	}
	asset, err := os.Open(filepath.Join(transactionDir, managerFileName))
	if err != nil {
		return err
	}
	verifyErr := VerifyAsset(asset, manifest.AssetSize, manifest.AssetSHA256)
	closeErr := asset.Close()
	if verifyErr != nil {
		return verifyErr
	}
	if closeErr != nil {
		return closeErr
	}
	previousSlot, err := updater.currentSlot()
	if err != nil {
		return err
	}
	// A bootstrap slot carries no version in its name, so this agreement
	// check only applies to slots this updater itself installed. The
	// running version is still cross-checked: the manager validated the
	// candidate against it and this process re-verified the signed
	// manifest before reaching here.
	if _, err := ParseVersion(previousSlot); err == nil && previousSlot != request.RunningVersion {
		return errors.New("selected slot no longer matches the running manager version")
	}
	temporarySlot, err := updater.prepareSlot(transactionDir, manifest)
	if err != nil {
		return err
	}
	journal := Journal{
		SchemaVersion: journalSchemaVersion, AttemptID: request.AttemptID, State: JournalPrepared,
		TargetVersion: request.TargetVersion, PreviousSlot: previousSlot, PreviousVersion: request.RunningVersion,
		ManifestSHA256: request.ManifestSHA256, UpdatedAt: journalNow(updater.Now),
		RequestSchema: request.SchemaVersion, HelperSHA256: request.HelperSHA256,
	}
	if err := updater.writeJournal(journal); err != nil {
		return err
	}
	if err := updater.writeReceipt(Receipt{
		SchemaVersion: journal.receiptSchema(), AttemptID: journal.AttemptID, State: "restarting", TargetVersion: journal.TargetVersion,
		RunningVersion: journal.PreviousVersion, PreviousVersion: journal.PreviousVersion, UpdatedAt: journal.UpdatedAt,
	}); err != nil {
		return err
	}
	prepared = true
	if err := os.Remove(updater.Paths.requestPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDirectory(updater.Paths.pendingDir()); err != nil {
		return err
	}
	return updater.switchTarget(ctx, journal, temporarySlot, manifest, true)
}

func (updater *Updater) resume(ctx context.Context, journal Journal) error {
	if err := validateJournal(journal); err != nil {
		return err
	}
	current, err := updater.currentSlot()
	if err != nil {
		return updater.recoveryRequired(journal, err)
	}
	switch journal.State {
	case JournalPrepared:
		if current == journal.TargetVersion {
			journal.State = JournalSwitched
			journal.UpdatedAt = journalNow(updater.Now)
			if err := updater.writeJournal(journal); err != nil {
				return err
			}
			return updater.checkTarget(ctx, journal, false)
		}
		if current != journal.PreviousSlot {
			return updater.recoveryRequired(journal, errors.New("selected slot is neither the target nor the recorded previous slot"))
		}
		transactionDir := updater.Paths.transactionDir(journal.AttemptID)
		manifestBytes, err := os.ReadFile(filepath.Join(transactionDir, ManifestAssetName))
		if err != nil {
			return updater.recoveryRequired(journal, err)
		}
		signature, err := os.ReadFile(filepath.Join(transactionDir, SignatureAssetName))
		if err != nil {
			return updater.recoveryRequired(journal, err)
		}
		manifest, err := VerifySignedManifest(manifestBytes, signature, updater.Keys)
		if err != nil {
			return updater.recoveryRequired(journal, err)
		}
		// A resumed transaction is one a crash or a power cut interrupted.
		// The helper never swaps itself on that path: the swap is only ever
		// the tail of a transaction this process carried from the request
		// through to a healthy target.
		return updater.switchTarget(ctx, journal, filepath.Join(updater.Paths.VersionsDir, "."+journal.TargetVersion+".prepared"), manifest, false)
	case JournalSwitched:
		if current != journal.TargetVersion {
			return updater.rollback(ctx, journal, errors.New("target slot is no longer selected"))
		}
		return updater.checkTarget(ctx, journal, false)
	case JournalRollbackSwitched:
		if current != journal.PreviousSlot {
			if err := updater.selectSlot(journal.PreviousSlot); err != nil {
				return updater.recoveryRequired(journal, err)
			}
		}
		return updater.checkRollback(ctx, journal)
	default:
		return nil
	}
}

func (updater *Updater) switchTarget(ctx context.Context, journal Journal, temporarySlot string, manifest Manifest, maySwapHelper bool) error {
	if err := updater.Service.Stop(ctx); err != nil {
		return updater.recoveryRequired(journal, err)
	}
	targetSlot := filepath.Join(updater.Paths.VersionsDir, manifest.ReleaseVersion)
	if _, err := os.Stat(targetSlot); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(temporarySlot, targetSlot); err != nil {
			return updater.recoveryRequired(journal, err)
		}
		if err := syncDirectory(updater.Paths.VersionsDir); err != nil {
			return updater.recoveryRequired(journal, err)
		}
	} else if err != nil {
		return updater.recoveryRequired(journal, err)
	} else if err := verifyAssetPath(filepath.Join(targetSlot, managerFileName), manifest); err != nil {
		return updater.recoveryRequired(journal, errors.New("existing target slot does not match the signed release"))
	} else {
		_ = os.RemoveAll(temporarySlot)
	}
	if err := updater.selectSlot(manifest.ReleaseVersion); err != nil {
		return updater.recoveryRequired(journal, err)
	}
	journal.State = JournalSwitched
	journal.UpdatedAt = journalNow(updater.Now)
	if err := updater.writeJournal(journal); err != nil {
		return err
	}
	if err := updater.writeReceipt(Receipt{
		SchemaVersion: journal.receiptSchema(), AttemptID: journal.AttemptID, State: "checking_health", TargetVersion: journal.TargetVersion,
		RunningVersion: journal.TargetVersion, PreviousVersion: journal.PreviousVersion, UpdatedAt: journal.UpdatedAt,
	}); err != nil {
		return err
	}
	return updater.checkTarget(ctx, journal, maySwapHelper)
}

func (updater *Updater) checkTarget(ctx context.Context, journal Journal, maySwapHelper bool) error {
	if err := updater.Service.Start(ctx); err != nil {
		return updater.rollback(ctx, journal, err)
	}
	executable := filepath.Join(updater.Paths.VersionsDir, journal.TargetVersion, managerFileName)
	if err := updater.Health.Check(ctx, journal.TargetVersion, executable); err != nil {
		return updater.rollback(ctx, journal, err)
	}
	journal.State = JournalTargetHealthy
	journal.Failure = ""
	journal.UpdatedAt = journalNow(updater.Now)
	if err := updater.writeJournal(journal); err != nil {
		return err
	}
	// Only here, with the target manager proven healthy, and never on any
	// rollback path. Whatever this returns, the update is a success: the
	// manager did become healthy, which is the outcome the receipt reports.
	helperState := updater.swapHelper(journal, maySwapHelper)
	if err := updater.writeReceipt(Receipt{
		SchemaVersion: journal.receiptSchema(), AttemptID: journal.AttemptID, State: "succeeded", TargetVersion: journal.TargetVersion,
		RunningVersion: journal.TargetVersion, PreviousVersion: journal.PreviousVersion, UpdatedAt: journal.UpdatedAt,
		HelperState: helperState,
	}); err != nil {
		return err
	}
	updater.pruneSlots(journal.TargetVersion, journal.PreviousSlot)
	return nil
}

func (paths Paths) updaterDir() string   { return filepath.Join(paths.InstallRoot, "updater") }
func (paths Paths) helperPath() string   { return filepath.Join(paths.updaterDir(), helperFileName) }
func (paths Paths) helperNext() string   { return paths.helperPath() + ".next" }
func (paths Paths) helperBackup() string { return paths.helperPath() + ".previous" }

// swapHelper replaces the root updater helper with the bytes this release
// signed. It returns the helper_state to record, which is empty for a
// schema-1 request: an older manager decodes the receipt strictly and must
// never meet a field it does not know.
//
// Renaming over the path of a running executable is safe on Linux. This
// process holds its own inode and keeps executing the old bytes until it
// exits. The live path is never opened for writing, which is what would
// return ETXTBSY, and MemoryDenyWriteExecute is not violated because these
// bytes are written to a file and never mapped executable in this run.
func (updater *Updater) swapHelper(journal Journal, maySwap bool) string {
	if journal.receiptSchema() != RequestHelperSchemaVersion {
		return ""
	}
	transactionDir := updater.Paths.transactionDir(journal.AttemptID)
	manifest, err := updater.verifiedManifest(transactionDir)
	if err != nil {
		return helperSwapFailed(cleanFailure(err))
	}
	if manifest.SchemaVersion != ManifestHelperSchemaVersion || manifest.HelperSHA256 != journal.HelperSHA256 {
		return helperSwapFailed("the signed release does not name the helper this request promised")
	}
	live, err := FileDigest(updater.Paths.helperPath())
	if err != nil {
		return helperSwapFailed(cleanFailure(err))
	}
	if live == manifest.HelperSHA256 {
		return HelperStateUnchanged
	}
	if !maySwap {
		return helperSwapFailed("a recovered transaction does not swap the helper")
	}
	if err := updater.replaceHelper(transactionDir, manifest); err != nil {
		return helperSwapFailed(cleanFailure(err))
	}
	return HelperStateUpdated
}

func (updater *Updater) verifiedManifest(transactionDir string) (Manifest, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(transactionDir, ManifestAssetName))
	if err != nil {
		return Manifest{}, err
	}
	signature, err := os.ReadFile(filepath.Join(transactionDir, SignatureAssetName))
	if err != nil {
		return Manifest{}, err
	}
	// The helper verifies with its own compiled-in key ring, exactly as it
	// verified the manager payload. The ring is never data and never comes
	// from the manifest.
	return VerifySignedManifest(manifestBytes, signature, updater.Keys)
}

func (updater *Updater) replaceHelper(transactionDir string, manifest Manifest) error {
	staged := filepath.Join(transactionDir, helperFileName)
	if err := verifyHelperPath(staged, manifest); err != nil {
		return err
	}
	next := updater.Paths.helperNext()
	if err := os.Remove(next); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Every mode below is set with an explicit Chmod. The unit runs this
	// process under UMask=0077, which would otherwise leave the new helper
	// at 0700: root would still execute it, but the manager could no longer
	// hash it and staleness detection would silently degrade to unknown.
	if err := copySyncedFile(staged, next, 0o755); err != nil {
		return err
	}
	if err := copySyncedFile(updater.Paths.helperPath(), updater.Paths.helperBackup(), 0o755); err != nil {
		_ = os.Remove(next)
		return err
	}
	if err := syncDirectory(updater.Paths.updaterDir()); err != nil {
		_ = os.Remove(next)
		return err
	}
	if err := os.Rename(next, updater.Paths.helperPath()); err != nil {
		_ = os.Remove(next)
		return err
	}
	return syncDirectory(updater.Paths.updaterDir())
}

// copySyncedFile writes destination from source at an explicit mode and
// leaves the bytes on the platter. It writes to the final name rather than a
// temporary one because both of its call sites are names no other process
// reads: the live helper path is only ever reached by rename.
func copySyncedFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	if err := output.Chmod(mode); err != nil {
		output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func (updater *Updater) rollback(ctx context.Context, journal Journal, targetFailure error) error {
	_ = updater.Service.Stop(ctx)
	if err := updater.selectSlot(journal.PreviousSlot); err != nil {
		return updater.recoveryRequired(journal, errors.Join(targetFailure, err))
	}
	journal.State = JournalRollbackSwitched
	journal.Failure = cleanFailure(targetFailure)
	journal.UpdatedAt = journalNow(updater.Now)
	if err := updater.writeJournal(journal); err != nil {
		return err
	}
	if err := updater.writeReceipt(Receipt{
		SchemaVersion: journal.receiptSchema(), AttemptID: journal.AttemptID, State: "checking_health", TargetVersion: journal.TargetVersion,
		RunningVersion: journal.PreviousVersion, PreviousVersion: journal.PreviousVersion,
		Failure: journal.Failure, UpdatedAt: journal.UpdatedAt,
	}); err != nil {
		return err
	}
	return updater.checkRollback(ctx, journal)
}

func (updater *Updater) checkRollback(ctx context.Context, journal Journal) error {
	if err := updater.Service.Start(ctx); err != nil {
		return updater.recoveryRequired(journal, err)
	}
	executable := filepath.Join(updater.Paths.VersionsDir, journal.PreviousSlot, managerFileName)
	if err := updater.Health.Check(ctx, journal.PreviousVersion, executable); err != nil {
		return updater.recoveryRequired(journal, errors.Join(errors.New(journal.Failure), err))
	}
	journal.State = JournalRolledBack
	journal.UpdatedAt = journalNow(updater.Now)
	if err := updater.writeJournal(journal); err != nil {
		return err
	}
	if err := updater.writeReceipt(Receipt{
		SchemaVersion: journal.receiptSchema(), AttemptID: journal.AttemptID, State: "rolled_back", TargetVersion: journal.TargetVersion,
		RunningVersion: journal.PreviousVersion, PreviousVersion: journal.PreviousVersion,
		Failure: journal.Failure, UpdatedAt: journal.UpdatedAt,
	}); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(updater.Paths.VersionsDir, journal.TargetVersion))
	return nil
}

func (updater *Updater) recoveryRequired(journal Journal, cause error) error {
	journal.State = JournalRecoveryRequired
	journal.Failure = cleanFailure(cause)
	journal.UpdatedAt = journalNow(updater.Now)
	if err := updater.writeJournal(journal); err != nil {
		return errors.Join(cause, err)
	}
	if err := updater.writeReceipt(Receipt{
		SchemaVersion: journal.receiptSchema(), AttemptID: journal.AttemptID, State: "recovery_required", TargetVersion: journal.TargetVersion,
		PreviousVersion: journal.PreviousVersion,
		Failure:         journal.Failure, UpdatedAt: journal.UpdatedAt,
	}); err != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("manager update needs local recovery: %w", cause)
}

func (updater *Updater) prepareSlot(transactionDir string, manifest Manifest) (string, error) {
	if err := os.MkdirAll(updater.Paths.VersionsDir, 0o755); err != nil {
		return "", err
	}
	temporarySlot := filepath.Join(updater.Paths.VersionsDir, "."+manifest.ReleaseVersion+".prepared")
	if err := os.RemoveAll(temporarySlot); err != nil {
		return "", err
	}
	if err := os.Mkdir(temporarySlot, 0o755); err != nil {
		return "", err
	}
	target := filepath.Join(temporarySlot, managerFileName)
	if err := copyRegularFile(filepath.Join(transactionDir, managerFileName), target, 0o755); err != nil {
		return "", err
	}
	// The unit runs this process under UMask=0077, which silently strips
	// the modes requested above to owner-only. The manager service runs as
	// its own user, so a root-owned slot it cannot read or execute crash
	// loops the service straight into rollback (hardware, 2026-08-12).
	// Chmod is not subject to the umask.
	if err := os.Chmod(temporarySlot, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return "", err
	}
	if err := verifyAssetPath(target, manifest); err != nil {
		return "", err
	}
	if err := syncDirectory(temporarySlot); err != nil {
		return "", err
	}
	return temporarySlot, nil
}

func (updater *Updater) selectSlot(slot string) error {
	if !validSlotName(slot) {
		return errors.New("selected manager slot name is invalid")
	}
	if err := os.MkdirAll(updater.Paths.InstallRoot, 0o755); err != nil {
		return err
	}
	temporary := filepath.Join(updater.Paths.InstallRoot, ".current-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Symlink(filepath.Join("versions", slot), temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, updater.Paths.CurrentLink); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return syncDirectory(updater.Paths.InstallRoot)
}

// bootstrapSlotPattern names the content-addressed slots the installer
// creates. Every machine starts on one, because the installer is how the
// manager arrives, so the updater must treat them as legitimate previous
// slots: refusing them refused the first console update any freshly
// installed machine ever attempted (proven on hardware, 2026-08-12).
var bootstrapSlotPattern = regexp.MustCompile(`^bootstrap-[0-9a-f]{64}$`)

func validSlotName(slot string) bool {
	if _, err := ParseVersion(slot); err == nil {
		return true
	}
	return bootstrapSlotPattern.MatchString(slot)
}

func (updater *Updater) currentSlot() (string, error) {
	target, err := os.Readlink(updater.Paths.CurrentLink)
	if err != nil {
		return "", fmt.Errorf("read selected manager slot: %w", err)
	}
	if filepath.IsAbs(target) || filepath.Clean(target) != target || filepath.Dir(target) != "versions" {
		return "", errors.New("selected manager slot link is invalid")
	}
	slot := filepath.Base(target)
	if !validSlotName(slot) {
		return "", errors.New("selected manager slot is neither a stable release nor an installer bootstrap slot")
	}
	return slot, nil
}

func (updater *Updater) loadJournal() (Journal, error) {
	var journal Journal
	if err := readJSONFile(updater.Paths.journalPath(), 64<<10, &journal); err != nil {
		return Journal{}, err
	}
	if err := validateJournal(journal); err != nil {
		return Journal{}, err
	}
	return journal, nil
}

func (updater *Updater) writeJournal(journal Journal) error {
	return writeJSONFile(updater.Paths.journalPath(), journal, 0o600)
}

func (updater *Updater) writeReceipt(receipt Receipt) error {
	return writeJSONFile(updater.Paths.receiptPath(), receipt, 0o644)
}

func (updater *Updater) quarantineRequest(cause error) {
	_ = os.Remove(updater.Paths.requestPath())
	_ = syncDirectory(updater.Paths.pendingDir())
	_ = updater.writeReceipt(Receipt{SchemaVersion: 1, State: "failed_before_handoff", Failure: cleanFailure(cause), UpdatedAt: journalNow(updater.Now)})
}

func (updater *Updater) pruneSlots(keep ...string) {
	wanted := make(map[string]bool, len(keep))
	for _, slot := range keep {
		wanted[slot] = true
	}
	entries, err := os.ReadDir(updater.Paths.VersionsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || wanted[entry.Name()] {
			continue
		}
		if _, err := ParseVersion(entry.Name()); err == nil {
			_ = os.RemoveAll(filepath.Join(updater.Paths.VersionsDir, entry.Name()))
		}
	}
}

func verifyAssetPath(path string, manifest Manifest) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	err = VerifyAsset(file, manifest.AssetSize, manifest.AssetSHA256)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func verifyHelperPath(path string, manifest Manifest) error {
	if manifest.SchemaVersion != ManifestHelperSchemaVersion {
		return errors.New("this release does not sign a root updater helper")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	err = VerifyHelperAsset(file, manifest.HelperSize, manifest.HelperSHA256)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
