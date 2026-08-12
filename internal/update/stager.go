package update

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type AttemptStatus struct {
	SchemaVersion  int    `json:"schema_version"`
	AttemptID      string `json:"attempt_id"`
	State          string `json:"state"`
	RunningVersion string `json:"running_version"`
	TargetVersion  string `json:"target_version"`
	Failure        string `json:"failure,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

type Stager struct {
	DataDir            string
	Keys               KeyRing
	Client             *http.Client
	RootStatusPath     string
	UpdaterBinaryPath  string
	UpdaterUnitPath    string
	UpdaterPathUnit    string
	UpdaterServiceLink string
	UpdaterPathLink    string
	BootstrapCheck     func() error
	Now                func() time.Time
}

func NewStager(dataDir string, keys KeyRing) *Stager {
	return &Stager{
		DataDir: dataDir, Keys: keys, Client: NewHTTPReleaseSource().Client,
		RootStatusPath:     "/var/lib/basement-updater/status.json",
		UpdaterBinaryPath:  "/usr/lib/basement/updater/basement-updater",
		UpdaterUnitPath:    "/etc/systemd/system/basement-updater.service",
		UpdaterPathUnit:    "/etc/systemd/system/basement-updater.path",
		UpdaterServiceLink: "/etc/systemd/system/multi-user.target.wants/basement-updater.service",
		UpdaterPathLink:    "/etc/systemd/system/multi-user.target.wants/basement-updater.path",
		Now:                time.Now,
	}
}

func (stager *Stager) Stage(ctx context.Context, candidate Candidate, runningVersion string) (AttemptStatus, error) {
	status, err := stager.Prepare(candidate, runningVersion)
	if err != nil {
		return AttemptStatus{}, err
	}
	return stager.StagePrepared(ctx, candidate, status)
}

// Prepare validates the server-selected candidate and persists the update
// intent before the API starts asynchronous network work.
func (stager *Stager) Prepare(candidate Candidate, runningVersion string) (AttemptStatus, error) {
	if stager == nil {
		return AttemptStatus{}, errors.New("manager update stager is unavailable")
	}
	if err := stager.checkBootstrap(); err != nil {
		return AttemptStatus{}, err
	}
	manifest, err := VerifySignedManifest(candidate.ManifestBytes, candidate.Signature, stager.Keys)
	if err != nil {
		return AttemptStatus{}, err
	}
	if err := ValidateCandidate(manifest, candidate.Release.TagName, runningVersion); err != nil {
		return AttemptStatus{}, err
	}
	attemptID, err := newAttemptID()
	if err != nil {
		return AttemptStatus{}, err
	}
	status := AttemptStatus{
		SchemaVersion: 1, AttemptID: attemptID, State: "checking_signature",
		RunningVersion: runningVersion, TargetVersion: manifest.ReleaseVersion, UpdatedAt: journalNow(stager.Now),
	}
	if err := stager.writeStatus(status); err != nil {
		return AttemptStatus{}, err
	}
	return status, nil
}

// StagePrepared preserves the local one-click behavior by composing the two
// fleet-safe phases. Keeping this wrapper means a standalone installation
// still downloads, verifies, and hands off exactly as it did before rolling
// upgrades needed a barrier between verification and restart.
func (stager *Stager) StagePrepared(ctx context.Context, candidate Candidate, status AttemptStatus) (AttemptStatus, error) {
	staged, err := stager.StageOnly(ctx, candidate, status)
	if err != nil {
		return staged, err
	}
	return stager.ApplyStaged(staged)
}

// StageOnly downloads and independently verifies a release without creating
// the root updater request. A fleet must prove every node has accepted the
// signed release before the first manager restart, so handoff is deliberately
// a separate durable action.
func (stager *Stager) StageOnly(ctx context.Context, candidate Candidate, status AttemptStatus) (AttemptStatus, error) {
	if stager == nil || status.SchemaVersion != 1 || !attemptIDPattern.MatchString(status.AttemptID) {
		return AttemptStatus{}, errors.New("manager update attempt is invalid")
	}
	manifest, err := VerifySignedManifest(candidate.ManifestBytes, candidate.Signature, stager.Keys)
	if err != nil {
		return AttemptStatus{}, err
	}
	if status.TargetVersion != manifest.ReleaseVersion || status.RunningVersion == "" {
		return AttemptStatus{}, errors.New("manager update attempt does not match the verified release")
	}
	if err := ValidateCandidate(manifest, candidate.Release.TagName, status.RunningVersion); err != nil {
		return AttemptStatus{}, err
	}
	fail := func(cause error) (AttemptStatus, error) {
		status.State = "failed_before_handoff"
		status.Failure = cleanFailure(cause)
		status.UpdatedAt = journalNow(stager.Now)
		_ = stager.writeStatus(status)
		return status, cause
	}
	stagingRoot := filepath.Join(stager.DataDir, "updates", "staging")
	partialDir := filepath.Join(stagingRoot, "partial")
	preparedDir := filepath.Join(stagingRoot, "prepared", status.AttemptID)
	if err := os.MkdirAll(partialDir, 0o750); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(preparedDir, 0o750); err != nil {
		return fail(err)
	}
	preparedAsset := filepath.Join(preparedDir, managerFileName)
	if status.State == "staged" {
		if err := verifyAssetPath(preparedAsset, manifest); err == nil {
			return status, nil
		}
	}
	status.State = "downloading"
	status.Failure = ""
	status.UpdatedAt = journalNow(stager.Now)
	if err := stager.writeStatus(status); err != nil {
		return fail(err)
	}
	partialAsset := filepath.Join(partialDir, status.AttemptID+".basement")
	// A process loss can leave an untrusted partial file behind. Starting the
	// same durable attempt again must redownload it rather than treating those
	// bytes as a resumable verification result.
	if err := os.Remove(partialAsset); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if err := stager.download(ctx, candidate.AssetURL, partialAsset, manifest.AssetSize); err != nil {
		return fail(err)
	}
	defer os.Remove(partialAsset)
	status.State = "verifying"
	status.UpdatedAt = journalNow(stager.Now)
	if err := stager.writeStatus(status); err != nil {
		return fail(err)
	}
	if err := verifyAssetPath(partialAsset, manifest); err != nil {
		return fail(err)
	}
	for name, payload := range map[string][]byte{
		ManifestAssetName:  candidate.ManifestBytes,
		SignatureAssetName: candidate.Signature,
	} {
		if err := writeBytesAtomic(filepath.Join(preparedDir, name), payload, 0o600); err != nil {
			return fail(err)
		}
	}
	if err := renameFileAtomic(partialAsset, preparedAsset, 0o700); err != nil {
		return fail(err)
	}
	if err := verifyAssetPath(preparedAsset, manifest); err != nil {
		return fail(err)
	}
	status.State = "staged"
	status.UpdatedAt = journalNow(stager.Now)
	if err := stager.writeStatus(status); err != nil {
		return status, err
	}
	return status, nil
}

// ApplyStaged reopens and re-verifies every staged byte before it creates the
// fixed request marker. The root updater still repeats the same verification
// on its own copy and remains the security boundary.
func (stager *Stager) ApplyStaged(status AttemptStatus) (AttemptStatus, error) {
	if stager == nil || status.SchemaVersion != 1 || !attemptIDPattern.MatchString(status.AttemptID) || status.State != "staged" {
		return AttemptStatus{}, errors.New("manager update is not durably staged")
	}
	fail := func(cause error) (AttemptStatus, error) {
		status.State = "failed_before_handoff"
		status.Failure = cleanFailure(cause)
		status.UpdatedAt = journalNow(stager.Now)
		_ = stager.writeStatus(status)
		return status, cause
	}
	stagingRoot := filepath.Join(stager.DataDir, "updates", "staging")
	preparedDir := filepath.Join(stagingRoot, "prepared", status.AttemptID)
	manifestBytes, err := os.ReadFile(filepath.Join(preparedDir, ManifestAssetName))
	if err != nil {
		return fail(err)
	}
	signature, err := os.ReadFile(filepath.Join(preparedDir, SignatureAssetName))
	if err != nil {
		return fail(err)
	}
	manifest, err := VerifySignedManifest(manifestBytes, signature, stager.Keys)
	if err != nil {
		return fail(err)
	}
	if manifest.ReleaseVersion != status.TargetVersion || status.RunningVersion == "" {
		return fail(errors.New("manager update attempt does not match the staged release"))
	}
	if err := ValidateCandidate(manifest, manifest.ReleaseVersion, status.RunningVersion); err != nil {
		return fail(err)
	}
	preparedAsset := filepath.Join(preparedDir, managerFileName)
	if err := verifyAssetPath(preparedAsset, manifest); err != nil {
		return fail(err)
	}
	// The root updater runs with an empty capability set, so being root
	// grants it nothing here: it reaches this handoff purely through its
	// membership in the manager's group (SupplementaryGroups in its unit).
	// The pending directory needs group write for it to quarantine and
	// clean up, and every handoff file needs its group-read bit. Hardware
	// proved the mismatch (2026-08-12): files staged 0600 left the updater
	// permission denied, no receipt was ever written, and the console
	// showed waiting for the root updater forever. The explicit Chmod
	// repairs a pending directory an older manager created too narrow.
	pendingDir := filepath.Join(stagingRoot, "pending")
	if err := os.MkdirAll(pendingDir, 0o770); err != nil {
		return fail(err)
	}
	if err := os.Chmod(pendingDir, 0o770); err != nil {
		return fail(err)
	}
	requestPath := filepath.Join(pendingDir, requestFileName)
	if _, err := os.Stat(requestPath); err == nil {
		return fail(errors.New("another manager update is waiting for the root updater"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	for name, payload := range map[string][]byte{
		ManifestAssetName: manifestBytes, SignatureAssetName: signature,
	} {
		if err := writeBytesAtomic(filepath.Join(pendingDir, name), payload, 0o640); err != nil {
			return fail(err)
		}
	}
	if err := copyFileAtomic(preparedAsset, filepath.Join(pendingDir, managerFileName), 0o750); err != nil {
		return fail(err)
	}
	if err := verifyAssetPath(filepath.Join(pendingDir, managerFileName), manifest); err != nil {
		return fail(err)
	}
	request := ApplyRequest{
		SchemaVersion: 1, AttemptID: status.AttemptID, RunningVersion: status.RunningVersion,
		TargetVersion: manifest.ReleaseVersion, ManifestSHA256: ManifestDigest(manifestBytes),
	}
	if err := writeJSONFile(requestPath, request, 0o640); err != nil {
		return fail(err)
	}
	status.State = "waiting_for_root"
	status.UpdatedAt = journalNow(stager.Now)
	if err := stager.writeStatus(status); err != nil {
		return status, err
	}
	return status, nil
}

// ReconcileStartup fails over a staging attempt that a previous manager
// process abandoned before handing it to the root updater. The manager-owned
// states can only advance while the process that wrote them is alive, so at
// startup any of them left behind with no pending root request and no root
// receipt for the same attempt would otherwise read as an update in progress
// forever, refusing every install, generation and further update.
func (stager *Stager) ReconcileStartup() error {
	if err := stager.settleManagerOwned(
		map[string]bool{"checking_signature": true, "downloading": true, "verifying": true},
		"the manager restarted before this update reached the root updater; start the update again",
	); err != nil {
		return err
	}
	// waiting_for_root belongs to the root updater only while its handoff
	// file or a receipt naming this attempt exists. A live apply always has
	// one of the two: the request until the updater writes its restarting
	// receipt, that receipt afterwards. Neither existing means the updater
	// consumed the request without settling this attempt, which is exactly
	// what a quarantine that could not read the request looks like, and the
	// console would otherwise report an update in progress forever (found
	// on hardware, 2026-08-12).
	return stager.settleManagerOwned(
		map[string]bool{"waiting_for_root": true},
		"the root updater did not settle this update; check its receipt, then start the update again",
	)
}

// SettleResolved settles a manager-owned staging attempt when the owner
// resolves a stopped fleet upgrade. Unlike startup reconciliation this also
// covers a fully staged release that will never be applied, because the run it
// belonged to is over. States owned by the root updater are left alone: the
// root process settles those itself through its receipt.
func (stager *Stager) SettleResolved(failure string) error {
	return stager.settleManagerOwned(
		map[string]bool{"checking_signature": true, "downloading": true, "verifying": true, "staged": true},
		failure,
	)
}

func (stager *Stager) settleManagerOwned(states map[string]bool, failure string) error {
	if stager == nil {
		return nil
	}
	var status AttemptStatus
	if err := readJSONFile(stager.statusPath(), 64<<10, &status); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !states[status.State] {
		return nil
	}
	requestPath := filepath.Join(stager.DataDir, "updates", "staging", "pending", requestFileName)
	var request ApplyRequest
	requestErr := readJSONFile(requestPath, 64<<10, &request)
	if requestErr == nil && request.AttemptID == status.AttemptID {
		// The handoff file exists, so the root updater owns this attempt
		// and will write the receipt that settles it.
		return nil
	}
	if requestErr != nil && !errors.Is(requestErr, os.ErrNotExist) {
		return requestErr
	}
	var receipt Receipt
	receiptErr := readJSONFile(stager.RootStatusPath, 64<<10, &receipt)
	if receiptErr == nil && receipt.AttemptID == status.AttemptID {
		// Status() already prefers the root receipt for this attempt.
		return nil
	}
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return receiptErr
	}
	status.State = "failed_before_handoff"
	status.Failure = failure
	status.UpdatedAt = journalNow(stager.Now)
	return stager.writeStatus(status)
}

func (stager *Stager) Status() (AttemptStatus, bool, error) {
	var manager AttemptStatus
	managerErr := readJSONFile(stager.statusPath(), 64<<10, &manager)
	if managerErr != nil && !errors.Is(managerErr, os.ErrNotExist) {
		return AttemptStatus{}, false, managerErr
	}
	var root Receipt
	rootErr := readJSONFile(stager.RootStatusPath, 64<<10, &root)
	if rootErr == nil && root.AttemptID != "" && (managerErr != nil || root.AttemptID == manager.AttemptID) {
		return AttemptStatus{
			SchemaVersion: 1, AttemptID: root.AttemptID, State: root.State,
			RunningVersion: root.RunningVersion, TargetVersion: root.TargetVersion,
			Failure: root.Failure, UpdatedAt: root.UpdatedAt,
		}, true, nil
	}
	if rootErr != nil && !errors.Is(rootErr, os.ErrNotExist) {
		return AttemptStatus{}, false, rootErr
	}
	if managerErr != nil {
		return AttemptStatus{}, false, nil
	}
	return manager, true, nil
}

func (stager *Stager) BootstrapReady() error {
	return stager.checkBootstrap()
}

func (stager *Stager) checkBootstrap() error {
	if stager.BootstrapCheck != nil {
		return stager.BootstrapCheck()
	}
	if filepath.Clean(stager.DataDir) != "/var/lib/basement" {
		return errors.New("run the installer once to enable console updates for this data directory")
	}
	for _, path := range []string{
		stager.UpdaterBinaryPath, stager.UpdaterUnitPath, stager.UpdaterPathUnit,
		stager.UpdaterServiceLink, stager.UpdaterPathLink,
	} {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return errors.New("run the installer once to enable console updates")
		}
	}
	return nil
}

func (stager *Stager) download(ctx context.Context, location, destination string, expectedSize int64) error {
	parsed, err := url.Parse(location)
	if err != nil || parsed.Scheme != "https" || !releaseHostAllowed(parsed.Hostname()) {
		return errors.New("manager update asset URL is not allowed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "basement-manager-update")
	response, err := stager.client().Do(request)
	if err != nil {
		return fmt.Errorf("download manager update: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download manager update: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > expectedSize {
		return errors.New("manager update download is larger than its signed size")
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(destination)
		}
	}()
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, expectedSize+1))
	if copyErr == nil && written != expectedSize {
		copyErr = errors.New("manager update download size does not match its signed manifest")
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	remove = false
	return syncDirectory(filepath.Dir(destination))
}

func (stager *Stager) client() *http.Client {
	if stager.Client != nil {
		return stager.Client
	}
	return NewHTTPReleaseSource().Client
}

func (stager *Stager) writeStatus(status AttemptStatus) error {
	return writeJSONFile(stager.statusPath(), status, 0o600)
}

func (stager *Stager) statusPath() string {
	return filepath.Join(stager.DataDir, "updates", "status.json")
}

func newAttemptID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	const alphabet = "0123456789abcdef"
	encoded := make([]byte, len(random)*2)
	for index, value := range random {
		encoded[index*2] = alphabet[value>>4]
		encoded[index*2+1] = alphabet[value&15]
	}
	return "update-" + string(encoded), nil
}

func writeBytesAtomic(path string, payload []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".stage-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func renameFileAtomic(source, destination string, mode os.FileMode) error {
	if err := os.Chmod(source, mode); err != nil {
		return err
	}
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func copyFileAtomic(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".asset-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
		temporary.Close()
		return err
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
