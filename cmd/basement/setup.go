package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/punkjazz-labs/basement/internal/discovery"
	"github.com/punkjazz-labs/basement/internal/setup"
)

// runSetup implements `basement setup`: install on this machine if it is a
// GB10, otherwise discover GB10-class machines on the network, let the
// operator pick the master, and install over SSH. The flow itself
// (discovery, choice, confirmation, listen selection, install, summary)
// lives in internal/setup/wizard.go; this file is the terminal rendering of
// that flow plus flag parsing.
func runSetup(args []string) int {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	host := flags.String("host", "", "skip discovery and install on this host (IP or hostname)")
	sshUser := flags.String("user", setup.DefaultSSHUser(), "SSH user on the target machine")
	listen := flags.String("listen", "", "console interface: loopback, tailscale, lan or lan+tailscale (prompted when empty)")
	binary := flags.String("binary", "", "path to a linux/arm64 manager binary to install (defaults to this binary when compatible, else the latest release)")
	assumeYes := flags.Bool("yes", false, "skip confirmation prompts (still asks for passwords)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	paint := newStyle()
	ui := &terminalUI{
		terminalPrompter: &terminalPrompter{assumeYes: *assumeYes, paint: paint},
		paint:            paint,
		listenFlag:       *listen,
	}

	fmt.Println()
	fmt.Println(paint.bold("basement setup"))

	// On a GB10 machine, setup means: install right here.
	if runtime.GOOS == "linux" && *host == "" {
		local := setup.LocalRunner{}
		identity := setup.Probe(ctx, local)
		if identity.IsGB10() {
			fmt.Printf("%s This machine is a %s — installing locally.\n", paint.green("✓"), paint.bold(identity.Product()))
			executable, err := os.Executable()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s cannot locate this binary: %v\n", paint.red("✗"), err)
				return 1
			}
			if _, err := ui.finishInstall(ctx, local, setup.LocalFileSource{Path: executable}, nil, false); err != nil {
				return 1
			}
			return 0
		}
	}

	// Otherwise: find the machines and install over SSH.
	found := setup.Discovered{Target: *host}
	if found.Target == "" {
		discovered, err := setup.DiscoverAndChoose(ctx, ui)
		if err != nil {
			if errors.Is(err, setup.ErrDeclined) {
				return 1
			}
			fmt.Fprintf(os.Stderr, "%s %v\n", paint.red("✗"), err)
			return 1
		}
		found = discovered
	}
	target := found.Target

	// The account on the GB10 machine is whatever its owner created during
	// first boot (often not the operator's local username), so always ask —
	// unless --user was given explicitly.
	userFlagSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "user" {
			userFlagSet = true
		}
	})
	if !userFlagSet {
		resolved, err := setup.ResolveUsername(ui, target, *sshUser)
		if err != nil {
			return 1
		}
		*sshUser = resolved
	}

	runner, err := setup.ConnectAndVerify(ctx, ui, target, *sshUser)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", paint.red("✗"), err)
		return 1
	}
	defer runner.Close()

	source := setup.PickSource(*binary)
	result, err := ui.finishInstall(ctx, runner, source, found.Peers, true)
	if err != nil {
		return 1
	}
	// Owners of two Sparks should not have to run the installer twice: the
	// other GB10-class machines the sweep found are offered here, one at a
	// time, in the same run.
	setup.InstallMore(ctx, ui, setup.Machine{Target: target, Result: result}, found.Offer, source, *sshUser)
	return 0
}

// terminalUI is the terminal implementation of setup.WizardUI: every prompt
// string, ordering, default and color from before this refactor, now behind
// the interface the wizard flow (internal/setup) drives.
type terminalUI struct {
	*terminalPrompter
	paint      style
	listenFlag string
}

var _ setup.WizardUI = (*terminalUI)(nil)

// ConfirmAlways always puts the question to the operator, even under --yes.
// The only thing it is asked for is whether to install on another machine
// discovery turned up, and that must never be answered by a flag: on a
// shared network the machine next door is somebody else's. With no terminal
// to ask on, the read fails and the flow takes that as "no".
func (u *terminalUI) ConfirmAlways(prompt string) (bool, error) {
	fmt.Println()
	answer, err := u.ask(u.paint.bold(prompt) + " [y/N]: ")
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func (u *terminalUI) ChooseMachine(candidates []discovery.Candidate) (int, error) {
	fmt.Println()
	nameWidth := 0
	for _, candidate := range candidates {
		if length := len(setup.DisplayHost(candidate)); length > nameWidth {
			nameWidth = length
		}
	}
	for index, candidate := range candidates {
		likely := discovery.LikelyGB10Name(candidate.Hostname)
		marker, label := " ", ""
		if likely {
			marker = u.paint.green("●")
			label = u.paint.green("GB10-class")
		}
		line := fmt.Sprintf("  %d  %s %-*s  %-15s  %s", index+1, marker, nameWidth, setup.DisplayHost(candidate), candidate.IP, label)
		if !likely {
			line = u.paint.dim(line)
		}
		fmt.Println(line)
	}
	fmt.Println()

	choice, err := u.ask(u.paint.bold(fmt.Sprintf("Which machine should run basement? [1-%d]: ", len(candidates))))
	if err != nil {
		return 0, err
	}
	index, err := strconv.Atoi(strings.TrimSpace(choice))
	if err != nil || index < 1 || index > len(candidates) {
		return 0, errors.New("not a valid choice")
	}
	return index - 1, nil
}

func (u *terminalUI) ConfirmNonGB10(name string) (bool, error) {
	fmt.Printf("%s %s does not look like a GB10 machine — the %s entries do.\n",
		u.paint.yellow("!"), u.paint.bold(name), u.paint.green("● GB10-class"))
	proceed, _ := u.Confirm("Connect anyway to check its hardware?")
	if !proceed {
		fmt.Println(u.paint.dim("Nothing was installed."))
		return false, setup.ErrDeclined
	}
	return true, nil
}

func (u *terminalUI) AskUsername(target, suggested string) (string, error) {
	answer, err := u.ask(fmt.Sprintf("Username on %s [%s]: ", u.paint.bold(target), suggested))
	if err != nil {
		return "", err
	}
	if answer == "" {
		return suggested, nil
	}
	return answer, nil
}

// ChooseListen asks which network the console should be reachable from.
// Installing from another machine defaults to the local network (that is
// where the operator is). Running locally through SSH has the same default:
// loopback would strand the operator on the computer they connected from.
// A genuinely local session keeps loopback as the conservative default. A
// --listen flag bypasses the prompt.
func (u *terminalUI) ChooseListen(remote bool) (setup.ListenMode, error) {
	switch u.listenFlag {
	case "loopback":
		return setup.ListenLoopback, nil
	case "tailscale":
		return setup.ListenTailscale, nil
	case "lan":
		return setup.ListenLAN, nil
	case "lan+tailscale":
		return setup.ListenLANTailscale, nil
	case "":
	default:
		return "", fmt.Errorf("unknown --listen value %q", u.listenFlag)
	}
	defaultChoice, defaultMode := "1", setup.ListenLoopback
	if remote || runningOverSSH() {
		defaultChoice, defaultMode = "3", setup.ListenLAN
	}
	recommended := func(choice string) string {
		if choice == defaultChoice {
			return " " + u.paint.green("(recommended)")
		}
		return ""
	}
	if u.assumeYes {
		return defaultMode, nil
	}
	fmt.Println()
	fmt.Println(u.paint.bold("Who should be able to open the console?"))
	fmt.Println("  1  Only this machine itself" + recommended("1") + "\n     " + u.paint.dim("Other devices would need an SSH tunnel."))
	fmt.Println("  2  Your Tailscale devices" + recommended("2") + "\n     " + u.paint.dim("Reachable from anywhere via your tailnet."))
	fmt.Println("  3  Any device on your local network" + recommended("3") + "\n     " + u.paint.dim("Phones, laptops — and it opens in your browser when done."))
	fmt.Println("  4  Your local network and Tailscale" + recommended("4"))
	answer, err := u.ask(fmt.Sprintf("Type 1, 2, 3 or 4, or press Enter for %s: ", defaultChoice))
	if err != nil {
		return "", err
	}
	switch strings.TrimSpace(answer) {
	case "1":
		return setup.ListenLoopback, nil
	case "2":
		return setup.ListenTailscale, nil
	case "3":
		return setup.ListenLAN, nil
	case "4":
		return setup.ListenLANTailscale, nil
	case "":
		return defaultMode, nil
	default:
		return "", fmt.Errorf("%q is not one of the choices", strings.TrimSpace(answer))
	}
}

// Progress prints status dim, except a failure the flow chose to carry on
// from (a machine that could not be set up): that one is not a detail to
// skim past.
func (u *terminalUI) Progress(line string) {
	if strings.HasPrefix(line, "✗") {
		fmt.Println(u.paint.red(line))
		return
	}
	fmt.Println(u.paint.dim(line))
}

func (u *terminalUI) Summary(result setup.InstallResult) {
	rule := u.paint.dim(strings.Repeat("─", 62))
	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s %s\n", u.paint.green("✓"), u.paint.bold("basement is running"))
	fmt.Println()
	// One line per bound address, primary first, under a single label: a
	// console that binds the local network and Tailscale is reachable at
	// both, and the card must say so.
	fmt.Printf("    %-14s %s\n", "Console", u.paint.bold(result.ConsoleURL))
	for _, extra := range result.ExtraConsoleURLs() {
		fmt.Printf("    %-14s %s\n", "", u.paint.bold(extra))
	}
	if result.AltURL != "" {
		fmt.Printf("    %-14s %s\n", "Also try", result.AltURL)
	}
	if result.Token != "" {
		fmt.Printf("    %-14s %s\n", "Pairing token", u.paint.bold(result.Token))
	}
	if result.Loopback {
		fmt.Println()
		fmt.Println(u.paint.dim("    Loopback only — from another device use an SSH tunnel, or rerun"))
		fmt.Println(u.paint.dim("    setup and pick the tailscale or lan interface."))
	}
	fmt.Println()
	fmt.Println(u.paint.dim("    Re-print this card on the machine anytime:  basement pairing-url"))
	fmt.Println(rule)
}

// NextSteps prints the closing guidance under the last machine's card, in
// the same plain voice: the headline, then the steps, indented to line up
// with the summary's values.
func (u *terminalUI) NextSteps(lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Println()
	fmt.Printf("  %s\n", u.paint.bold(lines[0]))
	if len(lines) > 1 {
		fmt.Println()
	}
	for _, line := range lines[1:] {
		if line == "" {
			fmt.Println()
			continue
		}
		fmt.Println("    " + line)
	}
	fmt.Println()
}

// finishInstall runs the shared install tail (setup.FinishInstall) and adds
// the one piece that is genuinely terminal-only: opening the operator's
// browser at the finished console once it is reachable from outside. The
// error is already reported when it returns one; callers only decide the
// exit code.
func (u *terminalUI) finishInstall(ctx context.Context, runner setup.Runner, source setup.BinarySource, peers []string, remote bool) (setup.InstallResult, error) {
	result, err := setup.FinishInstall(ctx, u, runner, source, peers, remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", u.paint.red("✗"), err)
		return setup.InstallResult{}, err
	}
	if !result.Loopback {
		if runningOverSSH() {
			fmt.Println(u.paint.dim("  (open " + result.ConsoleURL + " in a browser on the computer you connected from)"))
		} else {
			openBrowser(result.ConsoleURL, u.paint)
		}
	}
	return result, nil
}

func runningOverSSH() bool {
	return os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != ""
}

func openBrowser(url string, paint style) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "linux":
		command = exec.Command("xdg-open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	if err := command.Start(); err == nil {
		fmt.Println(paint.dim("  (opening " + url + " in your browser)"))
	}
}

// style renders ANSI accents only on real terminals that want them.
type style struct{ enabled bool }

func newStyle() style {
	enabled := term.IsTerminal(int(os.Stdout.Fd())) &&
		os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	return style{enabled: enabled}
}

func (s style) wrap(code, text string) string {
	if !s.enabled {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s style) bold(text string) string   { return s.wrap("1", text) }
func (s style) dim(text string) string    { return s.wrap("2", text) }
func (s style) green(text string) string  { return s.wrap("32", text) }
func (s style) yellow(text string) string { return s.wrap("33", text) }
func (s style) red(text string) string    { return s.wrap("31", text) }

// terminalPrompter asks on the controlling terminal. Passwords are read
// without echo and never persisted.
type terminalPrompter struct {
	assumeYes bool
	reader    *bufio.Reader
	paint     style
}

func (p *terminalPrompter) ask(prompt string) (string, error) {
	if p.reader == nil {
		p.reader = bufio.NewReader(os.Stdin)
	}
	fmt.Print(prompt)
	line, err := p.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (p *terminalPrompter) Password(prompt string) (string, error) {
	fmt.Print(prompt)
	secret, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		return "", err
	}
	return string(secret), nil
}

// ServerNotice shows the free text a keyboard-interactive SSH server sends
// with its challenge (a one-time-code hint, a login banner). This is the one
// prompter that asks for it: there is a person at this terminal, they typed
// the password themselves a moment ago, and the text is sanitized before it
// gets here. The console's adoption path implements nothing of the sort, so
// the same text is discarded there rather than written to the journal.
func (p *terminalPrompter) ServerNotice(text string) {
	fmt.Println(p.paint.dim(text))
}

func (p *terminalPrompter) Confirm(prompt string) (bool, error) {
	if p.assumeYes {
		return true, nil
	}
	answer, err := p.ask(prompt + " [y/N]: ")
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}
