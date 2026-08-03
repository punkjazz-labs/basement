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

func main() {
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
