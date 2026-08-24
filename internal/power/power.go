// Package power holds this Spark's GPU power mode: the durable setting, and
// the one command that puts it on the GPU.
//
// The whole feature is one clock cap. "cool" holds the GB10 SM clock at or
// below CoolClockMHz, which on hardware costs about a third of the peak GPU
// power and six degrees of heat while decode speed stays within one percent of
// full speed. "full" removes the cap.
//
// basement owns this because the driver does not: the cap is not durable, so
// it is gone after every reboot. The manager writes the owner's choice down
// and asks the GPU for it again at every start.
//
// Every failure here is open. A machine with no GPU, no driver, or a
// nvidia-smi that hangs runs exactly as it did before this feature existed:
// at full speed, with one plain sentence recorded beside the setting. Nothing
// in this package can stop a manager from starting or a model from serving.
package power

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/punkjazz-labs/basement/internal/store"
)

// CoolClockMHz is the top SM clock a cool Spark runs at. It is a constant and
// not a setting: one value was qualified on hardware, and a slider over clock
// numbers is a support burden that buys the owner nothing.
const CoolClockMHz = 2200

// commandTimeout bounds the one command, and pipeGrace bounds the wait for its
// output after that. nvidia-smi answers in well under a second on a healthy
// machine, so bounds this generous only ever catch a driver that has stopped
// answering at all.
//
// Both are needed, and the second one is not obvious. Killing the process at
// the deadline does not end the wait for its output: a child the driver left
// behind keeps the pipes open, and reading them blocks until it exits. Measured
// on this repository, that turned a ten second bound into sixty. The wait is
// what holds the Controller lock, so an unbounded one wedges every later change
// on this machine and reads to the fleet as a Spark that does not answer.
// WaitDelay is what closes the pipes, so the true bound is these two added.
//
// They are variables only so that the test that proves the bound can shorten
// them. Production never reassigns either.
var (
	commandTimeout = 10 * time.Second
	pipeGrace      = 2 * time.Second
)

// The two failures that have a plainer word than "it failed". They are
// sentinels so that Command can report a cause and failureSentence can name
// it, without either of them reading a driver message for keywords.
var (
	errNoTool  = errors.New("nvidia-smi is not installed on this machine")
	errTimeout = errors.New("nvidia-smi did not answer before the deadline")
)

// The sentences the console shows. Each one says what did not happen, in the
// owner's terms, and none of them quotes the driver: a driver message belongs
// in the log, beside the error this package returns.
const (
	missingToolSentence = "This machine has no nvidia-smi command, so the GPU clock did not change."
	timeoutSentence     = "The nvidia-smi command did not answer in time, so the GPU clock did not change."
	commandSentence     = "The nvidia-smi command failed, so the GPU clock did not change."
)

// Runner is the one command this package ever runs. Production is Command;
// a test supplies its own and never touches a GPU.
type Runner func(ctx context.Context, args ...string) error

// Arguments is the exact nvidia-smi call one mode needs. The cool form caps
// the clock range at its top end and leaves the bottom at zero, so the GPU
// still idles down as it always did.
func Arguments(mode string) ([]string, error) {
	switch mode {
	case store.PowerModeCool:
		return []string{"-lgc", "0," + strconv.Itoa(CoolClockMHz)}, nil
	case store.PowerModeFull:
		return []string{"-rgc"}, nil
	}
	return nil, store.ErrPowerMode
}

// Command is the runner every real Spark uses. It is the only place in this
// product that runs nvidia-smi to change something rather than to read
// something.
func Command(ctx context.Context, args ...string) error {
	commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, "nvidia-smi", args...)
	// Without this the deadline above bounds the process and not the call: see
	// pipeGrace. With it, a pipe still held after the process is gone ends the
	// wait instead of outliving it.
	command.WaitDelay = pipeGrace
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return errNoTool
	}
	// Checked on the context rather than on the error text: a killed process
	// reports itself as a signal, and only the deadline knows it was a
	// deadline. A pipe that outlived its grace is the same answer in the same
	// words: what did not arrive in time is the answer, whatever still holds
	// the far end of it.
	if commandCtx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
		return errTimeout
	}
	detail := firstLine(string(output))
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(value), "\n")
	return strings.TrimSpace(line)
}

// failureSentence turns one failure into the one sentence recorded beside the
// setting. Anything this package has no plainer word for is still a failure of
// the command, so it reads as one.
func failureSentence(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errNoTool):
		return missingToolSentence
	case errors.Is(err, errTimeout):
		return timeoutSentence
	}
	return commandSentence
}

// Controller owns the power mode of the machine it runs on. It is the whole
// node-local boundary: the console reaches it through the local API, and the
// fleet controller reaches it through the mutual TLS transport, but only this
// type ever writes the setting or speaks to the GPU.
type Controller struct {
	database *store.Store
	run      Runner
	logger   *slog.Logger
	// mu keeps one change at a time. Two owners moving the same switch at once
	// must not leave the recorded failure of one attempt beside the mode of
	// the other.
	mu sync.Mutex
}

func NewController(database *store.Store, run Runner) *Controller {
	if run == nil {
		run = Command
	}
	return &Controller{database: database, run: run, logger: slog.Default()}
}

// SetLogger gives this controller the manager's own logger, so the driver's
// own words about a refusal land in the same place as everything else the
// manager says. Called once at startup. Without it the standard logger is
// used, which the service journal still captures.
func (c *Controller) SetLogger(logger *slog.Logger) {
	if logger == nil {
		return
	}
	c.mu.Lock()
	c.logger = logger
	c.mu.Unlock()
}

// PowerMode reads the setting and changes nothing.
func (c *Controller) PowerMode(ctx context.Context) (store.PowerMode, error) {
	return c.database.PowerMode(ctx)
}

// SetPowerMode records the owner's choice and then asks the GPU for it. The
// choice is written first and on purpose: a Spark whose driver refuses the cap
// today still asks for it at its next start, and the console can say which of
// the two happened.
//
// A model that is serving is not consulted and not disturbed. The cap applies
// to work already in flight, which is exactly what the owner asked for.
func (c *Controller) SetPowerMode(ctx context.Context, mode string) (store.PowerMode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, err := c.database.SetPowerMode(ctx, mode)
	if err != nil {
		return store.PowerMode{}, err
	}
	return c.apply(ctx, current)
}

// ApplyStored puts the stored mode back on the GPU. The driver forgets the cap
// at every reboot, so this runs once at every manager start. It reports what
// it found, and a caller that only wanted the side effect can ignore it.
//
// A setting nobody has ever chosen is left alone. Every Spark starts at full
// speed already, so resetting the clock at boot would change nothing that
// needed changing, and on a machine with no driver at all it would record a
// failure against a mode that is in force. A machine that has never met this
// feature must look exactly as it did before the feature existed.
func (c *Controller) ApplyStored(ctx context.Context) (store.PowerMode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, err := c.database.PowerMode(ctx)
	if err != nil {
		return store.PowerMode{}, err
	}
	if current.UpdatedAt == "" {
		return current, nil
	}
	return c.apply(ctx, current)
}

// apply runs the command for one mode and records the outcome. Only a database
// that cannot be written comes back as an error: the GPU refusing is a state
// of this Spark, not a failure of the request that reached it.
func (c *Controller) apply(ctx context.Context, current store.PowerMode) (store.PowerMode, error) {
	arguments, argumentsErr := Arguments(current.Mode)
	if argumentsErr != nil {
		return store.PowerMode{}, argumentsErr
	}
	err := c.run(ctx, arguments...)
	// A machine with no nvidia-smi is a machine at full speed, so full speed is
	// in force on it and there is nothing to report. Saying otherwise would put
	// a failure on every machine that has no driver, about the one mode such a
	// machine is guaranteed to be in.
	if current.Mode == store.PowerModeFull && errors.Is(err, errNoTool) {
		err = nil
	}
	if err != nil {
		// The driver's own words, once, here. They are what a person debugging
		// a Spark needs and they are never stored: what the console shows is
		// the constant sentence beside this line.
		c.logger.Warn("GPU power mode was not applied", "mode", current.Mode, "error", err)
	}
	return c.database.RecordPowerModeFailure(ctx, failureSentence(err))
}
