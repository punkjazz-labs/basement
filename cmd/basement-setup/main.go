// Command basement-setup is the double-clickable installer: no terminal,
// ever. It binds a loopback-only HTTP server (internal/setupweb), opens the
// operator's default browser at the wizard page, and runs the same install
// flow as `basement setup` behind it. Built with -H=windowsgui on Windows so
// no console window appears; on macOS it ships inside Basement Setup.app
// (packaging/build-macos-installer.sh, not this binary) so Finder never opens
// Terminal either.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/punkjazz-labs/basement/internal/setupweb"
)

// version is set by the release build (-X main.version=<tag>). It is logged
// rather than displayed: this process has no window of its own, so the log is
// where a support conversation can find it — the system log on macOS, since
// an .app has no terminal attached, and nowhere at all on Windows, where the
// GUI subsystem discards stderr. macOS also carries it in the bundle's
// CFBundleVersion, which Finder shows without running anything.
var version = "dev"

const wizardWorkerArg = "--wizard-worker"

func main() {
	// Launch Services expects the process it starts from an app bundle to
	// service application events for as long as it remains alive. This binary
	// has no Cocoa event loop: its UI is the browser. Leave the Launch Services
	// process immediately and run the long-lived wizard as an ordinary child,
	// so opening the app again never targets an unresponsive application.
	if shouldLaunchWizardWorker(runtime.GOOS, os.Args[1:]) {
		if err := launchWizardWorker(); err != nil {
			fmt.Fprintln(os.Stderr, "basement-setup:", err)
			os.Exit(1)
		}
		return
	}
	runWizard()
}

func shouldLaunchWizardWorker(goos string, args []string) bool {
	return goos == "darwin" && (len(args) == 0 || args[0] != wizardWorkerArg)
}

func launchWizardWorker() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate wizard: %w", err)
	}
	command := exec.Command(executable, wizardWorkerArg)
	if err := command.Start(); err != nil {
		return fmt.Errorf("start wizard: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("release wizard process: %w", err)
	}
	return nil
}

func runWizard() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	logger.Printf("basement-setup %s", version)
	srv, err := setupweb.New(logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "basement-setup:", err)
		os.Exit(1)
	}

	url := srv.URL()
	srv.Start(ctx)
	openBrowser(url)
	srv.Wait(ctx)
}

func openBrowser(url string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	_ = command.Start()
}
