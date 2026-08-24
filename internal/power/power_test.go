package power

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/store"
)

// fakeSMI is the nvidia-smi that never was. It records every call so a test
// can say exactly what the GPU would have been asked for, and it can be made
// to fail the way a machine without a driver fails.
type fakeSMI struct {
	mu    sync.Mutex
	calls [][]string
	err   error
}

func (f *fakeSMI) run(_ context.Context, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	return f.err
}

func (f *fakeSMI) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

func newTestController(t *testing.T) (*Controller, *store.Store, *fakeSMI) {
	t.Helper()
	// Every controller test that is not about the privilege path runs as root,
	// so the recorded command line is the tool and its own arguments. The two
	// tests that are about it say so.
	asRoot(t)
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	smi := &fakeSMI{}
	return NewController(database, smi.run), database, smi
}

// asRoot and asService put this process on one side of the root boundary for
// the length of one test. Only these two touch processEUID.
func asRoot(t *testing.T) { setEUID(t, 0) }

func asService(t *testing.T) { setEUID(t, 1000) }

func setEUID(t *testing.T, euid int) {
	t.Helper()
	restore := processEUID
	t.Cleanup(func() { processEUID = restore })
	processEUID = func() int { return euid }
}

// The two modes are two exact commands and nothing else.
func TestEachModeRunsItsOwnCommand(t *testing.T) {
	ctx := context.Background()
	controller, _, smi := newTestController(t)

	cool, err := controller.SetPowerMode(ctx, store.PowerModeCool)
	if err != nil {
		t.Fatal(err)
	}
	if cool.Mode != store.PowerModeCool || cool.Failure != "" {
		t.Fatalf("cool mode reads %+v", cool)
	}
	full, err := controller.SetPowerMode(ctx, store.PowerModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if full.Mode != store.PowerModeFull || full.Failure != "" {
		t.Fatalf("full mode reads %+v", full)
	}
	calls := smi.recorded()
	if len(calls) != 2 {
		t.Fatalf("the GPU was asked %d times: %v", len(calls), calls)
	}
	if strings.Join(calls[0], " ") != "nvidia-smi -lgc 0,2200" {
		t.Fatalf("cool ran %v", calls[0])
	}
	if strings.Join(calls[1], " ") != "nvidia-smi -rgc" {
		t.Fatalf("full ran %v", calls[1])
	}
	if _, err := controller.SetPowerMode(ctx, "turbo"); !errors.Is(err, store.ErrPowerMode) {
		t.Fatalf("an unknown mode was accepted: %v", err)
	}
	if len(smi.recorded()) != 2 {
		t.Fatal("a refused mode still ran a command")
	}
}

// The manager is not root on a Spark, and the driver will not take a clock
// change from anyone else, so the command goes through sudo. Both shapes are
// pinned here: the one a Spark runs, and the one a root manager runs.
func TestTheCommandGoesThroughSudoWhenTheManagerIsNotRoot(t *testing.T) {
	ctx := context.Background()
	controller, _, smi := newTestController(t)

	if _, err := controller.SetPowerMode(ctx, store.PowerModeCool); err != nil {
		t.Fatal(err)
	}
	asService(t)
	if _, err := controller.SetPowerMode(ctx, store.PowerModeCool); err != nil {
		t.Fatal(err)
	}

	calls := smi.recorded()
	if len(calls) != 2 {
		t.Fatalf("the GPU was asked %d times: %v", len(calls), calls)
	}
	if strings.Join(calls[0], " ") != "nvidia-smi -lgc 0,2200" {
		t.Fatalf("a root manager ran %v", calls[0])
	}
	if strings.Join(calls[1], " ") != "/usr/bin/sudo -n nvidia-smi -lgc 0,2200" {
		t.Fatalf("a service manager ran %v", calls[1])
	}
	// Full speed takes the same road.
	if _, err := controller.SetPowerMode(ctx, store.PowerModeFull); err != nil {
		t.Fatal(err)
	}
	if last := smi.recorded()[2]; strings.Join(last, " ") != "/usr/bin/sudo -n nvidia-smi -rgc" {
		t.Fatalf("a service manager releasing the cap ran %v", last)
	}
}

// The grant on the machine and the arguments in this file are one fact kept in
// two places, read by two different programs. This is what holds them together:
// a command line the applier can run that the grant does not name is a switch
// that fails on every Spark.
func TestTheShippedGrantNamesTheTwoCommandsAndNoOther(t *testing.T) {
	grant, err := os.ReadFile("../../packaging/sudoers/basement-power")
	if err != nil {
		t.Fatal(err)
	}
	text := string(grant)
	for _, mode := range []string{store.PowerModeCool, store.PowerModeFull} {
		arguments, err := Arguments(mode)
		if err != nil {
			t.Fatal(err)
		}
		// sudoers separates its own list items with commas, so a comma inside
		// one command line is escaped there and only there.
		granted := strings.ReplaceAll("/usr/bin/"+nvidiaSMI+" "+strings.Join(arguments, " "), ",", `\,`)
		if !strings.Contains(text, granted) {
			t.Fatalf("the grant does not name %q:\n%s", granted, text)
		}
	}
	if count := strings.Count(text, nvidiaSMI); count != 2 {
		t.Fatalf("the grant names %d commands, want exactly the two above:\n%s", count, text)
	}
	if !strings.HasPrefix(text, "basement ALL=(root) NOPASSWD: ") {
		t.Fatalf("the grant is not a passwordless root grant to the service account:\n%s", text)
	}
}

// The grant is only half of the privilege path, and the other half lives in the
// unit. Hardware proved both halves: with NoNewPrivileges=yes the kernel
// ignores the setuid bit on sudo and the Spark answered "The no new privileges
// flag is set, which prevents sudo from running as root"; with these two lines
// the same two commands ran and the clock moved.
//
// This is pinned here, next to the grant, because a later pass that hardens the
// unit would otherwise turn the power switch off on every Spark and nothing
// would say so until a person moved the switch on hardware.
func TestTheManagerUnitAllowsTheOneWayThisPackageReachesRoot(t *testing.T) {
	unit, err := os.ReadFile("../../packaging/systemd/basement.service")
	if err != nil {
		t.Fatal(err)
	}
	text := string(unit)
	for _, required := range []string{
		"NoNewPrivileges=no",
		"CapabilityBoundingSet=CAP_SETUID CAP_SETGID CAP_SYS_ADMIN CAP_AUDIT_WRITE",
		// The manager process itself must still gain nothing of its own. Only
		// the child sudo starts may hold anything in the set above.
		"AmbientCapabilities=\n",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("the manager unit no longer carries %q, so the power switch cannot reach root", required)
		}
	}
}

// The bound is a bound on the call, not on the process. A driver that exits
// and leaves a child holding its output used to make the ten second deadline
// last sixty, and that wait holds the lock every later change queues on. This
// is the real command, with the real timeout mechanism, against exactly that
// shape.
func TestTheCommandBoundHoldsWhenAChildHoldsTheOutput(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("this test needs a POSIX shell to stand in for nvidia-smi")
	}
	directory := t.TempDir()
	// nvidia-smi exits at once and leaves a child with the pipes still open,
	// so reading its output blocks on the child rather than on the command.
	script := "#!/bin/sh\nsleep 5 &\nexit 0\n"
	if err := os.WriteFile(filepath.Join(directory, "nvidia-smi"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	restoreTimeout, restoreGrace := commandTimeout, pipeGrace
	t.Cleanup(func() { commandTimeout, pipeGrace = restoreTimeout, restoreGrace })
	commandTimeout, pipeGrace = 300*time.Millisecond, 200*time.Millisecond

	started := time.Now()
	err := Command(context.Background(), "nvidia-smi", "-rgc")
	elapsed := time.Since(started)

	// Generous next to the bound and far below the five seconds the child
	// holds, so this cannot pass by accident on a slow machine.
	if elapsed > 2*time.Second {
		t.Fatalf("the command took %s, so the bound did not hold", elapsed)
	}
	if err == nil {
		t.Fatal("a command that never delivered its output reported success")
	}
	if sentence := failureSentence(err); sentence != timeoutSentence {
		t.Fatalf("a held pipe reads %q", sentence)
	}
}

// "It failed" is the wrong word for a machine that would not let this manager
// try. Both refusals are here, the driver's and sudo's, against the real
// Command, and so is the failure that is still only a failure.
func TestARefusedClockChangeReadsAsAPermissionFailure(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("this test needs a POSIX shell to stand in for nvidia-smi and sudo")
	}
	directory := t.TempDir()
	writeScript := func(name, body string) string {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	restoreSudo := sudoPath
	t.Cleanup(func() { sudoPath = restoreSudo })
	sudoPath = writeScript("sudo", "#!/bin/sh\nshift\nexec \"$@\"\n")

	// The driver's own refusal, in the words and with the exit code a GB10 gave
	// from the unprivileged unit.
	writeScript("nvidia-smi", "#!/bin/sh\necho 'The current user does not have permission to change clocks' >&2\nexit 4\n")
	asRoot(t)
	line, err := CommandLine(store.PowerModeCool)
	if err != nil {
		t.Fatal(err)
	}
	failure := Command(context.Background(), line...)
	if sentence := failureSentence(failure); sentence != permissionSentence {
		t.Fatalf("the driver's refusal reads %q", sentence)
	}
	if failure == nil || !strings.Contains(failure.Error(), "permission to change clocks") {
		t.Fatalf("the driver's own words did not reach the log: %v", failure)
	}

	// sudo's own refusal, which happens when the grant is missing. The tool
	// would have worked, so only sudo can be the reason.
	writeScript("nvidia-smi", "#!/bin/sh\nexit 0\n")
	sudoPath = writeScript("sudo", "#!/bin/sh\necho 'sudo: a password is required' >&2\nexit 1\n")
	asService(t)
	line, err = CommandLine(store.PowerModeCool)
	if err != nil {
		t.Fatal(err)
	}
	if sentence := failureSentence(Command(context.Background(), line...)); sentence != permissionSentence {
		t.Fatalf("a missing grant reads %q", sentence)
	}

	// A machine with the grant runs the command and reports nothing.
	sudoPath = writeScript("sudo", "#!/bin/sh\nshift\nexec \"$@\"\n")
	if err := Command(context.Background(), line...); err != nil {
		t.Fatalf("a granted command through sudo failed: %v", err)
	}

	// And a failure that is only a failure keeps the sentence it always had.
	writeScript("nvidia-smi", "#!/bin/sh\necho 'Setting locked clocks is not supported for GPU 00000000:01:00.0' >&2\nexit 3\n")
	if sentence := failureSentence(Command(context.Background(), line...)); sentence != commandSentence {
		t.Fatalf("an unsupported operation reads %q", sentence)
	}
}

// A machine with no working nvidia-smi is a machine at full speed. It keeps
// the mode its owner chose, it says in one sentence that the machine has not
// taken it, and nothing about it is an error the caller has to handle.
func TestAFailingCommandFailsOpenAndKeepsTheChosenMode(t *testing.T) {
	ctx := context.Background()
	controller, database, smi := newTestController(t)
	smi.err = errNoTool

	state, err := controller.SetPowerMode(ctx, store.PowerModeCool)
	if err != nil {
		t.Fatalf("a machine without nvidia-smi refused the request: %v", err)
	}
	if state.Mode != store.PowerModeCool {
		t.Fatalf("the chosen mode was lost: %+v", state)
	}
	if state.Failure != missingToolSentence {
		t.Fatalf("the failure sentence reads %q", state.Failure)
	}
	stored, err := database.PowerMode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Mode != store.PowerModeCool || stored.Failure != missingToolSentence {
		t.Fatalf("the store holds %+v", stored)
	}

	// Any other refusal is still one plain sentence, never a driver message.
	smi.err = errors.New("Setting applications clocks is not supported for GPU 00000000:01:00.0")
	other, err := controller.SetPowerMode(ctx, store.PowerModeCool)
	if err != nil {
		t.Fatal(err)
	}
	if other.Failure != commandSentence {
		t.Fatalf("a driver refusal reads %q", other.Failure)
	}

	// A machine that refuses the change has its own sentence, and it is the one
	// an owner can act on.
	smi.err = errNoPermission
	refused, err := controller.SetPowerMode(ctx, store.PowerModeCool)
	if err != nil {
		t.Fatalf("a machine that refused the change reported an error: %v", err)
	}
	if refused.Mode != store.PowerModeCool {
		t.Fatalf("a refused change lost the chosen mode: %+v", refused)
	}
	if refused.Failure != permissionSentence {
		t.Fatalf("a refused change reads %q", refused.Failure)
	}

	// And the machine heals: the next attempt that works clears the sentence.
	smi.err = nil
	healed, err := controller.SetPowerMode(ctx, store.PowerModeCool)
	if err != nil {
		t.Fatal(err)
	}
	if healed.Failure != "" {
		t.Fatalf("a working command left %q behind", healed.Failure)
	}
}

// Startup puts a chosen mode back, once, because the driver forgot it at the
// reboot.
func TestStartupAppliesTheStoredModeOnce(t *testing.T) {
	ctx := context.Background()
	controller, database, smi := newTestController(t)
	if _, err := database.SetPowerMode(ctx, store.PowerModeCool); err != nil {
		t.Fatal(err)
	}

	state, err := controller.ApplyStored(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != store.PowerModeCool || state.Failure != "" {
		t.Fatalf("startup left %+v", state)
	}
	calls := smi.recorded()
	if len(calls) != 1 || strings.Join(calls[0], " ") != "nvidia-smi -lgc 0,2200" {
		t.Fatalf("startup ran %v, want one cool command", calls)
	}
}

// A machine that has never met this feature must look exactly as it did before
// the feature existed: no command at startup, and above all no failure badge on
// a console that has never been asked for anything.
func TestStartupLeavesASettingNobodyChoseAlone(t *testing.T) {
	ctx := context.Background()
	controller, _, smi := newTestController(t)
	smi.err = errNoTool

	state, err := controller.ApplyStored(ctx)
	if err != nil {
		t.Fatalf("startup failed on a machine with no GPU: %v", err)
	}
	if state.Mode != store.PowerModeFull {
		t.Fatalf("a fresh machine reads %+v, want full speed", state)
	}
	if state.Failure != "" {
		t.Fatalf("a machine nobody has configured reports the failure %q", state.Failure)
	}
	if calls := smi.recorded(); len(calls) != 0 {
		t.Fatalf("a setting nobody chose ran %v", calls)
	}
}

// Full speed with no nvidia-smi is not a failure. It is the mode, in force, on
// a machine that could never have left it. Only the cap can fail on such a
// machine, and it says so.
func TestFullSpeedWithoutTheToolIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	controller, _, smi := newTestController(t)
	smi.err = errNoTool

	chosen, err := controller.SetPowerMode(ctx, store.PowerModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if chosen.Mode != store.PowerModeFull || chosen.Failure != "" {
		t.Fatalf("choosing full speed on a machine with no tool reads %+v", chosen)
	}
	// The same machine at startup, now that the setting has been chosen: still
	// nothing to report.
	restarted, err := controller.ApplyStored(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Failure != "" {
		t.Fatalf("a restart reported %q about full speed", restarted.Failure)
	}
	if calls := smi.recorded(); len(calls) != 2 {
		t.Fatalf("full speed ran %v", calls)
	}
	// The cap is the mode such a machine really cannot be in, and that one is
	// reported.
	capped, err := controller.SetPowerMode(ctx, store.PowerModeCool)
	if err != nil {
		t.Fatal(err)
	}
	if capped.Failure != missingToolSentence {
		t.Fatalf("the cap on a machine with no tool reads %q", capped.Failure)
	}
}
