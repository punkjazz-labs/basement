package setup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// silentPrompter is the console's adoption prompter in miniature: it answers
// every question with the one typed password and prints nothing itself, so
// anything that reaches stdout during a dial got there from DialSSH.
type silentPrompter struct {
	password string
	mu       sync.Mutex
	prompts  []string
}

func (p *silentPrompter) Password(prompt string) (string, error) {
	p.mu.Lock()
	p.prompts = append(p.prompts, prompt)
	p.mu.Unlock()
	return p.password, nil
}

func (p *silentPrompter) Confirm(string) (bool, error) { return true, nil }

func (p *silentPrompter) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...)
}

// hostileKeyboardInteractiveServer is an SSH server that offers only
// keyboard-interactive auth, asks for the password, and then sends a second
// challenge whose instruction echoes that password straight back at the
// client. A real server has no reason to do this; a hostile one does, because
// anything the client prints goes to the systemd journal of the machine
// running the adoption.
func hostileKeyboardInteractiveServer(t *testing.T, password string) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		KeyboardInteractiveCallback: func(_ ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			// An escape sequence in the question too: server text is server
			// text wherever it turns up.
			answers, err := challenge("", "", []string{"\x1b[2JPassword: "}, []bool{false})
			if err != nil || len(answers) != 1 {
				return nil, errors.New("no answer")
			}
			// The attack: the instruction is free text the server controls.
			if _, err := challenge("", "verification of "+answers[0]+" failed, retrying", nil, nil); err != nil {
				return nil, err
			}
			if answers[0] != password {
				return nil, errors.New("denied")
			}
			return &ssh.Permissions{}, nil
		},
	}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				serverConn, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer serverConn.Close()
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					_ = channel.Reject(ssh.Prohibited, "nothing to run here")
				}
			}()
		}
	}()
	return listener.Addr().String()
}

// captureStdout redirects os.Stdout for the duration of the test and returns
// everything written to it.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	captured := make(chan string, 1)
	go func() {
		payload, _ := io.ReadAll(reader)
		captured <- string(payload)
	}()
	var once sync.Once
	var result string
	read := func() string {
		once.Do(func() {
			os.Stdout = original
			writer.Close()
			result = <-captured
			reader.Close()
		})
		return result
	}
	t.Cleanup(func() { read() })
	return read
}

// A keyboard-interactive server chooses every word of its instructions, so
// nothing on the adoption path may print them: the password the owner typed
// into the console must not reach the journal by way of a server that echoes
// it back.
func TestKeyboardInteractiveNeverPrintsServerText(t *testing.T) {
	const password = "correct-horse-battery-staple"
	// Keep the dial off this developer's real agent, keys and known_hosts.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SSH_AUTH_SOCK", "")

	address := hostileKeyboardInteractiveServer(t, password)
	prompter := &silentPrompter{password: password}
	output := captureStdout(t)
	runner, err := DialSSH(context.Background(), address, "nvidia", prompter)
	printed := output()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	runner.Close()

	if strings.Contains(printed, password) {
		t.Fatalf("the SSH password was printed to stdout: %q", printed)
	}
	if strings.Contains(printed, "verification of") {
		t.Errorf("server-supplied instruction text was printed: %q", printed)
	}
	if printed != "" {
		t.Errorf("a non-interactive prompter saw output it never asked for: %q", printed)
	}
	prompts := prompter.seen()
	if len(prompts) == 0 {
		t.Fatal("the keyboard-interactive challenge never reached the prompter")
	}
	for _, prompt := range prompts {
		if strings.Contains(prompt, "\x1b") {
			t.Errorf("server-supplied question kept its escape sequences: %q", prompt)
		}
	}
}

// A prompter with a person in front of it opts in, and still only ever sees
// text that has been stripped of control characters and capped.
func TestServerNoticeReachesOnlyPromptersThatAskForIt(t *testing.T) {
	long := strings.Repeat("a", serverTextLimit*2)
	cases := []struct{ raw, want string }{
		{"  please enter your token  ", "please enter your token"},
		{"\x1b[2Jbanner\x07", "[2Jbanner"},
		{"one\ntwo", "one two"},
		{"", ""},
	}
	for _, test := range cases {
		if got := sanitizeServerText(test.raw); got != test.want {
			t.Errorf("sanitizeServerText(%q) = %q, want %q", test.raw, got, test.want)
		}
	}
	if got := sanitizeServerText(long); len(got) > serverTextLimit {
		t.Errorf("sanitizeServerText did not cap the length: %d characters", len(got))
	}
	// The console's prompter is deliberately not a ServerNotice.
	var prompter Prompter = &silentPrompter{}
	if _, ok := prompter.(ServerNotice); ok {
		t.Error("a non-interactive prompter must not receive server text")
	}
}

func TestChangedHostKeyStopsWithFingerprintAndScopedRecovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(home+"/.ssh", 0o700); err != nil {
		t.Fatal(err)
	}
	_, oldPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := ssh.NewSignerFromKey(oldPrivate)
	if err != nil {
		t.Fatal(err)
	}
	knownLine := knownhosts.Line([]string{"spark-test.local"}, oldKey.PublicKey()) + "\n"
	if err := os.WriteFile(home+"/.ssh/known_hosts", []byte(knownLine), 0o600); err != nil {
		t.Fatal(err)
	}

	_, newPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := ssh.NewSignerFromKey(newPrivate)
	if err != nil {
		t.Fatal(err)
	}
	callback, err := hostKeyCallback(&silentPrompter{})
	if err != nil {
		t.Fatal(err)
	}
	err = callback("spark-test.local:22", &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 22}, newKey.PublicKey())
	if err == nil {
		t.Fatal("a changed host key was accepted")
	}
	message := err.Error()
	for _, want := range []string{
		"SSH host key for spark-test.local changed",
		ssh.FingerprintSHA256(newKey.PublicKey()),
		"Verify that fingerprint directly",
		"ssh-keygen -R 'spark-test.local'",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("changed-key error does not contain %q:\n%s", want, message)
		}
	}
}
