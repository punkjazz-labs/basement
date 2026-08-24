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

// commandTimeout bounds the one command. nvidia-smi answers in well under a
// second on a healthy machine, so a bound this generous only ever catches a
// driver that has stopped answering at all.
const commandTimeout = 10 * time.Second

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
	output, err := exec.CommandContext(commandCtx, "nvidia-smi", args...).CombinedOutput()
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return errNoTool
	}
	// Checked on the context rather than on the error text: a killed process
	// reports itself as a signal, and only the deadline knows it was a
	// deadline.
	if commandCtx.Err() != nil {
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
	// mu keeps one change at a time. Two owners moving the same switch at once
	// must not leave the recorded failure of one attempt beside the mode of
	// the other.
	mu sync.Mutex
}

func NewController(database *store.Store, run Runner) *Controller {
	if run == nil {
		run = Command
	}
	return &Controller{database: database, run: run}
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
func (c *Controller) ApplyStored(ctx context.Context) (store.PowerMode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, err := c.database.PowerMode(ctx)
	if err != nil {
		return store.PowerMode{}, err
	}
	return c.apply(ctx, current)
}

// apply runs the command for one mode and records the outcome. Only a database
// that cannot be written comes back as an error: the GPU refusing is a state
// of this Spark, not a failure of the request that reached it.
func (c *Controller) apply(ctx context.Context, current store.PowerMode) (store.PowerMode, error) {
	arguments, err := Arguments(current.Mode)
	if err != nil {
		return store.PowerMode{}, err
	}
	return c.database.RecordPowerModeFailure(ctx, failureSentence(c.run(ctx, arguments...)))
}
