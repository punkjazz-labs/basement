package setup

import (
	"context"
	"encoding/binary"
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
		"groupadd --system runonspark",
		"usermod -a -G docker runonspark",
		"install -d -o runonspark -g runonspark -m 0750 " + dataDir,
		"tee " + unitPath,
		"tee " + dropInPath,
		"systemctl daemon-reload && systemctl enable --now runonspark-manager.service && systemctl restart runonspark-manager.service",
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

func TestInstallLoopbackRemovesDropInAndReportsTunnelURL(t *testing.T) {
	runner := newFakeRunner()
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
	runner.outputs["tailscale ip -4"] = "\n"
	if _, err := Install(context.Background(), runner, LocalFileSource{Path: "/tmp/binary"}, Options{Listen: ListenTailscale}, nil); err == nil {
		t.Fatal("tailscale mode succeeded without a tailscale address")
	}
}

// The embedded unit must stay identical to the packaged one so the script
// install path and the setup command produce the same service.
func TestEmbeddedUnitMatchesPackaging(t *testing.T) {
	packaged, err := os.ReadFile("../../packaging/systemd/runonspark-manager.service")
	if err != nil {
		t.Fatal(err)
	}
	if string(packaged) != systemdUnit {
		t.Fatal("internal/setup/assets/runonspark-manager.service differs from packaging/systemd/runonspark-manager.service — keep them identical")
	}
}
