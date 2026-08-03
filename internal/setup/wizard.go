package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/punkjazz-labs/basement/internal/discovery"
)

// WizardUI drives one interactive setup run: discover-or-fixed-target,
// choose a machine, connect, confirm hardware, choose how the console
// listens, install, and report the result. cmd/basement/setup.go (terminal)
// and internal/setupweb (browser) each implement it; every decision the
// flow makes lives once, here, so both surfaces stay in lockstep.
type WizardUI interface {
	Prompter // Password, Confirm — unchanged, SSH needs them mid-flow

	// ConfirmAlways asks a yes/no question that must reach the operator even
	// in an unattended run. Confirm may answer itself when the operator
	// passed --yes; this one may not. It exists for one question only:
	// whether to install on a machine that merely turned up in a discovery
	// sweep. On a shared network that machine can belong to somebody else,
	// so a flag must never decide it. An implementation that cannot ask
	// (no terminal, closed input) returns an error, which the flow reads as
	// "no".
	ConfirmAlways(prompt string) (bool, error)
	// ChooseMachine presents candidates and returns the chosen index (0
	// based) or an error if nothing valid was chosen.
	ChooseMachine(candidates []discovery.Candidate) (int, error)
	// ConfirmNonGB10 warns that name did not look like a GB10 machine by
	// hostname and asks whether to connect anyway.
	ConfirmNonGB10(name string) (bool, error)
	// AskUsername asks for the SSH username on target, offering suggested
	// as the default; implementations resolve an empty answer to suggested
	// themselves.
	AskUsername(target, suggested string) (string, error)
	// ChooseListen asks which interface the console should bind. remote
	// reports whether the install target is a different machine than the
	// one running the wizard (it changes the recommended default).
	ChooseListen(remote bool) (ListenMode, error)
	// Progress announces one line of status text. Implementations decide
	// how to render it; the flow never assumes a particular presentation.
	Progress(line string)
	// Summary reports a completed install. A run that installs two machines
	// calls it twice, in install order.
	Summary(result InstallResult)
	// NextSteps reports what to do after the run, one line per entry, once
	// every machine has been dealt with. It is only called when there is
	// something concrete to say (see PairingSteps).
	NextSteps(lines []string)
}

// ErrDeclined signals that the operator declined to proceed (e.g. chose not
// to connect to a machine that did not look like a GB10) and the UI has
// already told them nothing was installed. Callers must not print anything
// further for it — just exit non-zero.
var ErrDeclined = errors.New("declined")

// Discovered is what one discovery pass and choice produced.
type Discovered struct {
	// Target is the machine the operator chose, addressed the way setup
	// connects to it.
	Target string
	// Peers are all the other machines the sweep found, in discovery order.
	// They are recorded on the target as fleet groundwork (ADR 0010).
	Peers []string
	// Offer is the subset of Peers whose hostname looks GB10-class: the
	// only machines setup offers to install on in the same run. A network
	// can hold plenty of unrelated SSH hosts, and walking the operator
	// through every one of them would be noise at best and somebody else's
	// hardware at worst. A GB10 under a custom hostname is missed here on
	// purpose; setup can be run again for it.
	Offer []string
}

// DiscoverAndChoose finds GB10-class machines on the network and lets ui
// pick one.
func DiscoverAndChoose(ctx context.Context, ui WizardUI) (Discovered, error) {
	ui.Progress("Scanning your network for GB10 machines (DGX Spark, ASUS Ascent GX10, MSI EdgeXpert, …)")
	candidates, err := discovery.Discover(ctx, func(string, ...any) {})
	if err != nil {
		return Discovered{}, fmt.Errorf("discovery failed: %w", err)
	}
	if len(candidates) == 0 {
		// Capitalized and punctuated as a sentence on purpose: this text
		// reaches the operator verbatim, not just a Go error log.
		return Discovered{}, errors.New("No SSH-reachable machines found. Is the GB10 machine on this network?\n  You can also point setup directly:  basement setup --host <ip>")
	}

	index, err := ui.ChooseMachine(candidates)
	if err != nil {
		return Discovered{}, err
	}
	if index < 0 || index >= len(candidates) {
		return Discovered{}, errors.New("not a valid choice")
	}
	picked := candidates[index]

	// Stop the obvious mistake before any connection: hostname hints are
	// not proof, so a custom-named GB10 can still proceed deliberately —
	// but the default answer is no.
	if !discovery.LikelyGB10Name(picked.Hostname) {
		proceed, err := ui.ConfirmNonGB10(DisplayHost(picked))
		if err != nil {
			return Discovered{}, err
		}
		if !proceed {
			return Discovered{}, ErrDeclined
		}
	}

	found := Discovered{Target: picked.DisplayName()}
	for position, candidate := range candidates {
		if position == index {
			continue
		}
		found.Peers = append(found.Peers, candidate.DisplayName())
		if discovery.LikelyGB10Name(candidate.Hostname) {
			found.Offer = append(found.Offer, candidate.DisplayName())
		}
	}
	return found, nil
}

// DisplayHost strips the cosmetic ".local" suffix mDNS names carry.
func DisplayHost(candidate discovery.Candidate) string {
	return strings.TrimSuffix(candidate.DisplayName(), ".local")
}

// ResolveUsername looks up a remembered username for target, falling back to
// fallback, and asks ui to confirm or change it. The answer is remembered
// only after a successful connection (see ConnectAndVerify) — an unverified
// guess must never overwrite a known-good login.
func ResolveUsername(ui WizardUI, target, fallback string) (string, error) {
	suggested := fallback
	if known := RememberedUser(target); known != "" {
		suggested = known
	}
	return ui.AskUsername(target, suggested)
}

// ConnectAndVerify dials target over SSH as sshUser, confirms the machine
// carries a GB10 superchip, and remembers the username once the connection
// succeeds.
func ConnectAndVerify(ctx context.Context, ui WizardUI, target, sshUser string) (*SSHRunner, error) {
	ui.Progress("→ Connecting to " + sshUser + "@" + target + "…")
	runner, err := DialSSH(ctx, target, sshUser, ui)
	if err != nil {
		return nil, err
	}
	RememberUser(target, sshUser)

	identity := Probe(ctx, runner)
	if !identity.IsGB10() {
		runner.Close()
		gpu := identity.GPUName
		if gpu == "" {
			gpu = "none detected"
		}
		return nil, fmt.Errorf("%s is not a GB10 machine (GPU: %s) — basement recipes are built for the GB10 superchip, so setup will not install here", target, gpu)
	}
	descriptor := identity.Hostname
	if identity.OSName != "" {
		descriptor += ", " + identity.OSName
	}
	ui.Progress("✓ Confirmed: " + identity.Product() + " (" + descriptor + ")")
	return runner, nil
}

// FinishInstall asks how the console should listen, runs the install, and
// reports the result. remote reports whether the target is a different
// machine than the one running the wizard (it changes the recommended
// listen default). This is the tail shared by every entry point: the local
// GB10 shortcut, a remote SSH install, and the browser wizard.
func FinishInstall(ctx context.Context, ui WizardUI, runner Runner, source BinarySource, peers []string, remote bool) (InstallResult, error) {
	mode, err := ui.ChooseListen(remote)
	if err != nil {
		return InstallResult{}, err
	}
	result, err := Install(ctx, runner, source, Options{Listen: mode, DiscoveredPeers: peers}, func(format string, args ...any) {
		ui.Progress("  · " + fmt.Sprintf(format, args...))
	})
	if err != nil {
		return InstallResult{}, fmt.Errorf("install failed: %w", err)
	}
	ui.Summary(result)
	return result, nil
}

// Machine is one machine a run installed on: the address setup connected to
// and the result that install produced.
type Machine struct {
	Target string
	Result InstallResult
}

// connectTarget is ConnectAndVerify behind a variable so tests can stand in
// a fake machine instead of dialling SSH. Production never reassigns it.
var connectTarget = func(ctx context.Context, ui WizardUI, target, sshUser string) (Runner, func(), error) {
	runner, err := ConnectAndVerify(ctx, ui, target, sshUser)
	if err != nil {
		return nil, func() {}, err
	}
	return runner, func() { runner.Close() }, nil
}

// InstallMore continues a run that already installed on first: it offers the
// other GB10-class machines discovery found, one at a time, and installs on
// the ones the operator accepts — same username prompt, same SSH connection
// and GB10 identity check, same install as the first machine. The offer is
// always a question, never assumed: see WizardUI.ConfirmAlways.
//
// It stops offering at the first "no" (the operator is done adding
// machines), and it never fails the run: a machine that cannot be installed
// is reported and the machines already installed keep their result. When the
// run ends up touching more than one machine, the pairing guidance goes out
// through ui.NextSteps.
//
// Additional machines are installed without DiscoveredPeers: the fleet.json
// groundwork (ADR 0010) is recorded on the machine the operator chose first,
// and nothing reads it yet either way.
func InstallMore(ctx context.Context, ui WizardUI, first Machine, offer []string, source BinarySource, sshUser string) []Machine {
	installed := []Machine{first}
	var pending []string
	for index, target := range offer {
		accepted, err := ui.ConfirmAlways("Set up " + target + " as well?")
		if err != nil || !accepted {
			pending = append(pending, offer[index:]...)
			break
		}
		result, err := installOne(ctx, ui, target, sshUser, source)
		if err != nil {
			ui.Progress("✗ " + target + ": " + err.Error())
			ui.Progress("  Nothing changed on " + target + "; the machines already set up are unaffected.")
			pending = append(pending, target)
			continue
		}
		installed = append(installed, Machine{Target: target, Result: result})
	}
	if steps := PairingSteps(installed, pending); len(steps) > 0 {
		ui.NextSteps(steps)
	}
	return installed
}

// installOne runs the whole per-machine tail for an additional machine:
// which account to use, connect and confirm the hardware, install, report.
func installOne(ctx context.Context, ui WizardUI, target, sshUser string, source BinarySource) (InstallResult, error) {
	// The second machine can well have a different account than the first,
	// so ask, with the account that already worked as the default.
	username, err := ResolveUsername(ui, target, sshUser)
	if err != nil {
		return InstallResult{}, err
	}
	runner, closeRunner, err := connectTarget(ctx, ui, target, username)
	if err != nil {
		return InstallResult{}, err
	}
	defer closeRunner()
	return FinishInstall(ctx, ui, runner, source, nil, true)
}

// PairingSteps renders what the operator should do once every machine in the
// run has been dealt with. Pairing itself is deliberately manual: an API key
// is generated in one console and pasted into the other, so nothing here
// invents an automatic trust exchange (that is ADR 0005 work).
//
// Two or more machines installed: the consoles, then the three steps that
// make them a fleet. One machine installed with another GB10-class machine
// still on the network: a pointer at where the same path starts later.
// Anything else: nothing to say.
func PairingSteps(installed []Machine, pending []string) []string {
	if len(installed) == 1 && len(pending) > 0 {
		return []string{
			pending[0] + " is also on this network.",
			"Run this installer again for it when you want to pair the two under Fleet.",
		}
	}
	if len(installed) < 2 {
		return nil
	}
	head, worker := installed[0], installed[1]
	lines := []string{"Next: pair them, so models that need two Sparks can run."}
	for _, machine := range installed {
		lines = append(lines, "  "+machine.Target+"  "+machine.Result.ConsoleURL)
	}
	lines = append(lines, "",
		"1. Open "+worker.Result.ConsoleURL+", go to the Connect tab and create an API key.",
		"2. Open "+head.Result.ConsoleURL+", go to the Fleet tab, choose Add a Spark, and enter",
		"   "+worker.Result.ConsoleURL+" with that key.",
		"3. Models that need two Sparks then become installable from "+head.Target+".")
	if len(installed) > 2 {
		lines = append(lines, "", "A two-Spark model uses exactly one other Spark, so add only one under Fleet.")
	}
	for _, machine := range installed[:2] {
		if machine.Result.Loopback {
			lines = append(lines, "",
				"Pairing needs both consoles reachable from each other. "+machine.Target+" listens on",
				"loopback only, so run setup there again and pick the local network or Tailscale option.")
		}
	}
	return lines
}

// PickSource chooses how the linux/arm64 manager binary reaches the target:
// an explicit --binary path, this process's own binary when it is already a
// linux/arm64 build, or a release download performed on the target itself.
func PickSource(binaryFlag string) BinarySource {
	if binaryFlag != "" {
		return UploadSource{Path: binaryFlag}
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == "arm64" {
		if executable, err := os.Executable(); err == nil {
			return UploadSource{Path: executable}
		}
	}
	return ReleaseSource{}
}

// DefaultSSHUser guesses a reasonable default SSH username from the local
// account running the wizard.
func DefaultSSHUser() string {
	if current, err := user.Current(); err == nil && current.Username != "" {
		// Windows reports DOMAIN\name; only the name part is a useful default.
		name := current.Username
		if index := strings.LastIndexByte(name, '\\'); index >= 0 {
			name = name[index+1:]
		}
		return name
	}
	return "nvidia"
}

// Usernames that connected successfully are remembered per machine — never
// passwords or keys, only the account name.
func knownUsersPath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(configDir, "basement", "known-users.json")
}

// legacyKnownUsersPath is the pre-rename (spec 10) location. RememberedUser
// falls back to reading it when the new path has nothing yet, so upgrading
// this machine's setup binary does not forget every remembered login;
// RememberUser only ever writes the new path.
func legacyKnownUsersPath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(configDir, "runonspark-manager", "known-users.json")
}

// RememberedUser returns the last username that connected successfully to
// target, or "" if none is known.
func RememberedUser(target string) string {
	if user := rememberedUserAt(knownUsersPath(), target); user != "" {
		return user
	}
	return rememberedUserAt(legacyKnownUsersPath(), target)
}

func rememberedUserAt(path, target string) string {
	if path == "" {
		return ""
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var users map[string]string
	if json.Unmarshal(payload, &users) != nil {
		return ""
	}
	return users[strings.ToLower(target)]
}

// RememberUser records username as the one that connects to target.
func RememberUser(target, username string) {
	path := knownUsersPath()
	if path == "" {
		return
	}
	users := map[string]string{}
	if payload, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(payload, &users)
	}
	users[strings.ToLower(target)] = username
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if payload, err := json.MarshalIndent(users, "", "  "); err == nil {
		_ = os.WriteFile(path, payload, 0o600)
	}
}
