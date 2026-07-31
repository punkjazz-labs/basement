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
// packaging/systemd/runonspark-manager.service; a test enforces that.
//
//go:embed assets/runonspark-manager.service
var systemdUnit string

const (
	installDir  = "/usr/lib/runonspark-manager"
	binaryPath  = installDir + "/runonspark-manager"
	dataDir     = "/var/lib/runonspark-manager"
	unitPath    = "/etc/systemd/system/runonspark-manager.service"
	dropInDir   = "/etc/systemd/system/runonspark-manager.service.d"
	dropInPath  = dropInDir + "/listen.conf"
	stagingPath = "/tmp/runonspark-manager.staged"
	// releaseURL is where tagged releases publish the linux/arm64 binary
	// (ADR 0010; asset naming is part of the release contract).
	releaseURL = "https://github.com/punkjazz-labs/runonspark-manager/releases/latest/download"
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

// Result is what the operator needs after a successful install.
type Result struct {
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
		"curl -fsSL %[1]s/runonspark-manager-linux-arm64 -o %[2]s && "+
			"curl -fsSL %[1]s/runonspark-manager-linux-arm64.sha256 -o %[2]s.sha256 && "+
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
func Install(ctx context.Context, runner Runner, source BinarySource, opts Options, logf func(string, ...any)) (Result, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if _, err := runner.Run(ctx, "command -v systemctl", nil); err != nil {
		return Result{}, fmt.Errorf("%s has no systemd; RunOnSpark Manager needs a systemd-based OS", runner.Describe())
	}
	if _, err := runner.Run(ctx, "getent group docker", nil); err != nil {
		return Result{}, fmt.Errorf("Docker is not installed on %s; install Docker first, then rerun setup", runner.Describe())
	}

	staged, err := source.Stage(ctx, runner, logf)
	if err != nil {
		return Result{}, err
	}

	logf("installing binary and service user")
	steps := []string{
		"install -d -m 0755 " + installDir,
		fmt.Sprintf("install -m 0755 %s %s", staged, binaryPath),
		"getent group runonspark >/dev/null || groupadd --system runonspark",
		"getent passwd runonspark >/dev/null || useradd --system --gid runonspark --home-dir " + dataDir + " --shell /usr/sbin/nologin runonspark",
		"usermod -a -G docker runonspark",
		"install -d -o runonspark -g runonspark -m 0750 " + dataDir,
	}
	for _, step := range steps {
		if _, err := runner.RunPrivileged(ctx, step, nil); err != nil {
			return Result{}, fmt.Errorf("%s: %w", step, err)
		}
	}
	if _, err := runner.RunPrivileged(ctx, "tee "+unitPath+" >/dev/null", strings.NewReader(systemdUnit)); err != nil {
		return Result{}, fmt.Errorf("write systemd unit: %w", err)
	}

	listen, err := resolveListen(ctx, runner, opts.Listen)
	if err != nil {
		return Result{}, err
	}
	if listen == "" {
		// Loopback default: drop any previous override so reruns converge.
		if _, err := runner.RunPrivileged(ctx, "rm -f "+dropInPath, nil); err != nil {
			return Result{}, err
		}
	} else {
		logf("console will listen on %s", listen)
		dropIn := fmt.Sprintf("[Service]\nExecStart=\nExecStart=%s --data-dir %s --listen %s\n", binaryPath, dataDir, listen)
		if _, err := runner.RunPrivileged(ctx, "install -d -m 0755 "+dropInDir+" && tee "+dropInPath+" >/dev/null", strings.NewReader(dropIn)); err != nil {
			return Result{}, fmt.Errorf("write listen configuration: %w", err)
		}
	}

	logf("starting runonspark-manager.service")
	if _, err := runner.RunPrivileged(ctx, "systemctl daemon-reload && systemctl enable --now runonspark-manager.service && systemctl restart runonspark-manager.service", nil); err != nil {
		return Result{}, fmt.Errorf("start service: %w", err)
	}

	if len(opts.DiscoveredPeers) > 0 {
		fleet, err := json.MarshalIndent(map[string]any{
			"schema":           1,
			"recorded_at":      time.Now().UTC().Format(time.RFC3339),
			"discovered_peers": opts.DiscoveredPeers,
		}, "", "  ")
		if err == nil {
			payload := string(fleet) + "\n"
			if _, err := runner.RunPrivileged(ctx, "tee "+dataDir+"/fleet.json >/dev/null && chown runonspark:runonspark "+dataDir+"/fleet.json", strings.NewReader(payload)); err != nil {
				logf("note: could not record discovered peers: %v", err)
			}
		}
	}

	token := waitForToken(ctx, runner)
	result := Result{Token: token, Loopback: listen == ""}
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
