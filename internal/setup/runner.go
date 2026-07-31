// Package setup installs RunOnSpark Manager onto a GB10-class machine, either
// the one it is running on or a remote one over SSH. Discovery finds the
// machines; this package verifies identity and performs the install.
package setup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Runner executes shell commands on the target machine. Implementations exist
// for the local host and for SSH; the install engine is identical over both.
type Runner interface {
	// Run executes a command as the connecting user.
	Run(ctx context.Context, command string, stdin io.Reader) (stdout string, err error)
	// RunPrivileged executes a command as root (sudo when necessary).
	RunPrivileged(ctx context.Context, command string, stdin io.Reader) (stdout string, err error)
	// Describe names the target for user-facing messages.
	Describe() string
}

// LocalRunner executes on this machine, using sudo when not already root.
type LocalRunner struct{}

func (LocalRunner) Describe() string { return "this machine" }

func (LocalRunner) Run(ctx context.Context, command string, stdin io.Reader) (string, error) {
	return runShell(ctx, []string{"sh", "-c", command}, stdin)
}

func (LocalRunner) RunPrivileged(ctx context.Context, command string, stdin io.Reader) (string, error) {
	if os.Geteuid() == 0 {
		return runShell(ctx, []string{"sh", "-c", command}, stdin)
	}
	// Interactive sudo: the terminal handles the password prompt.
	return runShell(ctx, []string{"sudo", "sh", "-c", command}, stdin)
}

func runShell(ctx context.Context, argv []string, stdin io.Reader) (string, error) {
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Stdin = stdin
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	// sudo may need the terminal for its password prompt.
	if argv[0] == "sudo" {
		command.Stderr = io.MultiWriter(&stderr, os.Stderr)
	}
	if err := command.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w (%s)", argv[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
