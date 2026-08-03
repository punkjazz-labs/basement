package setup

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// fakeRunner records commands and answers them from canned outputs.
type fakeRunner struct {
	commands   []string
	privileged []string
	outputs    map[string]string // substring match → stdout
	failures   map[string]string // substring match → error text
	writes     map[string]string // tee target → stdin payload
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{outputs: map[string]string{}, failures: map[string]string{}, writes: map[string]string{}}
}

func (f *fakeRunner) Describe() string { return "fake" }

func (f *fakeRunner) respond(command string, stdin io.Reader) (string, error) {
	for fragment, message := range f.failures {
		if strings.Contains(command, fragment) {
			return "", &commandError{message}
		}
	}
	if stdin != nil && strings.Contains(command, "tee ") {
		payload, _ := io.ReadAll(stdin)
		f.writes[command] = string(payload)
	}
	for fragment, output := range f.outputs {
		if strings.Contains(command, fragment) {
			return output, nil
		}
	}
	return "", nil
}

type commandError struct{ message string }

func (e *commandError) Error() string { return e.message }

func (f *fakeRunner) Run(_ context.Context, command string, stdin io.Reader) (string, error) {
	f.commands = append(f.commands, command)
	return f.respond(command, stdin)
}

func (f *fakeRunner) RunPrivileged(_ context.Context, command string, stdin io.Reader) (string, error) {
	f.privileged = append(f.privileged, command)
	return f.respond(command, stdin)
}

func TestIdentityClassification(t *testing.T) {
	cases := []struct {
		name     string
		identity Identity
		gb10     bool
	}{
		{"dgx spark via gpu", Identity{GPUName: "NVIDIA GB10"}, true},
		{"dgx spark via device tree", Identity{DeviceModel: "NVIDIA DGX Spark"}, true},
		{"oem via device tree", Identity{DeviceModel: "ASUS Ascent GX10 (GB10)"}, true},
		{"case insensitive", Identity{GPUName: "nvidia gb10"}, true},
		{"regular linux box", Identity{GPUName: "NVIDIA GeForce RTX 4090", DeviceModel: ""}, false},
		{"no gpu at all", Identity{}, false},
	}
	for _, test := range cases {
		if got := test.identity.IsGB10(); got != test.gb10 {
			t.Errorf("%s: IsGB10 = %v, want %v", test.name, got, test.gb10)
		}
	}
	if product := (Identity{DeviceModel: "NVIDIA DGX Spark", GPUName: "NVIDIA GB10"}).Product(); product != "NVIDIA DGX Spark" {
		t.Errorf("Product prefers device model, got %q", product)
	}
}

func TestValidateARM64ELF(t *testing.T) {
	arm64 := make([]byte, 64)
	copy(arm64, []byte{0x7f, 'E', 'L', 'F'})
	binary.LittleEndian.PutUint16(arm64[18:], 183)
	if err := validateARM64ELF(arm64); err != nil {
		t.Errorf("valid arm64 ELF rejected: %v", err)
	}
	amd64 := make([]byte, 64)
	copy(amd64, []byte{0x7f, 'E', 'L', 'F'})
	binary.LittleEndian.PutUint16(amd64[18:], 62)
	if err := validateARM64ELF(amd64); err == nil {
		t.Error("amd64 ELF accepted")
	}
	if err := validateARM64ELF([]byte("#!/bin/sh\n")); err == nil {
		t.Error("script accepted as ELF")
	}
}

func TestInstallRunsFullPlanForLAN(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["docker info"] = "io.containerd.runc.v2 nvidia runc \n"
	runner.outputs["hostname -I"] = "192.168.99.134 \n"
	runner.outputs["pairing-token"] = "tok-abc\n"
	runner.outputs["hostname -s"] = "spark-head\n"

	result, err := Install(context.Background(), runner, LocalFileSource{Path: "/tmp/binary"},
		Options{Listen: ListenLAN, DiscoveredPeers: []string{"gx10-office.local"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConsoleURL != "http://192.168.99.134:7070" {
		t.Errorf("ConsoleURL = %q", result.ConsoleURL)
	}
	if result.AltURL != "http://spark-head.local:7070" {
		t.Errorf("AltURL = %q", result.AltURL)
	}
	if result.Token != "tok-abc" {
		t.Errorf("Token = %q", result.Token)
	}
	if result.Loopback {
		t.Error("LAN install reported loopback")
	}

	joined := strings.Join(runner.privileged, "\n")
	for _, required := range []string{
		"install -m 0755 /tmp/binary " + binaryPath,
		"groupadd --system basement",
		"usermod -a -G docker basement",
		"install -d -o basement -g basement -m 0750 " + dataDir,
		"tee " + unitPath,
		"tee " + dropInPath,
		"systemctl daemon-reload && systemctl enable --now basement.service && systemctl restart basement.service",
		"tee " + dataDir + "/fleet.json",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("privileged plan is missing %q", required)
		}
	}

	var dropIn string
	for command, payload := range runner.writes {
		if strings.Contains(command, dropInPath) {
			dropIn = payload
		}
	}
	if !strings.Contains(dropIn, "--listen 192.168.99.134:7070") {
		t.Errorf("listen drop-in = %q", dropIn)
	}
	var fleet string
	for command, payload := range runner.writes {
		if strings.Contains(command, "fleet.json") {
			fleet = payload
		}
	}
	if !strings.Contains(fleet, "gx10-office.local") {
		t.Errorf("fleet.json is missing the discovered peer: %q", fleet)
	}
}

// Two cabled Sparks put a self-assigned 169.254 cluster address first in
// `hostname -I`; the LAN listen address must never be that. The default
// route's source address wins, and without one the first non-link-local
// address does.
func TestInstallLANSkipsLinkLocalAddress(t *testing.T) {
	route := newFakeRunner()
	route.outputs["docker info"] = "nvidia runc \n"
	route.outputs["route get"] = "192.168.99.148\n"
	route.outputs["hostname -I"] = "169.254.205.1 192.168.99.148 100.64.0.15 \n"
	route.outputs["pairing-token"] = "tok\n"
	result, err := Install(context.Background(), route, LocalFileSource{Path: "/tmp/binary"}, Options{Listen: ListenLAN}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConsoleURL != "http://192.168.99.148:7070" {
		t.Errorf("ConsoleURL = %q, want the default route's source address", result.ConsoleURL)
	}

	fallback := newFakeRunner()
	fallback.outputs["docker info"] = "nvidia runc \n"
	fallback.outputs["hostname -I"] = "169.254.205.1 192.168.99.148 100.64.0.15 \n"
	fallback.outputs["pairing-token"] = "tok\n"
	result, err = Install(context.Background(), fallback, LocalFileSource{Path: "/tmp/binary"}, Options{Listen: ListenLAN}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConsoleURL != "http://192.168.99.148:7070" {
		t.Errorf("ConsoleURL = %q, want the first non-link-local address", result.ConsoleURL)
	}

	linkLocalOnly := newFakeRunner()
	linkLocalOnly.outputs["docker info"] = "nvidia runc \n"
	linkLocalOnly.outputs["hostname -I"] = "169.254.205.1 \n"
	if _, err := Install(context.Background(), linkLocalOnly, LocalFileSource{Path: "/tmp/binary"}, Options{Listen: ListenLAN}, nil); err == nil {
		t.Error("a machine with only a link-local address was given a LAN listen address")
	}
}

// A fresh OEM machine with Docker but no registered NVIDIA runtime gets the
// runtime configured during install.
func TestInstallRegistersNvidiaRuntimeWhenMissing(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["docker info"] = "io.containerd.runc.v2 runc \n"
	runner.outputs["pairing-token"] = "tok\n"
	if _, err := Install(context.Background(), runner, LocalFileSource{Path: "/tmp/binary"}, Options{}, nil); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.privileged, "\n")
	if !strings.Contains(joined, "nvidia-ctk runtime configure --runtime=docker && systemctl restart docker") {
		t.Error("install did not register the NVIDIA runtime")
	}
}

func TestInstallExplainsMissingContainerToolkit(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["docker info"] = "runc \n"
	runner.failures["command -v nvidia-ctk"] = "not found"
	_, err := Install(context.Background(), runner, LocalFileSource{Path: "/tmp/binary"}, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "nvidia-container-toolkit") {
		t.Fatalf("expected a toolkit guidance error, got %v", err)
	}
}

func TestInstallLoopbackRemovesDropInAndReportsTunnelURL(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["docker info"] = "nvidia runc \n"
	runner.outputs["pairing-token"] = "tok\n"
	result, err := Install(context.Background(), runner, LocalFileSource{Path: "/tmp/binary"}, Options{Listen: ListenLoopback}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Loopback || result.ConsoleURL != "http://127.0.0.1:7070" {
		t.Errorf("loopback result = %+v", result)
	}
	if !strings.Contains(strings.Join(runner.privileged, "\n"), "rm -f "+dropInPath) {
		t.Error("loopback install must remove a stale listen drop-in")
	}
}

func TestInstallRefusesWithoutDocker(t *testing.T) {
	runner := newFakeRunner()
	runner.failures["getent group docker"] = "no such group"
	if _, err := Install(context.Background(), runner, LocalFileSource{Path: "/tmp/binary"}, Options{}, nil); err == nil {
		t.Fatal("install proceeded without Docker")
	} else if !strings.Contains(err.Error(), "Docker") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestInstallTailscaleModeNeedsTailscaleAddress(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["docker info"] = "nvidia runc \n"
	runner.outputs["tailscale ip -4"] = "\n"
	if _, err := Install(context.Background(), runner, LocalFileSource{Path: "/tmp/binary"}, Options{Listen: ListenTailscale}, nil); err == nil {
		t.Fatal("tailscale mode succeeded without a tailscale address")
	}
}

// TestInstallFreshHasNoLegacyAdoption is the baseline for the adopt tests
// below: a machine that never ran a pre-rename install must not trigger any
// of the legacy-adoption commands, only the ordinary fresh-install ones.
func TestInstallFreshHasNoLegacyAdoption(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["docker info"] = "nvidia runc \n"
	runner.outputs["pairing-token"] = "tok\n"
	if _, err := Install(context.Background(), runner, LocalFileSource{Path: "/tmp/binary"}, Options{}, nil); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(append(append([]string{}, runner.commands...), runner.privileged...), "\n")
	for _, unexpected := range []string{
		"systemctl disable --now " + legacyUnitName,
		"mv " + legacyDataDir,
		"usermod -l " + serviceUser,
	} {
		if strings.Contains(joined, unexpected) {
			t.Errorf("fresh install ran a legacy-adoption command: %q\nplan:\n%s", unexpected, joined)
		}
	}
	joined = strings.Join(runner.privileged, "\n")
	for _, required := range []string{
		"getent group " + serviceUser + " >/dev/null || groupadd --system " + serviceUser,
		"getent passwd " + serviceUser + " >/dev/null || useradd",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("fresh install plan is missing %q", required)
		}
	}
}

// TestInstallAdoptsPreRenameInstall is the dangerous path: a machine running
// the pre-rename unit, data directory and service account must be folded
// into the new names in place — service stopped and disabled, data moved
// with a single mv (never copy-then-delete), account renamed so uid/gid
// (and therefore file ownership) carries over.
func TestInstallAdoptsPreRenameInstall(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["docker info"] = "nvidia runc \n"
	runner.outputs["pairing-token"] = "tok\n"
	runner.outputs["test -f "+legacyUnitPath] = "present\n"
	runner.outputs["test -d "+legacyDataDir] = "present\n"
	runner.outputs["getent passwd "+legacyServiceUser] = "present\n"

	if _, err := Install(context.Background(), runner, LocalFileSource{Path: "/tmp/binary"}, Options{}, nil); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(runner.privileged, "\n")
	for _, required := range []string{
		"systemctl disable --now " + legacyUnitName,
		"mv " + legacyDataDir + " " + dataDir,
		"usermod -l " + serviceUser + " -d " + dataDir + " " + legacyServiceUser + " && groupmod -n " + serviceUser + " " + legacyServiceUser,
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("adopt plan is missing %q\nplan:\n%s", required, joined)
		}
	}
	// Never copy-then-delete: the data directory move must be a single mv,
	// never a separate copy step (cp, rsync, tar) anywhere in the plan.
	for _, forbidden := range []string{"cp -r " + legacyDataDir, "rsync"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("adopt plan copied instead of moving the data directory: %q", forbidden)
		}
	}
}

// TestInstallLeavesNewInstallAloneWhenLegacyRemnantsExist is the mixed case:
// the new data directory already exists (a prior adopt or fresh install
// already happened here), so pre-rename remnants must be left untouched —
// never overwritten, never merged — and the situation only reported.
func TestInstallLeavesNewInstallAloneWhenLegacyRemnantsExist(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["docker info"] = "nvidia runc \n"
	runner.outputs["pairing-token"] = "tok\n"
	runner.outputs["test -d "+dataDir] = "present\n"
	runner.outputs["test -d "+legacyDataDir] = "present\n"

	var notes []string
	logf := func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
	if _, err := Install(context.Background(), runner, LocalFileSource{Path: "/tmp/binary"}, Options{}, logf); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(runner.privileged, "\n")
	if strings.Contains(joined, "mv "+legacyDataDir) {
		t.Errorf("install moved %s even though %s already exists\nplan:\n%s", legacyDataDir, dataDir, joined)
	}
	reported := false
	for _, note := range notes {
		if strings.Contains(note, legacyDataDir) && strings.Contains(note, dataDir) {
			reported = true
		}
	}
	if !reported {
		t.Errorf("leftover legacy data directory was not reported; log lines: %#v", notes)
	}
}

// The embedded unit must stay identical to the packaged one so the script
// install path and the setup command produce the same service.
func TestEmbeddedUnitMatchesPackaging(t *testing.T) {
	packaged, err := os.ReadFile("../../packaging/systemd/basement.service")
	if err != nil {
		t.Fatal(err)
	}
	if string(packaged) != systemdUnit {
		t.Fatal("internal/setup/assets/basement.service differs from packaging/systemd/basement.service — keep them identical")
	}
}

// The address a target reports for itself decides where its console binds,
// and nothing else. When the caller already verified an address (the console
// adopts the machine it signed in to over SSH), that is the address the
// result names, and the machine's own answer is demoted to an alternate.
func TestInstallConsoleURLFollowsTheVerifiedAddress(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["docker info"] = "nvidia runc \n"
	runner.outputs["pairing-token"] = "tok\n"
	// The hostile answer: an accomplice's address.
	runner.outputs["hostname -I"] = "203.0.113.9 \n"
	runner.outputs["hostname -s"] = "accomplice\n"

	result, err := Install(context.Background(), runner, LocalFileSource{Path: "/tmp/binary"},
		Options{Listen: ListenLAN, ConsoleHost: "192.168.99.137"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConsoleURL != "http://192.168.99.137:7070" {
		t.Errorf("ConsoleURL = %q, want the address the caller verified", result.ConsoleURL)
	}
	if result.AltURL != "http://203.0.113.9:7070" {
		t.Errorf("AltURL = %q, want the target's own answer kept separate", result.AltURL)
	}
	// The service still binds the address the machine actually holds.
	var dropIn string
	for command, payload := range runner.writes {
		if strings.Contains(command, dropInPath) {
			dropIn = payload
		}
	}
	if !strings.Contains(dropIn, "--listen 203.0.113.9:7070") {
		t.Errorf("listen drop-in = %q, want the target's own address", dropIn)
	}

	// Without ConsoleHost (the terminal wizard, where a person chose the
	// address and reads the result) nothing changes.
	plain := newFakeRunner()
	plain.outputs["docker info"] = "nvidia runc \n"
	plain.outputs["pairing-token"] = "tok\n"
	plain.outputs["hostname -I"] = "192.168.99.134 \n"
	plain.outputs["hostname -s"] = "spark-head\n"
	unanchored, err := Install(context.Background(), plain, LocalFileSource{Path: "/tmp/binary"}, Options{Listen: ListenLAN}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unanchored.ConsoleURL != "http://192.168.99.134:7070" || unanchored.AltURL != "http://spark-head.local:7070" {
		t.Errorf("unanchored install = %+v", unanchored)
	}
}
