package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	JournalPrepared         = "prepared"
	JournalSwitched         = "switched"
	JournalTargetHealthy    = "target_healthy"
	JournalRollbackSwitched = "rollback_switched"
	JournalRolledBack       = "rolled_back"
	JournalRecoveryRequired = "recovery_required"
)

var attemptIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

type ApplyRequest struct {
	SchemaVersion  int    `json:"schema_version"`
	AttemptID      string `json:"attempt_id"`
	RunningVersion string `json:"running_version"`
	TargetVersion  string `json:"target_version"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

type Journal struct {
	SchemaVersion   int    `json:"schema_version"`
	AttemptID       string `json:"attempt_id"`
	State           string `json:"state"`
	TargetVersion   string `json:"target_version"`
	PreviousSlot    string `json:"previous_slot"`
	PreviousVersion string `json:"previous_version"`
	ManifestSHA256  string `json:"manifest_sha256"`
	Failure         string `json:"failure,omitempty"`
	UpdatedAt       string `json:"updated_at"`
}

type Receipt struct {
	SchemaVersion   int    `json:"schema_version"`
	AttemptID       string `json:"attempt_id,omitempty"`
	State           string `json:"state"`
	TargetVersion   string `json:"target_version,omitempty"`
	RunningVersion  string `json:"running_version,omitempty"`
	PreviousVersion string `json:"previous_version,omitempty"`
	Failure         string `json:"failure,omitempty"`
	UpdatedAt       string `json:"updated_at"`
}

func readJSONFile(path string, limit int64, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if int64(len(payload)) > limit {
		return errors.New("JSON file exceeded its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON file must contain exactly one object")
	}
	return nil
}

func writeJSONFile(path string, value any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".json-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
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
	removeTemporary = false
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func cleanFailure(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}

func journalNow(now func() time.Time) string {
	if now == nil {
		now = time.Now
	}
	return now().UTC().Format(time.RFC3339Nano)
}

func validateApplyRequest(request ApplyRequest) error {
	if request.SchemaVersion != 1 {
		return errors.New("update request schema is unsupported")
	}
	if !attemptIDPattern.MatchString(request.AttemptID) {
		return errors.New("update request attempt id is invalid")
	}
	if _, err := ParseVersion(request.RunningVersion); err != nil {
		return errors.New("update request running version is invalid")
	}
	if _, err := ParseVersion(request.TargetVersion); err != nil {
		return errors.New("update request target version is invalid")
	}
	if !hexDigestPattern.MatchString(request.ManifestSHA256) {
		return errors.New("update request manifest digest is invalid")
	}
	return nil
}

func validateJournal(journal Journal) error {
	if journal.SchemaVersion != 1 || !attemptIDPattern.MatchString(journal.AttemptID) {
		return errors.New("updater journal identity is invalid")
	}
	if _, err := ParseVersion(journal.TargetVersion); err != nil {
		return errors.New("updater journal target version is invalid")
	}
	if _, err := ParseVersion(journal.PreviousVersion); err != nil {
		return errors.New("updater journal previous version is invalid")
	}
	// The previous slot is version-named when this updater installed it and
	// bootstrap-named when the installer did; both are places a rollback can
	// legitimately return to.
	if (journal.PreviousSlot != journal.PreviousVersion && !bootstrapSlotPattern.MatchString(journal.PreviousSlot)) || !hexDigestPattern.MatchString(journal.ManifestSHA256) {
		return errors.New("updater journal slot identity is invalid")
	}
	switch journal.State {
	case JournalPrepared, JournalSwitched, JournalTargetHealthy, JournalRollbackSwitched, JournalRolledBack, JournalRecoveryRequired:
		return nil
	default:
		return fmt.Errorf("updater journal state %q is invalid", journal.State)
	}
}
