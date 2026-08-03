package main

import (
	"bufio"
	"strings"
	"testing"
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
