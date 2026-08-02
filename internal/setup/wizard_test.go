package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/punkjazz-labs/runonspark-manager/internal/discovery"
)

// stubUI is a scripted WizardUI for exercising the flow functions without a
// terminal or a browser.
type stubUI struct {
	chooseMachineIndex int
	chooseMachineErr   error
	confirmNonGB10     bool
	confirmNonGB10Err  error
	username           string
	usernameErr        error
	listenMode         ListenMode
	listenErr          error
	progressLines      []string
	summaries          []InstallResult
}

func (s *stubUI) Password(string) (string, error) { return "", nil }
func (s *stubUI) Confirm(string) (bool, error)     { return true, nil }
func (s *stubUI) ChooseMachine([]discovery.Candidate) (int, error) {
	return s.chooseMachineIndex, s.chooseMachineErr
}
func (s *stubUI) ConfirmNonGB10(string) (bool, error) { return s.confirmNonGB10, s.confirmNonGB10Err }
func (s *stubUI) AskUsername(_, suggested string) (string, error) {
	if s.username != "" {
		return s.username, s.usernameErr
	}
	return suggested, s.usernameErr
}
func (s *stubUI) ChooseListen(bool) (ListenMode, error) { return s.listenMode, s.listenErr }
func (s *stubUI) Progress(line string)                  { s.progressLines = append(s.progressLines, line) }
func (s *stubUI) Summary(result InstallResult)          { s.summaries = append(s.summaries, result) }

func TestFinishInstallDrivesChooseListenProgressAndSummary(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs["docker info"] = "nvidia runc \n"
	runner.outputs["pairing-token"] = "tok-xyz\n"
	ui := &stubUI{listenMode: ListenLoopback}

	result, err := FinishInstall(context.Background(), ui, runner, LocalFileSource{Path: "/tmp/binary"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Token != "tok-xyz" {
		t.Errorf("Token = %q", result.Token)
	}
	if len(ui.summaries) != 1 || ui.summaries[0].Token != "tok-xyz" {
		t.Errorf("Summary was not called with the install result: %+v", ui.summaries)
	}
	found := false
	for _, line := range ui.progressLines {
		if strings.HasPrefix(line, "  · ") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected at least one bulleted progress line, got %v", ui.progressLines)
	}
}

func TestFinishInstallPropagatesChooseListenError(t *testing.T) {
	runner := newFakeRunner()
	ui := &stubUI{listenErr: errors.New("boom")}
	if _, err := FinishInstall(context.Background(), ui, runner, LocalFileSource{Path: "/tmp/binary"}, nil, false); err == nil {
		t.Fatal("expected the listen choice error to propagate")
	}
	if len(ui.summaries) != 0 {
		t.Error("Summary must not be called when the listen choice fails")
	}
}

func TestFinishInstallPropagatesInstallErrorWithoutSummary(t *testing.T) {
	runner := newFakeRunner()
	runner.failures["getent group docker"] = "no such group"
	ui := &stubUI{listenMode: ListenLoopback}
	_, err := FinishInstall(context.Background(), ui, runner, LocalFileSource{Path: "/tmp/binary"}, nil, false)
	if err == nil || !strings.Contains(err.Error(), "install failed") {
		t.Fatalf("expected a wrapped install failure, got %v", err)
	}
	if len(ui.summaries) != 0 {
		t.Error("Summary must not be called after a failed install")
	}
}

func TestResolveUsernameFallsBackWhenNothingRemembered(t *testing.T) {
	ui := &stubUI{}
	got, err := ResolveUsername(ui, "no-such-host.invalid", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if got != "operator" {
		t.Errorf("ResolveUsername = %q, want the fallback %q", got, "operator")
	}
}

func TestDisplayHostStripsDotLocal(t *testing.T) {
	if got := DisplayHost(discovery.Candidate{Hostname: "spark-head.local"}); got != "spark-head" {
		t.Errorf("DisplayHost = %q", got)
	}
	if got := DisplayHost(discovery.Candidate{Hostname: "gx10-office"}); got != "gx10-office" {
		t.Errorf("DisplayHost = %q", got)
	}
}
