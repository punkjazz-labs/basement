package setup

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// The embedded unit is a byte-for-byte copy of
// packaging/systemd/basement.service; a test enforces that.
//
//go:embed assets/basement.service
var systemdUnit string

const (
	installDir  = "/usr/lib/basement"
	binaryPath  = installDir + "/basement"
	dataDir     = "/var/lib/basement"
	unitPath    = "/etc/systemd/system/basement.service"
	dropInDir   = "/etc/systemd/system/basement.service.d"
	dropInPath  = dropInDir + "/listen.conf"
	stagingPath = "/tmp/basement.staged"
	serviceUser = "basement"
	// releaseURL is where tagged releases publish the linux/arm64 binary
	// (ADR 0010; asset naming is part of the release contract). It still
	// points at the runonspark-manager repository: the repo itself renames
	// separately from this branch (docs/plans/10-rename-basement.md).
	releaseURL = "https://github.com/punkjazz-labs/runonspark-manager/releases/latest/download"

	// Pre-rename (spec 10) names and paths. A machine set up before the
	// rename still runs under these; Install adopts it in place (see
	// adoptLegacyInstall) instead of orphaning it next to a second,
	// parallel installation.
	legacyDataDir     = "/var/lib/runonspark-manager"
	legacyUnitName    = "runonspark-manager.service"
	legacyUnitPath    = "/etc/systemd/system/" + legacyUnitName
	legacyServiceUser = "runonspark"
)

// ListenMode selects which interface the console binds after install.
type ListenMode string

const (
	ListenLoopback  ListenMode = "loopback"
	ListenTailscale ListenMode = "tailscale"
	ListenLAN       ListenMode = "lan"
)

// Options configures one installation.
type Options struct {
	Listen ListenMode
	// DiscoveredPeers are other machines found on the network, recorded on
	// the master for future distributed (multi-GB10) recipes.
	DiscoveredPeers []string
}

// InstallResult is what the operator needs after a successful install.
type InstallResult struct {
	ConsoleURL string
	AltURL     string
	Token      string
	Loopback   bool
}

// BinarySource stages the linux/arm64 manager binary onto the target and
// returns its staged path there.
type BinarySource interface {
	Stage(ctx context.Context, runner Runner, logf func(string, ...any)) (string, error)
}

// LocalFileSource uses a file already present on the target (the local
// install path: the running binary itself).
type LocalFileSource struct{ Path string }

func (s LocalFileSource) Stage(context.Context, Runner, func(string, ...any)) (string, error) {
	return s.Path, nil
}

// UploadSource streams a local file to the target over the runner and
// verifies its sha256 on arrival.
type UploadSource struct{ Path string }

func (s UploadSource) Stage(ctx context.Context, runner Runner, logf func(string, ...any)) (string, error) {
	payload, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	if err := validateARM64ELF(payload); err != nil {
		return "", fmt.Errorf("%s: %w", s.Path, err)
	}
	digest := sha256.Sum256(payload)
	logf("uploading manager binary (%.1f MB)", float64(len(payload))/1e6)
	if _, err := runner.Run(ctx, "cat > "+stagingPath, strings.NewReader(string(payload))); err != nil {
		return "", fmt.Errorf("upload binary: %w", err)
	}
	out, err := runner.Run(ctx, "sha256sum "+stagingPath, nil)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(strings.TrimSpace(out), hex.EncodeToString(digest[:])) {
		return "", fmt.Errorf("uploaded binary failed checksum verification")
	}
	return stagingPath, nil
}

// ReleaseSource downloads the latest published release on the target itself
// and verifies the published checksum.
type ReleaseSource struct{}

func (ReleaseSource) Stage(ctx context.Context, runner Runner, logf func(string, ...any)) (string, error) {
	logf("downloading latest release on the target")
	script := fmt.Sprintf(
		"curl -fsSL %[1]s/basement-linux-arm64 -o %[2]s && "+
			"curl -fsSL %[1]s/basement-linux-arm64.sha256 -o %[2]s.sha256 && "+
			"cd /tmp && awk '{print $1\"  %[2]s\"}' %[2]s.sha256 | sha256sum -c -",
		releaseURL, stagingPath)
	if _, err := runner.Run(ctx, script, nil); err != nil {
		return "", fmt.Errorf("download release (no release published yet, or the target is offline — pass --binary with a linux/arm64 build instead): %w", err)
	}
	return stagingPath, nil
}

// validateARM64ELF rejects binaries that cannot run on a GB10 machine before
// any bytes travel: ELF magic plus the aarch64 machine type.
func validateARM64ELF(payload []byte) error {
	if len(payload) < 20 || payload[0] != 0x7f || payload[1] != 'E' || payload[2] != 'L' || payload[3] != 'F' {
		return fmt.Errorf("not a Linux ELF binary")
	}
	if machine := uint16(payload[18]) | uint16(payload[19])<<8; machine != 183 {
		return fmt.Errorf("not an ARM64 build (GB10 machines need GOOS=linux GOARCH=arm64)")
	}
	return nil
}

// Install performs the full installation through runner: system checks,
// binary, service user, systemd unit, listen drop-in, service start, pairing
// token. It mirrors packaging/install.sh so both paths stay equivalent.
func Install(ctx context.Context, runner Runner, source BinarySource, opts Options, logf func(string, ...any)) (InstallResult, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if _, err := runner.Run(ctx, "command -v systemctl", nil); err != nil {
		return InstallResult{}, fmt.Errorf("%s has no systemd; basement needs a systemd-based OS", runner.Describe())
	}
	if _, err := runner.Run(ctx, "getent group docker", nil); err != nil {
		return InstallResult{}, fmt.Errorf("Docker is not installed on %s; install Docker first, then rerun setup", runner.Describe())
	}
	// Fresh OEM GB10 machines often ship Docker without the NVIDIA runtime
	// registered; models cannot start without it, so register it here.
	runtimes, err := runner.RunPrivileged(ctx, "docker info --format '{{range $name, $ignored := .Runtimes}}{{$name}} {{end}}'", nil)
	if err != nil {
		return InstallResult{}, fmt.Errorf("Docker daemon is not reachable on %s: %w", runner.Describe(), err)
	}
	if !strings.Contains(runtimes, "nvidia") {
		if _, err := runner.Run(ctx, "command -v nvidia-ctk", nil); err != nil {
			return InstallResult{}, fmt.Errorf("the NVIDIA Container Toolkit is missing on %s; install nvidia-container-toolkit, then rerun setup", runner.Describe())
		}
		logf("registering the NVIDIA container runtime with Docker")
		if _, err := runner.RunPrivileged(ctx, "nvidia-ctk runtime configure --runtime=docker && systemctl restart docker", nil); err != nil {
			return InstallResult{}, fmt.Errorf("register NVIDIA container runtime: %w", err)
		}
	}

	staged, err := source.Stage(ctx, runner, logf)
	if err != nil {
		return InstallResult{}, err
	}

	if err := adoptLegacyInstall(ctx, runner, logf); err != nil {
		return InstallResult{}, err
	}

	logf("installing binary and service user")
	steps := []string{
		"install -d -m 0755 " + installDir,
		fmt.Sprintf("install -m 0755 %s %s", staged, binaryPath),
		"getent group " + serviceUser + " >/dev/null || groupadd --system " + serviceUser,
		"getent passwd " + serviceUser + " >/dev/null || useradd --system --gid " + serviceUser + " --home-dir " + dataDir + " --shell /usr/sbin/nologin " + serviceUser,
		"usermod -a -G docker " + serviceUser,
		"install -d -o " + serviceUser + " -g " + serviceUser + " -m 0750 " + dataDir,
	}
	for _, step := range steps {
		if _, err := runner.RunPrivileged(ctx, step, nil); err != nil {
			return InstallResult{}, fmt.Errorf("%s: %w", step, err)
		}
	}
	if _, err := runner.RunPrivileged(ctx, "tee "+unitPath+" >/dev/null", strings.NewReader(systemdUnit)); err != nil {
		return InstallResult{}, fmt.Errorf("write systemd unit: %w", err)
	}

	listen, err := resolveListen(ctx, runner, opts.Listen)
	if err != nil {
		return InstallResult{}, err
	}
	if listen == "" {
		// Loopback default: drop any previous override so reruns converge.
		if _, err := runner.RunPrivileged(ctx, "rm -f "+dropInPath, nil); err != nil {
			return InstallResult{}, err
		}
	} else {
		logf("console will listen on %s", listen)
		dropIn := fmt.Sprintf("[Service]\nExecStart=\nExecStart=%s --data-dir %s --listen %s\n", binaryPath, dataDir, listen)
		if _, err := runner.RunPrivileged(ctx, "install -d -m 0755 "+dropInDir+" && tee "+dropInPath+" >/dev/null", strings.NewReader(dropIn)); err != nil {
			return InstallResult{}, fmt.Errorf("write listen configuration: %w", err)
		}
	}

	logf("starting basement.service")
	if _, err := runner.RunPrivileged(ctx, "systemctl daemon-reload && systemctl enable --now basement.service && systemctl restart basement.service", nil); err != nil {
		return InstallResult{}, fmt.Errorf("start service: %w", err)
	}

	if len(opts.DiscoveredPeers) > 0 {
		fleet, err := json.MarshalIndent(map[string]any{
			"schema":           1,
			"recorded_at":      time.Now().UTC().Format(time.RFC3339),
			"discovered_peers": opts.DiscoveredPeers,
		}, "", "  ")
		if err == nil {
			payload := string(fleet) + "\n"
			if _, err := runner.RunPrivileged(ctx, "tee "+dataDir+"/fleet.json >/dev/null && chown "+serviceUser+":"+serviceUser+" "+dataDir+"/fleet.json", strings.NewReader(payload)); err != nil {
				logf("note: could not record discovered peers: %v", err)
			}
		}
	}

	token := waitForToken(ctx, runner)
	result := InstallResult{Token: token, Loopback: listen == ""}
	port := "7070"
	if index := strings.LastIndex(listen, ":"); index >= 0 {
		port = listen[index+1:]
	}
	if listen == "" {
		result.ConsoleURL = "http://127.0.0.1:" + port
	} else {
		result.ConsoleURL = "http://" + listen
		if short, err := runner.Run(ctx, "hostname -s 2>/dev/null || hostname", nil); err == nil && strings.TrimSpace(short) != "" {
			result.AltURL = fmt.Sprintf("http://%s.local:%s", strings.TrimSpace(short), port)
		}
	}
	return result, nil
}

// adoptLegacyInstall folds a pre-rename (spec 10) install into the current
// one instead of leaving it running side by side with a fresh install under
// the new names: it stops and disables the old unit, moves the old data
// directory (the SQLite database, artifacts, receipts and pairing state) in
// place with a single mv, and renames the old service account so uid/gid —
// and therefore file ownership — carries over untouched. It runs before the
// new unit and service user are installed, so those steps see whatever this
// leaves behind and converge on it.
//
// It never touches anything once the new data directory already exists:
// something already adopted or fresh-installed here, and moving old
// remnants on top of live new data could destroy it. That case is only
// reported through logf, never treated as an error — the new install is
// already the one in charge.
func adoptLegacyInstall(ctx context.Context, runner Runner, logf func(string, ...any)) error {
	oldUnitPresent, err := targetExists(ctx, runner, "test -f "+legacyUnitPath)
	if err != nil {
		return fmt.Errorf("check for a pre-rename unit: %w", err)
	}
	if oldUnitPresent {
		logf("found a pre-rename install (%s); adopting it", legacyUnitName)
		if _, err := runner.RunPrivileged(ctx, "systemctl disable --now "+legacyUnitName, nil); err != nil {
			return fmt.Errorf("stop pre-rename service: %w", err)
		}
	}

	newDataDirPresent, err := targetExists(ctx, runner, "test -d "+dataDir)
	if err != nil {
		return fmt.Errorf("check for the data directory: %w", err)
	}
	oldDataDirPresent, err := targetExists(ctx, runner, "test -d "+legacyDataDir)
	if err != nil {
		return fmt.Errorf("check for a pre-rename data directory: %w", err)
	}
	switch {
	case oldDataDirPresent && !newDataDirPresent:
		logf("moving %s to %s", legacyDataDir, dataDir)
		if _, err := runner.RunPrivileged(ctx, "mv "+legacyDataDir+" "+dataDir, nil); err != nil {
			return fmt.Errorf("move pre-rename data directory: %w", err)
		}
	case oldDataDirPresent && newDataDirPresent:
		logf("note: %s already exists; leaving %s in place untouched", dataDir, legacyDataDir)
	}

	oldUserPresent, err := targetExists(ctx, runner, "getent passwd "+legacyServiceUser+" >/dev/null 2>&1")
	if err != nil {
		return fmt.Errorf("check for a pre-rename service account: %w", err)
	}
	if oldUserPresent {
		logf("renaming service account %s to %s", legacyServiceUser, serviceUser)
		rename := "usermod -l " + serviceUser + " -d " + dataDir + " " + legacyServiceUser +
			" && groupmod -n " + serviceUser + " " + legacyServiceUser
		if _, err := runner.RunPrivileged(ctx, rename, nil); err != nil {
			return fmt.Errorf("rename pre-rename service account: %w", err)
		}
	}
	return nil
}

// targetExists runs check (expected to be a test(1) or getent invocation
// with no output of its own) and reports whether it succeeded, without
// treating "it does not exist" as a Runner error: check always exits 0, so
// only a genuine transport/execution failure reaches the caller as err.
func targetExists(ctx context.Context, runner Runner, check string) (bool, error) {
	out, err := runner.Run(ctx, check+" && echo present || echo absent", nil)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "present", nil
}

// resolveListen turns a listen mode into a concrete address using the
// target's own interfaces, exactly like install.sh does.
func resolveListen(ctx context.Context, runner Runner, mode ListenMode) (string, error) {
	switch mode {
	case ListenLoopback, "":
		return "", nil
	case ListenTailscale:
		out, err := runner.Run(ctx, "tailscale ip -4 2>/dev/null | head -n1", nil)
		address := strings.TrimSpace(out)
		if err != nil || address == "" {
			return "", fmt.Errorf("the target has no Tailscale address; pick loopback or lan instead")
		}
		return address + ":7070", nil
	case ListenLAN:
		out, err := runner.Run(ctx, "hostname -I 2>/dev/null | awk '{ print $1 }'", nil)
		address := strings.TrimSpace(out)
		if err != nil || address == "" {
			return "", fmt.Errorf("could not determine the target's LAN address")
		}
		return address + ":7070", nil
	default:
		return "", fmt.Errorf("unknown listen mode %q (loopback, tailscale, lan)", mode)
	}
}

// waitForToken polls for the pairing token the freshly started service mints.
func waitForToken(ctx context.Context, runner Runner) string {
	for attempt := 0; attempt < 20; attempt++ {
		out, err := runner.RunPrivileged(ctx, "cat "+dataDir+"/pairing-token 2>/dev/null", nil)
		if err == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out)
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(500 * time.Millisecond):
		}
	}
	return ""
}
