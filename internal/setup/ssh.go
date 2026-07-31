package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Prompter supplies the interactive pieces of an SSH connection so the engine
// stays testable. Secrets are used for the live connection only and are never
// persisted anywhere.
type Prompter interface {
	Password(prompt string) (string, error)
	Confirm(prompt string) (bool, error)
}

// SSHRunner executes commands on a remote machine over SSH.
type SSHRunner struct {
	client       *ssh.Client
	target       string
	prompter     Prompter
	sudoPassword string
	sudoChecked  bool
	sudoNeedsPW  bool
}

func (r *SSHRunner) Describe() string { return r.target }

// DialSSH connects to addr as user. Authentication order: SSH agent, default
// key files (passphrase prompted when needed), then password — mirroring what
// plain `ssh` would try. Host keys follow known_hosts with a
// trust-on-first-use prompt that appends accepted keys.
func DialSSH(ctx context.Context, addr, user string, prompter Prompter) (*SSHRunner, error) {
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, "22")
	}
	hostKey, err := hostKeyCallback(prompter)
	if err != nil {
		return nil, err
	}
	var methods []ssh.AuthMethod
	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		if conn, err := net.Dial("unix", socket); err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	if signers := defaultKeySigners(prompter); len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}
	methods = append(methods, ssh.RetryableAuthMethod(ssh.PasswordCallback(func() (string, error) {
		return prompter.Password(fmt.Sprintf("%s@%s password: ", user, addr))
	}), 3))
	// Some sshd configurations offer keyboard-interactive instead of plain
	// password auth; answer its prompts the same way.
	methods = append(methods, ssh.RetryableAuthMethod(ssh.KeyboardInteractive(
		func(_, instruction string, questions []string, echos []bool) ([]string, error) {
			if instruction != "" {
				fmt.Println(strings.TrimSpace(instruction))
			}
			answers := make([]string, len(questions))
			for index, question := range questions {
				prompt := strings.TrimSpace(question)
				if prompt == "" {
					prompt = fmt.Sprintf("%s@%s response", user, addr)
				}
				answer, err := prompter.Password(prompt + " ")
				if err != nil {
					return nil, err
				}
				answers[index] = answer
			}
			return answers, nil
		}), 3))

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            methods,
		HostKeyCallback: hostKey,
		Timeout:         10 * time.Second,
	}
	dialer := net.Dialer{Timeout: config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	clientConn, channels, requests, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh to %s: %w", addr, err)
	}
	return &SSHRunner{client: ssh.NewClient(clientConn, channels, requests), target: fmt.Sprintf("%s@%s", user, addr), prompter: prompter}, nil
}

func (r *SSHRunner) Close() error { return r.client.Close() }

func (r *SSHRunner) Run(ctx context.Context, command string, stdin io.Reader) (string, error) {
	return r.exec(ctx, command, stdin)
}

// RunPrivileged wraps the command in sudo. Passwordless sudo is probed once;
// when a password is required it is prompted a single time and replayed over
// stdin (`sudo -S`) for subsequent privileged steps, held in memory only.
func (r *SSHRunner) RunPrivileged(ctx context.Context, command string, stdin io.Reader) (string, error) {
	if !r.sudoChecked {
		if _, err := r.exec(ctx, "sudo -n true", nil); err != nil {
			r.sudoNeedsPW = true
		}
		r.sudoChecked = true
	}
	if !r.sudoNeedsPW {
		return r.exec(ctx, "sudo -n sh -c "+shellQuote(command), stdin)
	}
	if r.sudoPassword == "" {
		password, err := r.prompter.Password(fmt.Sprintf("sudo password on %s: ", r.target))
		if err != nil {
			return "", err
		}
		r.sudoPassword = password
	}
	input := io.MultiReader(strings.NewReader(r.sudoPassword+"\n"), nonNil(stdin))
	return r.exec(ctx, "sudo -S -p '' sh -c "+shellQuote(command), input)
}

func (r *SSHRunner) exec(ctx context.Context, command string, stdin io.Reader) (string, error) {
	session, err := r.client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	if stdin != nil {
		session.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		session.Signal(ssh.SIGKILL)
		return stdout.String(), ctx.Err()
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return stdout.String(), fmt.Errorf("remote command failed: %s", detail)
	}
	return stdout.String(), nil
}

// defaultKeySigners loads the usual private keys, prompting for passphrases
// only when a key is actually encrypted.
func defaultKeySigners(prompter Prompter) []ssh.Signer {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var signers []ssh.Signer
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		path := filepath.Join(home, ".ssh", name)
		payload, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(payload)
		if err == nil {
			signers = append(signers, signer)
			continue
		}
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			passphrase, promptErr := prompter.Password(fmt.Sprintf("passphrase for %s (empty to skip): ", path))
			if promptErr != nil || passphrase == "" {
				continue
			}
			if signer, err := ssh.ParsePrivateKeyWithPassphrase(payload, []byte(passphrase)); err == nil {
				signers = append(signers, signer)
			}
		}
	}
	return signers
}

// hostKeyCallback verifies against ~/.ssh/known_hosts and offers
// trust-on-first-use for unknown hosts, appending accepted keys.
func hostKeyCallback(prompter Prompter) (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	knownPath := filepath.Join(home, ".ssh", "known_hosts")
	if err := os.MkdirAll(filepath.Dir(knownPath), 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(knownPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(knownPath, nil, 0o600); err != nil {
			return nil, err
		}
	}
	verify, err := knownhosts.New(knownPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", knownPath, err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := verify(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) || len(keyErr.Want) > 0 {
			// A mismatched recorded key is never auto-accepted.
			return err
		}
		accept, promptErr := prompter.Confirm(fmt.Sprintf(
			"The authenticity of %s can't be established.\n%s key fingerprint is %s.\nTrust this machine and continue?",
			hostname, key.Type(), ssh.FingerprintSHA256(key)))
		if promptErr != nil {
			return promptErr
		}
		if !accept {
			return fmt.Errorf("host key for %s was not accepted", hostname)
		}
		line := knownhosts.Line([]string{hostname}, key)
		file, err := os.OpenFile(knownPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = fmt.Fprintln(file, line)
		return err
	}, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func nonNil(reader io.Reader) io.Reader {
	if reader == nil {
		return strings.NewReader("")
	}
	return reader
}
