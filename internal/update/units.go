package update

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Generation 2 of ADR 0020: the root updater carries the unit texts of its own
// build commit and reconciles the installed ones against them. These three
// files are byte-for-byte copies of packaging/systemd/, exactly as
// internal/setup/assets/ are, and TestEmbeddedUnitMatchesPackagedUnit enforces
// it. The manager's copy and this copy exist separately on purpose: the helper
// must not import the installer, which drags in remote runners and install
// planning that a root process with an empty capability set has no business
// linking.
//
//go:embed assets/basement.service
var managerUnitText string

//go:embed assets/basement-updater.service
var updaterServiceUnitText string

//go:embed assets/basement-updater.path
var updaterPathUnitText string

// Units values recorded in a schema-2 receipt. A failed reconcile is never a
// failed update, and not_permitted is not a failure at all: it is the honest
// name for a machine still running a generation-1 updater unit.
const (
	UnitsNotPermitted = "not_permitted"
	UnitsReconciled   = "reconciled"
	UnitsUnchanged    = "unchanged"
	unitsFailPrefix   = "failed:"
)

func unitsFailed(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		reason = "unknown reason"
	}
	if len(reason) > helperMaxFailureReason {
		reason = reason[:helperMaxFailureReason]
	}
	return unitsFailPrefix + reason
}

// unitsPermitted reports whether a recorded units value proves this machine's
// updater can write its own unit directory. Only the probe states truth, so
// only a value the probe produced answers this.
func unitsPermitted(units string) bool {
	return units == UnitsReconciled || units == UnitsUnchanged || strings.HasPrefix(units, unitsFailPrefix)
}

// unitProbeName is fixed and carries no unit suffix. Fixed, because a process
// killed between the create and the remove leaves the file behind and the next
// run has to be able to find and clear exactly it. Suffix-less, because
// systemd ignores a file in this directory that does not end in a unit type,
// so even permanent debris is inert.
const unitProbeName = "basement-updater-write-probe"

type embeddedUnit struct {
	name string
	text string
}

// embeddedUnits is the entire set this updater may write. basement.service.d
// drop-ins are absent on purpose: the listen drop-in is owner data written at
// install time, and ADR 0020 leaves it, the key ring and every other service
// outside what an update may touch.
func embeddedUnits() []embeddedUnit {
	return []embeddedUnit{
		{name: "basement.service", text: managerUnitText},
		{name: "basement-updater.service", text: updaterServiceUnitText},
		{name: "basement-updater.path", text: updaterPathUnitText},
	}
}

// UnitReloader asks systemd to re-read unit files from disk. It is separate
// from ServiceController because it names no service: reconciliation touches
// three units and settles them with one reload.
type UnitReloader interface {
	DaemonReload(context.Context) error
}

// reconcileUnits brings the installed unit files back to the texts this
// binary was built with, and returns the units value to record. It is called
// only from the post-target_healthy window, and only for a transaction this
// process carried from the request through to a healthy manager.
//
// The texts written are the running binary's, not the freshly swapped one's:
// a rename over a live executable leaves this process on its old inode by
// design, so a release that changes both the helper and its units reconciles
// them on the following run. That is the honest ordering and it is why the
// swap happens first: the generation-1 value must not depend on a unit write
// that a generation-1 sandbox refuses anyway.
//
// Safety. A bad unit written here plus a daemon-reload can break the very next
// updater run, which is the one repair path an update has. Three things bound
// that: only compiled-in texts are ever written, so nothing a manifest or a
// request names can reach this; every replaced file leaves a .previous sibling
// for a manual repair over console or SSH; and the installer still overwrites
// all three unconditionally and never deletes a .previous.
func (updater *Updater) reconcileUnits(ctx context.Context, journal Journal, mayReconcile bool) string {
	if journal.receiptSchema() != RequestHelperSchemaVersion {
		// Same rule as helper_state: a schema-1 request gets a schema-1
		// receipt, and the manager waiting on it decodes strictly.
		return ""
	}
	if !mayReconcile {
		return unitsFailed("a recovered transaction does not reconcile units")
	}
	directory := updater.Paths.UnitDir
	if directory == "" {
		return UnitsNotPermitted
	}
	// Reading ReadWritePaths out of the unit text would state intent. Only
	// the probe states truth, because a drop-in can widen or narrow the
	// effective sandbox without changing the unit this binary embeds.
	if err := probeUnitWrite(directory); err != nil {
		return UnitsNotPermitted
	}
	changed := 0
	failure := ""
	for _, unit := range embeddedUnits() {
		installed := filepath.Join(directory, unit.name)
		live, err := os.ReadFile(installed)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			if failure == "" {
				failure = cleanFailure(err)
			}
			continue
		}
		if err == nil && string(live) == unit.text {
			continue
		}
		if err == nil {
			// The recovery copy is written before the replacement, so a
			// machine is never left with neither the old text nor a complete
			// new one. Its name ends in .previous, which is not a unit
			// suffix, so systemd never loads it.
			if backupErr := copySyncedFile(installed, installed+".previous", 0o644); backupErr != nil {
				if failure == "" {
					failure = cleanFailure(backupErr)
				}
				continue
			}
		}
		// writeBytesAtomic chmods explicitly. UMask=0077 in this unit would
		// otherwise leave a unit file at 0600, which systemd reads as root
		// but nothing else can audit; the same umask already cost a hardware
		// day on the manager slot.
		if writeErr := writeBytesAtomic(installed, []byte(unit.text), 0o644); writeErr != nil {
			if failure == "" {
				failure = cleanFailure(writeErr)
			}
			continue
		}
		changed++
	}
	// One reload for the whole set, and none at all when nothing moved. A
	// reload that fails after a good write leaves new text on disk and old
	// text in systemd's memory until the next reload or boot; that is
	// recorded here, never treated as a rollback trigger.
	if changed > 0 {
		if err := updater.daemonReload(ctx); err != nil && failure == "" {
			failure = cleanFailure(err)
		}
	}
	if failure != "" {
		return unitsFailed(failure)
	}
	if changed == 0 {
		return UnitsUnchanged
	}
	return UnitsReconciled
}

func (updater *Updater) daemonReload(ctx context.Context) error {
	if updater.Reloader == nil {
		return errors.New("this updater has no way to reload systemd units")
	}
	return updater.Reloader.DaemonReload(ctx)
}

// probeUnitWrite answers the only question that matters before writing a unit:
// can this process, inside its own sandbox, create a file here right now. It
// clears the debris of an interrupted earlier probe first, then creates and
// immediately removes the fixed-name file.
func probeUnitWrite(directory string) error {
	probe := filepath.Join(directory, unitProbeName)
	if err := os.Remove(probe); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(probe)
		return err
	}
	return os.Remove(probe)
}
