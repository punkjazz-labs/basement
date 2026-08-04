package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	requestFileName   = "request.json"
	managerFileName   = "basement"
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
	if previousSlot != request.RunningVersion {
		return errors.New("selected slot no longer matches the running manager version")
	}
	temporarySlot, err := updater.prepareSlot(transactionDir, manifest)
	if err != nil {
		return err
	}
	journal := Journal{
		SchemaVersion: 1, AttemptID: request.AttemptID, State: JournalPrepared,
		TargetVersion: request.TargetVersion, PreviousSlot: previousSlot, PreviousVersion: request.RunningVersion,
		ManifestSHA256: request.ManifestSHA256, UpdatedAt: journalNow(updater.Now),
	}
	if err := updater.writeJournal(journal); err != nil {
		return err
	}
	if err := updater.writeReceipt(Receipt{
		SchemaVersion: 1, AttemptID: journal.AttemptID, State: "restarting", TargetVersion: journal.TargetVersion,
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
	return updater.switchTarget(ctx, journal, temporarySlot, manifest)
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
			return updater.checkTarget(ctx, journal)
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
		return updater.switchTarget(ctx, journal, filepath.Join(updater.Paths.VersionsDir, "."+journal.TargetVersion+".prepared"), manifest)
	case JournalSwitched:
		if current != journal.TargetVersion {
			return updater.rollback(ctx, journal, errors.New("target slot is no longer selected"))
		}
		return updater.checkTarget(ctx, journal)
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

func (updater *Updater) switchTarget(ctx context.Context, journal Journal, temporarySlot string, manifest Manifest) error {
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
		SchemaVersion: 1, AttemptID: journal.AttemptID, State: "checking_health", TargetVersion: journal.TargetVersion,
		RunningVersion: journal.TargetVersion, PreviousVersion: journal.PreviousVersion, UpdatedAt: journal.UpdatedAt,
	}); err != nil {
		return err
	}
	return updater.checkTarget(ctx, journal)
}

func (updater *Updater) checkTarget(ctx context.Context, journal Journal) error {
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
	if err := updater.writeReceipt(Receipt{
		SchemaVersion: 1, AttemptID: journal.AttemptID, State: "succeeded", TargetVersion: journal.TargetVersion,
		RunningVersion: journal.TargetVersion, PreviousVersion: journal.PreviousVersion, UpdatedAt: journal.UpdatedAt,
	}); err != nil {
		return err
	}
	updater.pruneSlots(journal.TargetVersion, journal.PreviousSlot)
	return nil
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
		SchemaVersion: 1, AttemptID: journal.AttemptID, State: "checking_health", TargetVersion: journal.TargetVersion,
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
		SchemaVersion: 1, AttemptID: journal.AttemptID, State: "rolled_back", TargetVersion: journal.TargetVersion,
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
		SchemaVersion: 1, AttemptID: journal.AttemptID, State: "recovery_required", TargetVersion: journal.TargetVersion,
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
	if err := verifyAssetPath(target, manifest); err != nil {
		return "", err
	}
	if err := syncDirectory(temporarySlot); err != nil {
		return "", err
	}
	return temporarySlot, nil
}

func (updater *Updater) selectSlot(slot string) error {
	if _, err := ParseVersion(slot); err != nil {
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

func (updater *Updater) currentSlot() (string, error) {
	target, err := os.Readlink(updater.Paths.CurrentLink)
	if err != nil {
		return "", fmt.Errorf("read selected manager slot: %w", err)
	}
	if filepath.IsAbs(target) || filepath.Clean(target) != target || filepath.Dir(target) != "versions" {
		return "", errors.New("selected manager slot link is invalid")
	}
	slot := filepath.Base(target)
	if _, err := ParseVersion(slot); err != nil {
		return "", errors.New("selected manager slot is not a stable release")
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

func copyRegularFile(source, destination string, mode os.FileMode) error {
	descriptor, err := unix.Open(source, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(descriptor), source)
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(descriptor, &before); err != nil {
		return err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 {
		return errors.New("staged update input is not one regular file")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".copy-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, file); err != nil {
		temporary.Close()
		return err
	}
	var after unix.Stat_t
	if err := unix.Fstat(descriptor, &after); err != nil {
		temporary.Close()
		return err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim {
		temporary.Close()
		return errors.New("staged update input changed while it was copied")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func acquireUpdateLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another manager update transaction is running")
	}
	return file, nil
}
