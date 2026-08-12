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

const (
	// RequestSchemaVersion is the handoff every manager has always written.
	// RequestHelperSchemaVersion adds the helper digest and, more
	// importantly, tells the helper that the manager waiting for this
	// transaction understands a helper_state in the receipt it writes back
	// (ADR 0020, decision 6).
	RequestSchemaVersion       = 1
	RequestHelperSchemaVersion = 2
	// journalSchemaVersion is private state: only the helper reads its own
	// journal, so it advances unconditionally. A schema-1 journal found at
	// boot recovery keeps schema-1 semantics and is completed as it stands,
	// never upgraded in place mid-transaction.
	journalSchemaVersion = 2
)

// HelperState values recorded in a schema-2 receipt. A failed swap is never a
// failed update: the manager did become healthy, which is the honest outcome
// to report.
const (
	HelperStateUpdated     = "updated"
	HelperStateUnchanged   = "unchanged"
	helperStateFailPrefix  = "swap_failed:"
	helperMaxFailureReason = 200
)

func helperSwapFailed(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		reason = "unknown reason"
	}
	if len(reason) > helperMaxFailureReason {
		reason = reason[:helperMaxFailureReason]
	}
	return helperStateFailPrefix + reason
}

type ApplyRequest struct {
	SchemaVersion  int    `json:"schema_version"`
	AttemptID      string `json:"attempt_id"`
	RunningVersion string `json:"running_version"`
	TargetVersion  string `json:"target_version"`
	ManifestSHA256 string `json:"manifest_sha256"`
	HelperSHA256   string `json:"helper_sha256,omitempty"`
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
	// RequestSchema carries the requesting manager's schema across every
	// resume boundary, because the receipt written at the end of a
	// transaction has to speak the schema of the request that started it
	// even when a reboot separated the two.
	RequestSchema int    `json:"request_schema,omitempty"`
	HelperSHA256  string `json:"helper_sha256,omitempty"`
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
	// HelperState appears only in a schema-2 receipt, which is written only
	// for a schema-2 request. An older manager decodes strictly and would
	// refuse a field it does not know, and a rollback puts an older manager
	// back in front of this file.
	HelperState string `json:"helper_state,omitempty"`
}

// receiptSchema is the whole cross-version rule in one place: the helper
// writes its receipt at the schema of the request it is serving. A schema-1
// request produces a schema-1 receipt with no helper_state even when this
// helper speaks protocol 2 and did swap itself, because a rollback puts the
// requesting manager back in front of that file and it decodes strictly.
func (journal Journal) receiptSchema() int {
	if journal.RequestSchema == RequestHelperSchemaVersion {
		return RequestHelperSchemaVersion
	}
	return RequestSchemaVersion
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
	switch request.SchemaVersion {
	case RequestSchemaVersion:
		if request.HelperSHA256 != "" {
			return errors.New("update request schema 1 does not carry a helper digest")
		}
	case RequestHelperSchemaVersion:
		if !hexDigestPattern.MatchString(request.HelperSHA256) {
			return errors.New("update request helper digest is invalid")
		}
	default:
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
	if journal.SchemaVersion < 1 || journal.SchemaVersion > journalSchemaVersion || !attemptIDPattern.MatchString(journal.AttemptID) {
		return errors.New("updater journal identity is invalid")
	}
	// A schema-1 journal predates helper handling entirely, so it is read
	// with schema-1 semantics and completed as it stands. It is never
	// upgraded in place while a transaction is still open.
	if journal.SchemaVersion == 1 && (journal.RequestSchema != 0 || journal.HelperSHA256 != "") {
		return errors.New("updater journal schema 1 does not carry helper identity")
	}
	if journal.RequestSchema == RequestHelperSchemaVersion && !hexDigestPattern.MatchString(journal.HelperSHA256) {
		return errors.New("updater journal helper digest is invalid")
	}
	if journal.RequestSchema != 0 && journal.RequestSchema != RequestSchemaVersion && journal.RequestSchema != RequestHelperSchemaVersion {
		return errors.New("updater journal request schema is unsupported")
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
