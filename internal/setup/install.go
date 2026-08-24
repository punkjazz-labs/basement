package setup

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// The embedded unit is a byte-for-byte copy of
// packaging/systemd/basement.service; a test enforces that.
//
//go:embed assets/basement.service
var systemdUnit string

//go:embed assets/basement-updater.service
var updaterSystemdUnit string

//go:embed assets/basement-updater.path
var updaterPathUnit string

// The embedded grant is a byte-for-byte copy of
// packaging/sudoers/basement-power; the same test enforces that. It names the
// two command lines internal/power can ever run and nothing else, so reading
// it is the whole answer to what this service may do as root.
//
//go:embed assets/basement-power.sudoers
var powerSudoers string

const (
	installDir          = "/usr/lib/basement"
	binaryPath          = installDir + "/basement"
	dataDir             = "/var/lib/basement"
	unitPath            = "/etc/systemd/system/basement.service"
	updaterUnitPath     = "/etc/systemd/system/basement-updater.service"
	updaterPathUnitPath = "/etc/systemd/system/basement-updater.path"
	dropInDir           = "/etc/systemd/system/basement.service.d"
	dropInPath          = dropInDir + "/listen.conf"
	sudoersDir          = "/etc/sudoers.d"
	sudoersPath         = sudoersDir + "/basement-power"
	stagingPath         = "/tmp/basement.staged"
	updaterStagingPath  = "/tmp/basement-updater.staged"
	serviceUser         = "basement"
	// releaseURL is where tagged releases publish the linux/arm64 binary
	// (ADR 0010; asset naming is part of the release contract). It still
	// points at the runonspark-manager repository: the repo itself renames
	// separately from this branch (docs/plans/10-rename-basement.md).
	releaseURL = "https://github.com/punkjazz-labs/basement/releases/latest/download"

	// Pre-rename (spec 10) names and paths. A machine set up before the
	// rename still runs under these; Install adopts it in place (see
	// adoptLegacyInstall) instead of orphaning it next to a second,
	// parallel installation.
	legacyDataDir     = "/var/lib/runonspark-manager"
	legacyUnitName    = "runonspark-manager.service"
	legacyUnitPath    = "/etc/systemd/system/" + legacyUnitName
	legacyServiceUser = "runonspark"
	serviceReadyWait  = 90 * time.Second
)

// ListenMode selects which interface the console binds after install.
type ListenMode string

const (
	ListenLoopback  ListenMode = "loopback"
	ListenTailscale ListenMode = "tailscale"
	ListenLAN       ListenMode = "lan"
	// ListenLANTailscale binds both addresses at once: the console answers
	// on the local network and on the tailnet. The LAN address is the
	// primary one.
	ListenLANTailscale ListenMode = "lan+tailscale"
)

// Options configures one installation.
type Options struct {
	Listen ListenMode
	// DiscoveredPeers are other machines found on the network, recorded on
	// the master for future distributed (multi-GB10) recipes.
	DiscoveredPeers []string
	// ConsoleHost anchors the reported ConsoleURL to an address the caller
	// already verified, instead of the one the target reports for itself.
	// The console's adoption path sets it to the address the owner adopted
	// and signed in to: `hostname -I` and `tailscale ip` are answers from a
	// machine that is not ours yet, and a hostile endpoint naming an
	// accomplice would otherwise redirect everything that follows the
	// install. The terminal wizard leaves it empty, because there a person
	// chose the address and reads the result before acting on it.
	ConsoleHost string
}

// InstallResult is what the operator needs after a successful install.
//
// ConsoleURL is the address to use. ConsoleURLs are every address the new
// console now answers on, primary first, and its first entry is always
// ConsoleURL: an install that binds one address has one entry. AltURL is
// another address the same console can be reached at, for display: when
// ConsoleHost was set, that is an address the target reported for itself,
// and it is not to be used for anything the caller's trust depends on.
type InstallResult struct {
	ConsoleURL  string
	ConsoleURLs []string
	AltURL      string
	Token       string
	Loopback    bool
}

// ExtraConsoleURLs are the addresses the new console answers on besides
// ConsoleURL, in bound order. An install that binds one address, which is
// most of them, has none.
func (r InstallResult) ExtraConsoleURLs() []string {
	if len(r.ConsoleURLs) < 2 {
		return nil
	}
	return r.ConsoleURLs[1:]
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

func stageUpdater(ctx context.Context, runner Runner, logf func(string, ...any)) (string, error) {
	logf("downloading the fixed root updater helper")
	script := fmt.Sprintf(
		"curl -fsSL %[1]s/basement-updater-linux-arm64 -o %[2]s && "+
			"curl -fsSL %[1]s/basement-updater-linux-arm64.sha256 -o %[2]s.sha256 && "+
			"cd /tmp && awk '{print $1\"  basement-updater.staged\"}' basement-updater.staged.sha256 | sha256sum -c -",
		releaseURL, updaterStagingPath)
	if _, err := runner.Run(ctx, script, nil); err != nil {
		return "", fmt.Errorf("download root updater helper: %w", err)
	}
	return updaterStagingPath, nil
}

const slotInstallScript = `set -eu
manager=$1
updater=$2
install_root=/usr/lib/basement
versions=$install_root/versions
flat=$install_root/basement

install -d -m 0755 "$install_root" "$versions" "$install_root/updater"

if [ -f "$flat" ] && [ ! -L "$flat" ]; then
  old_digest=$(sha256sum "$flat" | awk '{print $1}')
  old_version=$("$flat" version 2>/dev/null || true)
  if printf '%s\n' "$old_version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
    old_slot=$old_version
  else
    old_slot=bootstrap-$old_digest
  fi
  if [ ! -e "$versions/$old_slot/basement" ]; then
    install -d -m 0755 "$versions/$old_slot"
    install -m 0755 "$flat" "$versions/$old_slot/basement"
  fi
fi

manager_version=$("$manager" version 2>/dev/null || true)
if printf '%s\n' "$manager_version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
  slot=$manager_version
else
  manager_digest=$(sha256sum "$manager" | awk '{print $1}')
  slot=bootstrap-$manager_digest
fi

target=$versions/$slot
temporary=$versions/.$slot.install.$$
rm -rf "$temporary"
install -d -m 0755 "$temporary"
install -m 0755 "$manager" "$temporary/basement"
if [ -d "$target" ] && [ ! -f "$target/basement" ]; then
  echo "manager version slot is incomplete" >&2
  rm -rf "$temporary"
  exit 1
elif [ -f "$target/basement" ]; then
  old_digest=$(sha256sum "$target/basement" | awk '{print $1}')
  new_digest=$(sha256sum "$temporary/basement" | awk '{print $1}')
  if [ "$old_digest" != "$new_digest" ]; then
    echo "manager version slot already contains different bytes" >&2
    rm -rf "$temporary"
    exit 1
  fi
  rm -rf "$temporary"
elif [ ! -e "$target" ]; then
  mv "$temporary" "$target"
else
  echo "manager version slot is not a directory" >&2
  rm -rf "$temporary"
  exit 1
fi

current_tmp=$install_root/.current.$$
rm -f "$current_tmp"
ln -s "versions/$slot" "$current_tmp"
mv -Tf "$current_tmp" "$install_root/current"
rm -f "$flat"
ln -s current/basement "$flat"
install -m 0755 "$updater" "$install_root/updater/basement-updater"
`

// installSudoersScript delivers the GPU power grant, and everything in it
// exists for one guarantee: /etc/sudoers.d never holds a file with the real
// name and the wrong bytes. A broken file there makes every sudo on the machine
// fail, including the one that would repair it.
//
// So the bytes land under a temporary name in the same directory, are checked
// with visudo, and only then move into place. A move inside one directory is
// atomic, so a reader sees the old grant or the new one and never half of
// either. The temporary name starts with a dot and carries a second one, and
// sudo ignores any file in that directory whose name carries a dot, so debris
// from an interrupted run is inert and the next run clears it.
//
// A machine without visudo cannot be checked, so it is left alone and the
// install goes on. Such a machine has no sudo either, so the grant would buy it
// nothing; the power switch says so in its own words rather than through a
// failed install.
const installSudoersScript = `set -eu
if ! command -v visudo >/dev/null 2>&1; then
  echo "this machine has no visudo, so the GPU power grant was not installed" >&2
  exit 0
fi
install -d -o root -g root -m 0755 ` + sudoersDir + `
temporary=$(mktemp ` + sudoersDir + `/.basement-power.XXXXXX)
trap 'rm -f "$temporary"' EXIT
tee "$temporary" >/dev/null
chown root:root "$temporary"
chmod 0440 "$temporary"
visudo -cf "$temporary" >/dev/null
mv -f "$temporary" ` + sudoersPath + `
trap - EXIT
`

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
	updaterStaged, err := stageUpdater(ctx, runner, logf)
	if err != nil {
		return InstallResult{}, err
	}

	if err := adoptLegacyInstall(ctx, runner, logf); err != nil {
		return InstallResult{}, err
	}

	logf("installing binary and service user")
	steps := []string{
		"getent group " + serviceUser + " >/dev/null || groupadd --system " + serviceUser,
		"getent passwd " + serviceUser + " >/dev/null || useradd --system --gid " + serviceUser + " --home-dir " + dataDir + " --shell /usr/sbin/nologin " + serviceUser,
		"usermod -a -G docker " + serviceUser,
		"install -d -o " + serviceUser + " -g " + serviceUser + " -m 0750 " + dataDir,
		"install -d -o " + serviceUser + " -g " + serviceUser + " -m 0750 " + dataDir + "/updates " + dataDir + "/updates/staging " + dataDir + "/updates/staging/pending " + dataDir + "/updates/staging/partial",
		"install -d -o root -g root -m 0755 /var/lib/basement-updater",
	}
	for _, step := range steps {
		if _, err := runner.RunPrivileged(ctx, step, nil); err != nil {
			return InstallResult{}, fmt.Errorf("%s: %w", step, err)
		}
	}
	if _, err := runner.RunPrivileged(ctx, "systemctl disable --now basement-updater.path >/dev/null 2>&1 || true; systemctl stop basement-updater.service >/dev/null 2>&1 || true", nil); err != nil {
		return InstallResult{}, fmt.Errorf("stop updater trigger: %w", err)
	}
	if _, err := runner.RunPrivileged(ctx, "sh -s -- "+shellQuote(staged)+" "+shellQuote(updaterStaged), strings.NewReader(slotInstallScript)); err != nil {
		return InstallResult{}, fmt.Errorf("install manager version slot: %w", err)
	}
	if _, err := runner.RunPrivileged(ctx, "tee "+unitPath+" >/dev/null", strings.NewReader(systemdUnit)); err != nil {
		return InstallResult{}, fmt.Errorf("write systemd unit: %w", err)
	}
	if _, err := runner.RunPrivileged(ctx, "tee "+updaterUnitPath+" >/dev/null", strings.NewReader(updaterSystemdUnit)); err != nil {
		return InstallResult{}, fmt.Errorf("write updater systemd unit: %w", err)
	}
	if _, err := runner.RunPrivileged(ctx, "tee "+updaterPathUnitPath+" >/dev/null", strings.NewReader(updaterPathUnit)); err != nil {
		return InstallResult{}, fmt.Errorf("write updater path unit: %w", err)
	}
	logf("granting the service the two GPU clock commands")
	if _, err := runner.RunPrivileged(ctx, installSudoersScript, strings.NewReader(powerSudoers)); err != nil {
		return InstallResult{}, fmt.Errorf("write %s: %w", sudoersPath, err)
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
		logf("console will listen on %s", strings.Join(listenAddresses(listen), " and "))
		dropIn := fmt.Sprintf("[Service]\nExecStart=\nExecStart=%s --data-dir %s --listen %s\n", binaryPath, dataDir, listen)
		if _, err := runner.RunPrivileged(ctx, "install -d -m 0755 "+dropInDir+" && tee "+dropInPath+" >/dev/null", strings.NewReader(dropIn)); err != nil {
			return InstallResult{}, fmt.Errorf("write listen configuration: %w", err)
		}
	}

	logf("starting basement.service")
	if _, err := runner.RunPrivileged(ctx, "systemctl daemon-reload && systemctl enable basement.service basement-updater.service basement-updater.path && systemctl restart basement.service && systemctl start basement-updater.service && systemctl start basement-updater.path && systemctl is-active --quiet basement.service && systemctl is-active --quiet basement-updater.path && test -x /usr/lib/basement/updater/basement-updater", nil); err != nil {
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

	// The health check, the port and every reported URL follow the primary
	// address. A console that binds several addresses still has one identity.
	addresses := listenAddresses(listen)
	primary := ""
	if len(addresses) > 0 {
		primary = addresses[0]
	}
	healthAddress := primary
	if healthAddress == "" {
		healthAddress = "127.0.0.1:7070"
	}
	token, err := waitForService(ctx, runner, "http://"+healthAddress+"/healthz")
	if err != nil {
		return InstallResult{}, err
	}
	result := InstallResult{Token: token, Loopback: primary == ""}
	port := "7070"
	if _, listenPort, err := net.SplitHostPort(primary); err == nil && listenPort != "" {
		port = listenPort
	}
	// Both of these are built out of what the target said about itself: the
	// address it bound, and the name it calls itself. Useful to show a
	// person, and never more than that.
	reportedURL, localURL := "", ""
	if primary != "" {
		reportedURL = "http://" + primary
		if short, err := runner.Run(ctx, "hostname -s 2>/dev/null || hostname", nil); err == nil && strings.TrimSpace(short) != "" {
			localURL = fmt.Sprintf("http://%s.local:%s", strings.TrimSpace(short), port)
		}
	}
	switch {
	case primary == "":
		result.ConsoleURL = "http://127.0.0.1:" + port
		result.ConsoleURLs = []string{result.ConsoleURL}
	case opts.ConsoleHost != "":
		result.ConsoleURL = "http://" + net.JoinHostPort(opts.ConsoleHost, port)
		// Only the address the caller verified is a console URL here. Every
		// other address the target bound is the target's own answer, so it
		// stays in AltURL, for display and nothing more.
		result.ConsoleURLs = []string{result.ConsoleURL}
		for _, alternate := range []string{reportedURL, localURL} {
			if alternate != "" && alternate != result.ConsoleURL {
				result.AltURL = alternate
				break
			}
		}
	default:
		result.ConsoleURL = reportedURL
		result.AltURL = localURL
		for _, address := range addresses {
			result.ConsoleURLs = append(result.ConsoleURLs, "http://"+address)
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

// resolveListen turns a listen mode into the concrete addresses the console
// binds, using the target's own interfaces, exactly like install.sh does.
// One mode can resolve to more than one address: the result is then the
// comma separated list the manager's --listen flag takes, primary first.
func resolveListen(ctx context.Context, runner Runner, mode ListenMode) (string, error) {
	switch mode {
	case ListenLoopback, "":
		return "", nil
	case ListenTailscale:
		return resolveTailscaleListen(ctx, runner, "pick loopback or lan instead")
	case ListenLAN:
		return resolveLANListen(ctx, runner)
	case ListenLANTailscale:
		// The LAN address comes first. It is the primary one: the fleet
		// listener and every URL the machine reports for itself follow it.
		lan, err := resolveLANListen(ctx, runner)
		if err != nil {
			return "", err
		}
		tailscale, err := resolveTailscaleListen(ctx, runner, "pick lan instead")
		if err != nil {
			return "", err
		}
		return lan + "," + tailscale, nil
	default:
		return "", fmt.Errorf("unknown listen mode %q (loopback, tailscale, lan, lan+tailscale)", mode)
	}
}

// resolveTailscaleListen reads the target's tailnet address. advice names the
// mode to use instead when the target has none.
func resolveTailscaleListen(ctx context.Context, runner Runner, advice string) (string, error) {
	out, err := runner.Run(ctx, "tailscale ip -4 2>/dev/null | head -n1", nil)
	address := strings.TrimSpace(out)
	if err != nil || address == "" {
		return "", fmt.Errorf("the target has no Tailscale address; %s", advice)
	}
	return address + ":7070", nil
}

// resolveLANListen reads the target's local network address.
//
// hostname -I lists addresses in interface enumeration order, and a cluster
// port with link but no DHCP puts its self-assigned 169.254 address first
// (two cabled Sparks do exactly this). The default route's source address is
// the one the LAN actually reaches; a machine without a default route falls
// back to the first address that is not link-local.
func resolveLANListen(ctx context.Context, runner Runner) (string, error) {
	out, _ := runner.Run(ctx, "ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.*src \\([0-9.]*\\).*/\\1/p' | head -n1", nil)
	address := strings.TrimSpace(out)
	if address == "" {
		out, err := runner.Run(ctx, "hostname -I 2>/dev/null", nil)
		if err != nil {
			return "", fmt.Errorf("could not determine the target's LAN address")
		}
		for _, field := range strings.Fields(out) {
			if ip := net.ParseIP(field); ip != nil && ip.To4() != nil && !ip.IsLinkLocalUnicast() {
				address = field
				break
			}
		}
	}
	if address == "" {
		return "", fmt.Errorf("could not determine the target's LAN address")
	}
	return address + ":7070", nil
}

// listenAddresses splits a resolved listen value into its addresses. An
// install that binds the local network and Tailscale carries both here,
// comma separated, exactly as the manager's --listen flag takes them.
func listenAddresses(listen string) []string {
	var addresses []string
	for _, field := range strings.Split(listen, ",") {
		if address := strings.TrimSpace(field); address != "" {
			addresses = append(addresses, address)
		}
	}
	return addresses
}

// waitForService proves both halves of a usable first launch: the HTTP server
// answers on the interface setup configured, and the pairing token exists.
// systemd's active state proves neither one, so it is never sufficient for the
// success card.
func waitForService(ctx context.Context, runner Runner, healthURL string) (string, error) {
	deadline := time.Now().Add(serviceReadyWait)
	var token string
	var healthReady bool
	for {
		if token == "" {
			out, err := runner.RunPrivileged(ctx, "cat "+dataDir+"/pairing-token 2>/dev/null", nil)
			if err == nil {
				token = strings.TrimSpace(out)
			}
		}
		if !healthReady {
			_, err := runner.Run(ctx, "curl -fsS --max-time 2 -o /dev/null "+shellQuote(healthURL), nil)
			healthReady = err == nil
		}
		if token != "" && healthReady {
			return token, nil
		}
		if time.Now().After(deadline) {
			switch {
			case !healthReady && token == "":
				return "", fmt.Errorf("basement.service started, but %s did not answer and no pairing token appeared", healthURL)
			case !healthReady:
				return "", fmt.Errorf("basement.service started, but %s did not answer", healthURL)
			default:
				return "", fmt.Errorf("basement.service started, but no pairing token appeared at %s/pairing-token", dataDir)
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for basement.service readiness: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
