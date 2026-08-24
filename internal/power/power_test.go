package power

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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

// Startup puts the stored mode back, once, because the driver forgot it at
// the reboot. A Spark that was never asked for anything asks for full speed,
// which is what it already runs at.
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

func TestStartupOnAHardwareFreeMachineChangesNothingItCannot(t *testing.T) {
	ctx := context.Background()
	controller, _, smi := newTestController(t)
	smi.err = errTimeout

	state, err := controller.ApplyStored(ctx)
	if err != nil {
		t.Fatalf("startup failed on a machine with no GPU: %v", err)
	}
	if state.Mode != store.PowerModeFull {
		t.Fatalf("a fresh machine reads %+v, want full speed", state)
	}
	if state.Failure != timeoutSentence {
		t.Fatalf("the timeout sentence reads %q", state.Failure)
	}
}
