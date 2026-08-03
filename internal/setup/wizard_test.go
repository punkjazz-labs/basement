package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/discovery"
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
	// confirmAlways are the answers to hand out, in order; an exhausted
	// script answers no.
	confirmAlways        []bool
	confirmAlwaysPrompts []string
	nextSteps            [][]string
}

func (s *stubUI) Password(string) (string, error) { return "", nil }
func (s *stubUI) Confirm(string) (bool, error)    { return true, nil }
func (s *stubUI) ConfirmAlways(prompt string) (bool, error) {
	s.confirmAlwaysPrompts = append(s.confirmAlwaysPrompts, prompt)
	if len(s.confirmAlways) == 0 {
		return false, nil
	}
	answer := s.confirmAlways[0]
	s.confirmAlways = s.confirmAlways[1:]
	return answer, nil
}
func (s *stubUI) NextSteps(lines []string) { s.nextSteps = append(s.nextSteps, lines) }
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

// installableRunner is a fake machine every install step succeeds on.
func installableRunner(lanIP, hostname string) *fakeRunner {
	runner := newFakeRunner()
	runner.outputs["docker info"] = "nvidia runc \n"
	runner.outputs["hostname -I"] = lanIP + "\n"
	runner.outputs["hostname -s"] = hostname + "\n"
	runner.outputs["pairing-token"] = "tok-" + hostname + "\n"
	return runner
}

// fakeMachines redirects the per-machine connection at fake targets, so the
// multi-machine loop runs without SSH. Targets listed in refuse fail to
// connect at all, the way an unreachable or non-GB10 machine would.
func fakeMachines(t *testing.T, machines map[string]*fakeRunner, refuse map[string]string) {
	t.Helper()
	original := connectTarget
	connectTarget = func(_ context.Context, _ WizardUI, target, _ string) (Runner, func(), error) {
		if message, bad := refuse[target]; bad {
			return nil, func() {}, errors.New(message)
		}
		runner, known := machines[target]
		if !known {
			t.Fatalf("unexpected connection to %q", target)
		}
		return runner, func() {}, nil
	}
	t.Cleanup(func() { connectTarget = original })
}

func firstMachine() Machine {
	return Machine{Target: "spark-head", Result: InstallResult{ConsoleURL: "http://192.168.99.134:7070"}}
}

func TestInstallMoreAsksAboutEachMachineAndStopsAtTheFirstNo(t *testing.T) {
	fakeMachines(t, map[string]*fakeRunner{
		"spark-worker": installableRunner("192.168.99.137", "spark-worker"),
	}, nil)
	ui := &stubUI{listenMode: ListenLAN, confirmAlways: []bool{true, false}}

	installed := InstallMore(context.Background(), ui, firstMachine(),
		[]string{"spark-worker", "edgexpert-alpha"}, LocalFileSource{Path: "/tmp/binary"}, "nvidia")

	if len(ui.confirmAlwaysPrompts) != 2 {
		t.Fatalf("asked %d times, want one question per remaining machine: %v", len(ui.confirmAlwaysPrompts), ui.confirmAlwaysPrompts)
	}
	for index, want := range []string{"spark-worker", "edgexpert-alpha"} {
		if !strings.Contains(ui.confirmAlwaysPrompts[index], want) {
			t.Errorf("question %d = %q, want it to name %q", index, ui.confirmAlwaysPrompts[index], want)
		}
	}
	if len(installed) != 2 || installed[1].Target != "spark-worker" {
		t.Fatalf("installed = %+v, want the first machine plus spark-worker", installed)
	}
	if installed[1].Result.ConsoleURL != "http://192.168.99.137:7070" {
		t.Errorf("second machine console = %q", installed[1].Result.ConsoleURL)
	}
	if len(ui.summaries) != 1 {
		t.Errorf("Summary calls = %d, want one for the machine this loop installed", len(ui.summaries))
	}
}

func TestInstallMoreStopsAskingOnceDeclined(t *testing.T) {
	fakeMachines(t, nil, nil) // any connection attempt fails the test
	ui := &stubUI{listenMode: ListenLAN, confirmAlways: []bool{false}}

	installed := InstallMore(context.Background(), ui, firstMachine(),
		[]string{"spark-worker", "edgexpert-alpha"}, LocalFileSource{Path: "/tmp/binary"}, "nvidia")

	if len(ui.confirmAlwaysPrompts) != 1 {
		t.Errorf("asked %d times, want to stop at the first no: %v", len(ui.confirmAlwaysPrompts), ui.confirmAlwaysPrompts)
	}
	if len(installed) != 1 {
		t.Errorf("installed = %+v, want only the machine that was already set up", installed)
	}
	if len(ui.nextSteps) != 1 || !strings.Contains(strings.Join(ui.nextSteps[0], " "), "spark-worker") {
		t.Errorf("next steps = %v, want the declined machine named for later", ui.nextSteps)
	}
}

// A machine that cannot be installed must not take the machines that already
// finished down with it: their consoles are up and their cards were printed.
func TestInstallMoreKeepsEarlierMachinesWhenOneFails(t *testing.T) {
	fakeMachines(t, nil, map[string]string{"spark-worker": "ssh: connection refused"})
	ui := &stubUI{listenMode: ListenLAN, confirmAlways: []bool{true}}

	installed := InstallMore(context.Background(), ui, firstMachine(),
		[]string{"spark-worker"}, LocalFileSource{Path: "/tmp/binary"}, "nvidia")

	if len(installed) != 1 || installed[0].Target != "spark-head" {
		t.Fatalf("installed = %+v, want the first machine still reported", installed)
	}
	if installed[0].Result.ConsoleURL != "http://192.168.99.134:7070" {
		t.Errorf("the first machine's result changed: %+v", installed[0].Result)
	}
	reported := strings.Join(ui.progressLines, "\n")
	if !strings.Contains(reported, "connection refused") {
		t.Errorf("the failure was not reported: %q", reported)
	}
	if len(ui.nextSteps) != 1 || !strings.Contains(strings.Join(ui.nextSteps[0], " "), "spark-worker") {
		t.Errorf("next steps = %v, want the machine that failed named for later", ui.nextSteps)
	}
}

func TestPairingStepsSpellOutTheConsoleProcedure(t *testing.T) {
	steps := PairingSteps([]Machine{
		{Target: "spark-head", Result: InstallResult{ConsoleURL: "http://192.168.99.134:7070"}},
		{Target: "spark-worker", Result: InstallResult{ConsoleURL: "http://192.168.99.137:7070"}},
	}, nil)
	joined := strings.Join(steps, "\n")
	for _, want := range []string{
		"http://192.168.99.134:7070",
		"http://192.168.99.137:7070",
		"Connect tab",
		"Fleet tab",
		"Add a Spark",
		"two Sparks",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("pairing steps do not mention %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "—") {
		t.Errorf("pairing steps use an em dash:\n%s", joined)
	}
}

// Loopback consoles cannot reach each other, so the steps say so instead of
// handing out an address that only works on the machine itself.
func TestPairingStepsFlagALoopbackConsole(t *testing.T) {
	steps := PairingSteps([]Machine{
		{Target: "spark-head", Result: InstallResult{ConsoleURL: "http://127.0.0.1:7070", Loopback: true}},
		{Target: "spark-worker", Result: InstallResult{ConsoleURL: "http://192.168.99.137:7070"}},
	}, nil)
	joined := strings.Join(steps, "\n")
	if !strings.Contains(joined, "loopback") || !strings.Contains(joined, "spark-head") {
		t.Errorf("pairing steps do not warn about the loopback console:\n%s", joined)
	}
}

func TestPairingStepsSayNothingForASingleMachineRun(t *testing.T) {
	steps := PairingSteps([]Machine{firstMachine()}, nil)
	if len(steps) != 0 {
		t.Errorf("steps = %v, want nothing to say", steps)
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
