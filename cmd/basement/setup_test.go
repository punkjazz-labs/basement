package main

import (
	"bufio"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/setup"
)

func scriptedUI(answers string) *terminalUI {
	return &terminalUI{terminalPrompter: &terminalPrompter{
		assumeYes: true,
		reader:    bufio.NewReader(strings.NewReader(answers)),
	}}
}

// --yes exists to skip the confirmations of the install the operator asked
// for. It must never answer the one question that reaches out to a second
// machine: on a shared network that machine can belong to somebody else.
func TestAssumeYesNeverAcceptsAnotherMachineByItself(t *testing.T) {
	ui := scriptedUI("n\n")
	if proceed, err := ui.Confirm("Trust this host key?"); err != nil || !proceed {
		t.Fatalf("Confirm under --yes = %v, %v; want an automatic yes", proceed, err)
	}
	proceed, err := ui.ConfirmAlways("Set up spark-worker as well?")
	if err != nil {
		t.Fatal(err)
	}
	if proceed {
		t.Error("--yes auto-accepted installing on another discovered machine")
	}
}

func TestConfirmAlwaysTakesTheTypedAnswer(t *testing.T) {
	if proceed, err := scriptedUI("y\n").ConfirmAlways("Set up spark-worker as well?"); err != nil || !proceed {
		t.Errorf("ConfirmAlways(y) = %v, %v; want true", proceed, err)
	}
	if proceed, err := scriptedUI("\n").ConfirmAlways("Set up spark-worker as well?"); err != nil || proceed {
		t.Errorf("ConfirmAlways(enter) = %v, %v; want the no default", proceed, err)
	}
	// No terminal to ask on: the read fails and the flow reads that as no.
	if proceed, err := scriptedUI("").ConfirmAlways("Set up spark-worker as well?"); err == nil || proceed {
		t.Errorf("ConfirmAlways with closed input = %v, %v; want an error and no", proceed, err)
	}
}

func TestListenChoiceTreatsAnSSHSessionAsRemote(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "192.0.2.20 50123 192.0.2.30 22")
	ui := scriptedUI("")
	mode, err := ui.ChooseListen(false)
	if err != nil {
		t.Fatal(err)
	}
	if mode != setup.ListenLAN {
		t.Errorf("ChooseListen in an SSH session = %q, want lan", mode)
	}
}

// Four choices, and the typed number decides. Choice 4 is the new one: the
// console binds the local network and Tailscale at the same time.
func TestListenChoiceMapsEveryTypedNumber(t *testing.T) {
	for answer, want := range map[string]setup.ListenMode{
		"1": setup.ListenLoopback,
		"2": setup.ListenTailscale,
		"3": setup.ListenLAN,
		"4": setup.ListenLANTailscale,
	} {
		ui := scriptedUI(answer + "\n")
		ui.assumeYes = false
		mode, err := ui.ChooseListen(false)
		if err != nil {
			t.Errorf("ChooseListen(%q) failed: %v", answer, err)
			continue
		}
		if mode != want {
			t.Errorf("ChooseListen(%q) = %q, want %q", answer, mode, want)
		}
	}
	ui := scriptedUI("5\n")
	ui.assumeYes = false
	if mode, err := ui.ChooseListen(false); err == nil {
		t.Errorf("ChooseListen(\"5\") = %q, want an error", mode)
	}
}

// --listen names the same four modes without a prompt.
func TestListenFlagMapsEveryMode(t *testing.T) {
	for flag, want := range map[string]setup.ListenMode{
		"loopback":      setup.ListenLoopback,
		"tailscale":     setup.ListenTailscale,
		"lan":           setup.ListenLAN,
		"lan+tailscale": setup.ListenLANTailscale,
	} {
		ui := scriptedUI("")
		ui.listenFlag = flag
		mode, err := ui.ChooseListen(false)
		if err != nil || mode != want {
			t.Errorf("--listen %s = %q, %v; want %q", flag, mode, err, want)
		}
	}
	ui := scriptedUI("")
	ui.listenFlag = "lan+carrier-pigeon"
	if mode, err := ui.ChooseListen(false); err == nil {
		t.Errorf("--listen lan+carrier-pigeon = %q, want an error", mode)
	}
}

func TestListenChoiceKeepsLoopbackForAGenuinelyLocalSession(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_CLIENT", "")
	t.Setenv("SSH_TTY", "")
	ui := scriptedUI("")
	mode, err := ui.ChooseListen(false)
	if err != nil {
		t.Fatal(err)
	}
	if mode != setup.ListenLoopback {
		t.Errorf("ChooseListen in a local session = %q, want loopback", mode)
	}
}
