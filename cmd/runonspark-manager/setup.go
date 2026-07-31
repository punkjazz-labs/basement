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
	paint := newStyle()
	prompter := &terminalPrompter{assumeYes: *assumeYes, paint: paint}

	fmt.Println()
	fmt.Println(paint.bold("RunOnSpark setup"))

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
			return finishInstall(ctx, local, setup.LocalFileSource{Path: executable}, prompter, *listen, nil, paint, false)
		}
	}

	// Otherwise: find the machines and install over SSH.
	target := *host
	var peers []string
	if target == "" {
		fmt.Println(paint.dim("Scanning your network for GB10 machines (DGX Spark, ASUS Ascent GX10, MSI EdgeXpert, …)"))
		candidates, err := discovery.Discover(ctx, func(string, ...any) {})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s discovery failed: %v\n", paint.red("✗"), err)
			return 1
		}
		if len(candidates) == 0 {
			fmt.Fprintf(os.Stderr, "%s No SSH-reachable machines found. Is the GB10 machine on this network?\n  You can also point setup directly:  runonspark-manager setup --host <ip>\n", paint.red("✗"))
			return 1
		}

		fmt.Println()
		nameWidth := 0
		for _, candidate := range candidates {
			if length := len(displayHost(candidate)); length > nameWidth {
				nameWidth = length
			}
		}
		for index, candidate := range candidates {
			likely := discovery.LikelyGB10Name(candidate.Hostname)
			marker, label := " ", ""
			if likely {
				marker = paint.green("●")
				label = paint.green("GB10-class")
			}
			line := fmt.Sprintf("  %d  %s %-*s  %-15s  %s", index+1, marker, nameWidth, displayHost(candidate), candidate.IP, label)
			if !likely {
				line = paint.dim(line)
			}
			fmt.Println(line)
		}
		fmt.Println()

		choice, err := prompter.ask(paint.bold(fmt.Sprintf("Which machine should run RunOnSpark Manager? [1-%d]: ", len(candidates))))
		if err != nil {
			return 1
		}
		index, err := strconv.Atoi(strings.TrimSpace(choice))
		if err != nil || index < 1 || index > len(candidates) {
			fmt.Fprintf(os.Stderr, "%s not a valid choice\n", paint.red("✗"))
			return 1
		}
		picked := candidates[index-1]

		// Stop the obvious mistake before any connection: hostname hints are
		// not proof, so a custom-named GB10 can still proceed deliberately —
		// but the default answer is no.
		if !discovery.LikelyGB10Name(picked.Hostname) {
			fmt.Printf("%s %s does not look like a GB10 machine — the %s entries do.\n",
				paint.yellow("!"), paint.bold(displayHost(picked)), paint.green("● GB10-class"))
			proceed, err := prompter.Confirm("Connect anyway to check its hardware?")
			if err != nil || !proceed {
				fmt.Println(paint.dim("Nothing was installed."))
				return 1
			}
		}

		target = picked.DisplayName()
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
		answer, err := prompter.ask(fmt.Sprintf("Username on %s [%s]: ", paint.bold(target), *sshUser))
		if err != nil {
			return 1
		}
		if answer != "" {
			*sshUser = answer
		}
	}

	fmt.Printf("%s Connecting to %s…\n", paint.dim("→"), paint.bold(*sshUser+"@"+target))
	runner, err := setup.DialSSH(ctx, target, *sshUser, prompter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", paint.red("✗"), err)
		return 1
	}
	defer runner.Close()

	identity := setup.Probe(ctx, runner)
	if !identity.IsGB10() {
		gpu := identity.GPUName
		if gpu == "" {
			gpu = "none detected"
		}
		fmt.Fprintf(os.Stderr, "%s %s is not a GB10 machine (GPU: %s) — RunOnSpark recipes are built for the GB10 superchip, so setup will not install here.\n", paint.red("✗"), target, gpu)
		return 1
	}
	descriptor := identity.Hostname
	if identity.OSName != "" {
		descriptor += ", " + identity.OSName
	}
	fmt.Printf("%s Confirmed: %s (%s)\n", paint.green("✓"), paint.bold(identity.Product()), descriptor)

	source := pickSource(*binary)
	return finishInstall(ctx, runner, source, prompter, *listen, peers, paint, true)
}

func displayHost(candidate discovery.Candidate) string {
	return strings.TrimSuffix(candidate.DisplayName(), ".local")
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

func finishInstall(ctx context.Context, runner setup.Runner, source setup.BinarySource, prompter *terminalPrompter, listenFlag string, peers []string, paint style, remote bool) int {
	mode, err := chooseListen(prompter, listenFlag, paint, remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", paint.red("✗"), err)
		return 1
	}
	result, err := setup.Install(ctx, runner, source, setup.Options{Listen: mode, DiscoveredPeers: peers}, func(format string, args ...any) {
		fmt.Println(paint.dim("  · " + fmt.Sprintf(format, args...)))
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s install failed: %v\n", paint.red("✗"), err)
		return 1
	}

	rule := paint.dim(strings.Repeat("─", 62))
	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s %s\n", paint.green("✓"), paint.bold("RunOnSpark Manager is running"))
	fmt.Println()
	fmt.Printf("    %-14s %s\n", "Console", paint.bold(result.ConsoleURL))
	if result.AltURL != "" {
		fmt.Printf("    %-14s %s\n", "Also try", result.AltURL)
	}
	if result.Token != "" {
		fmt.Printf("    %-14s %s\n", "Pairing token", paint.bold(result.Token))
	}
	if result.Loopback {
		fmt.Println()
		fmt.Println(paint.dim("    Loopback only — from another device use an SSH tunnel, or rerun"))
		fmt.Println(paint.dim("    setup and pick the tailscale or lan interface."))
	}
	fmt.Println()
	fmt.Println(paint.dim("    Re-print this card on the machine anytime:  runonspark-manager pairing-url"))
	fmt.Println(rule)

	if !result.Loopback {
		openBrowser(result.ConsoleURL, paint)
	}
	return 0
}

// chooseListen asks which network the console should be reachable from.
// Installing from another machine defaults to the local network (that is
// where the operator is); installing on the machine itself defaults to
// loopback, the conservative choice.
func chooseListen(prompter *terminalPrompter, listenFlag string, paint style, remote bool) (setup.ListenMode, error) {
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
	defaultChoice, defaultMode := "1", setup.ListenLoopback
	if remote {
		defaultChoice, defaultMode = "3", setup.ListenLAN
	}
	recommended := func(choice string) string {
		if choice == defaultChoice {
			return " " + paint.green("(recommended)")
		}
		return ""
	}
	if prompter.assumeYes {
		return defaultMode, nil
	}
	fmt.Println()
	fmt.Println(paint.bold("Who should be able to open the console?"))
	fmt.Println("  1  Only this machine itself" + recommended("1") + "\n     " + paint.dim("Other devices would need an SSH tunnel."))
	fmt.Println("  2  Your Tailscale devices" + recommended("2") + "\n     " + paint.dim("Reachable from anywhere via your tailnet."))
	fmt.Println("  3  Any device on your local network" + recommended("3") + "\n     " + paint.dim("Phones, laptops — and it opens in your browser when done."))
	answer, err := prompter.ask(fmt.Sprintf("Type 1, 2 or 3 — or press Enter for %s: ", defaultChoice))
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
	case "":
		return defaultMode, nil
	default:
		return "", fmt.Errorf("%q is not one of the choices", strings.TrimSpace(answer))
	}
}

func defaultSSHUser() string {
	if current, err := user.Current(); err == nil && current.Username != "" {
		// Windows reports DOMAIN\name; only the name part is a useful default.
		name := current.Username
		if index := strings.LastIndexByte(name, '\\'); index >= 0 {
			name = name[index+1:]
		}
		return name
	}
	return "nvidia"
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
