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
	database, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	smi := &fakeSMI{}
	return NewController(database, smi.run), database, smi
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
	if strings.Join(calls[0], " ") != "-lgc 0,2200" {
		t.Fatalf("cool ran nvidia-smi %v", calls[0])
	}
	if strings.Join(calls[1], " ") != "-rgc" {
		t.Fatalf("full ran nvidia-smi %v", calls[1])
	}
	if _, err := controller.SetPowerMode(ctx, "turbo"); !errors.Is(err, store.ErrPowerMode) {
		t.Fatalf("an unknown mode was accepted: %v", err)
	}
	if len(smi.recorded()) != 2 {
		t.Fatal("a refused mode still ran a command")
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
	err := Command(context.Background(), "-rgc")
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
	if len(calls) != 1 || strings.Join(calls[0], " ") != "-lgc 0,2200" {
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
