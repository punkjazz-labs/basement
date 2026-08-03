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
	// Summary reports a completed install.
	Summary(result InstallResult)
}

// ErrDeclined signals that the operator declined to proceed (e.g. chose not
// to connect to a machine that did not look like a GB10) and the UI has
// already told them nothing was installed. Callers must not print anything
// further for it — just exit non-zero.
var ErrDeclined = errors.New("declined")

// DiscoverAndChoose finds GB10-class machines on the network and lets ui
// pick one. It returns the chosen machine's display name and the remaining
// candidates (for fleet recording).
func DiscoverAndChoose(ctx context.Context, ui WizardUI) (target string, peers []string, err error) {
	ui.Progress("Scanning your network for GB10 machines (DGX Spark, ASUS Ascent GX10, MSI EdgeXpert, …)")
	candidates, err := discovery.Discover(ctx, func(string, ...any) {})
	if err != nil {
		return "", nil, fmt.Errorf("discovery failed: %w", err)
	}
	if len(candidates) == 0 {
		// Capitalized and punctuated as a sentence on purpose: this text
		// reaches the operator verbatim, not just a Go error log.
		return "", nil, errors.New("No SSH-reachable machines found. Is the GB10 machine on this network?\n  You can also point setup directly:  basement setup --host <ip>")
	}

	index, err := ui.ChooseMachine(candidates)
	if err != nil {
		return "", nil, err
	}
	if index < 0 || index >= len(candidates) {
		return "", nil, errors.New("not a valid choice")
	}
	picked := candidates[index]

	// Stop the obvious mistake before any connection: hostname hints are
	// not proof, so a custom-named GB10 can still proceed deliberately —
	// but the default answer is no.
	if !discovery.LikelyGB10Name(picked.Hostname) {
		proceed, err := ui.ConfirmNonGB10(DisplayHost(picked))
		if err != nil {
			return "", nil, err
		}
		if !proceed {
			return "", nil, ErrDeclined
		}
	}

	target = picked.DisplayName()
	for position, candidate := range candidates {
		if position != index {
			peers = append(peers, candidate.DisplayName())
		}
	}
	return target, peers, nil
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
