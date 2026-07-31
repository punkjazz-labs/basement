package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/punkjazz-labs/runonspark-manager/internal/discovery"
	"github.com/punkjazz-labs/runonspark-manager/internal/setup"
)

// runSetup implements `runonspark-manager setup`: install on this machine if
// it is a GB10, otherwise discover GB10-class machines on the network, let
// the operator pick the master, and install over SSH.
func runSetup(args []string) int {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	host := flags.String("host", "", "skip discovery and install on this host (IP or hostname)")
	sshUser := flags.String("user", defaultSSHUser(), "SSH user on the target machine")
	listen := flags.String("listen", "", "console interface: loopback, tailscale or lan (prompted when empty)")
	binary := flags.String("binary", "", "path to a linux/arm64 manager binary to install (defaults to this binary when compatible, else the latest release)")
	assumeYes := flags.Bool("yes", false, "skip confirmation prompts (still asks for passwords)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	prompter := &terminalPrompter{assumeYes: *assumeYes}

	// On a GB10 machine, setup means: install right here.
	if runtime.GOOS == "linux" && *host == "" {
		local := setup.LocalRunner{}
		identity := setup.Probe(ctx, local)
		if identity.IsGB10() {
			fmt.Printf("This machine is a %s — installing RunOnSpark Manager locally.\n", identity.Product())
			executable, err := os.Executable()
			if err != nil {
				fmt.Fprintln(os.Stderr, "cannot locate this binary:", err)
				return 1
			}
			return finishInstall(ctx, local, setup.LocalFileSource{Path: executable}, prompter, *listen, nil)
		}
	}

	// Otherwise: find the machines and install over SSH.
	target := *host
	var peers []string
	if target == "" {
		fmt.Println("Looking for GB10 machines on your network (DGX Spark, ASUS Ascent GX10, MSI EdgeXpert, …)")
		candidates, err := discovery.Discover(ctx, func(format string, args ...any) {
			fmt.Printf("  "+format+"\n", args...)
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "discovery failed:", err)
			return 1
		}
		if len(candidates) == 0 {
			fmt.Fprintln(os.Stderr, "No SSH-reachable machines found. Is the GB10 machine on this network? You can also point setup directly: runonspark-manager setup --host <ip>")
			return 1
		}
		fmt.Println()
		fmt.Println("Machines found:")
		for index, candidate := range candidates {
			marker := " "
			if discovery.LikelyGB10Name(candidate.Hostname) {
				marker = "★" // likely GB10 by hostname; confirmed after connecting
			}
			fmt.Printf("  %d) %s %s (%s)\n", index+1, marker, candidate.DisplayName(), candidate.IP)
		}
		fmt.Println()
		choice, err := prompter.ask(fmt.Sprintf("Which machine should run RunOnSpark Manager (the master)? [1-%d]: ", len(candidates)))
		if err != nil {
			return 1
		}
		index, err := strconv.Atoi(strings.TrimSpace(choice))
		if err != nil || index < 1 || index > len(candidates) {
			fmt.Fprintln(os.Stderr, "not a valid choice")
			return 1
		}
		target = candidates[index-1].DisplayName()
		for position, candidate := range candidates {
			if position != index-1 {
				peers = append(peers, candidate.DisplayName())
			}
		}
	}

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
		answer, err := prompter.ask(fmt.Sprintf("Username on %s [%s]: ", target, *sshUser))
		if err != nil {
			return 1
		}
		if answer != "" {
			*sshUser = answer
		}
	}

	fmt.Printf("Connecting to %s@%s…\n", *sshUser, target)
	runner, err := setup.DialSSH(ctx, target, *sshUser, prompter)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer runner.Close()

	identity := setup.Probe(ctx, runner)
	if !identity.IsGB10() {
		gpu := identity.GPUName
		if gpu == "" {
			gpu = "none detected"
		}
		fmt.Fprintf(os.Stderr, "%s is not a GB10 machine (GPU: %s) — RunOnSpark recipes are built for the GB10 superchip, so setup will not install here.\n", target, gpu)
		return 1
	}
	fmt.Printf("Confirmed: %s (%s, %s)\n", identity.Product(), identity.Hostname, identity.OSName)

	source := pickSource(*binary)
	return finishInstall(ctx, runner, source, prompter, *listen, peers)
}

// pickSource chooses how the linux/arm64 binary reaches the target.
func pickSource(binaryFlag string) setup.BinarySource {
	if binaryFlag != "" {
		return setup.UploadSource{Path: binaryFlag}
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == "arm64" {
		if executable, err := os.Executable(); err == nil {
			return setup.UploadSource{Path: executable}
		}
	}
	return setup.ReleaseSource{}
}

func finishInstall(ctx context.Context, runner setup.Runner, source setup.BinarySource, prompter *terminalPrompter, listenFlag string, peers []string) int {
	mode, err := chooseListen(prompter, listenFlag)
	if err != nil {
		return 1
	}
	result, err := setup.Install(ctx, runner, source, setup.Options{Listen: mode, DiscoveredPeers: peers}, func(format string, args ...any) {
		fmt.Printf("  "+format+"\n", args...)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "install failed:", err)
		return 1
	}

	fmt.Println()
	fmt.Println("==================================================================")
	fmt.Println("  RunOnSpark Manager is running.")
	fmt.Println()
	fmt.Printf("  Open the console:  %s\n", result.ConsoleURL)
	if result.AltURL != "" {
		fmt.Printf("  Also try:          %s\n", result.AltURL)
	}
	if result.Loopback {
		fmt.Println("  (loopback only — from another device use an SSH tunnel, or rerun")
		fmt.Println("   setup and pick the tailscale or lan interface)")
	}
	if result.Token != "" {
		fmt.Println()
		fmt.Printf("  Pairing token:     %s\n", result.Token)
	}
	fmt.Println()
	fmt.Println("  Re-print this card on the machine anytime with:")
	fmt.Println("    " + "/usr/lib/runonspark-manager/runonspark-manager pairing-url")
	fmt.Println("==================================================================")

	if !result.Loopback {
		openBrowser(result.ConsoleURL)
	}
	return 0
}

// chooseListen mirrors install.sh's interface prompt.
func chooseListen(prompter *terminalPrompter, listenFlag string) (setup.ListenMode, error) {
	switch listenFlag {
	case "loopback":
		return setup.ListenLoopback, nil
	case "tailscale":
		return setup.ListenTailscale, nil
	case "lan":
		return setup.ListenLAN, nil
	case "":
	default:
		return "", fmt.Errorf("unknown --listen value %q", listenFlag)
	}
	if prompter.assumeYes {
		return setup.ListenLoopback, nil
	}
	fmt.Println()
	fmt.Println("Where should the RunOnSpark console be reachable?")
	fmt.Println("  1) The machine itself only (127.0.0.1) [default]")
	fmt.Println("  2) Your Tailscale network")
	fmt.Println("  3) Your local network")
	answer, err := prompter.ask("Choice [1]: ")
	if err != nil {
		return "", err
	}
	switch strings.TrimSpace(answer) {
	case "2":
		return setup.ListenTailscale, nil
	case "3":
		return setup.ListenLAN, nil
	default:
		return setup.ListenLoopback, nil
	}
}

func defaultSSHUser() string {
	if current, err := user.Current(); err == nil && current.Username != "" {
		return current.Username
	}
	return "nvidia"
}

func openBrowser(url string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "linux":
		command = exec.Command("xdg-open", url)
	default:
		return
	}
	if err := command.Start(); err == nil {
		fmt.Printf("  (opening %s in your browser)\n", url)
	}
}

// terminalPrompter asks on the controlling terminal. Passwords are read
// without echo and never persisted.
type terminalPrompter struct {
	assumeYes bool
	reader    *bufio.Reader
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
