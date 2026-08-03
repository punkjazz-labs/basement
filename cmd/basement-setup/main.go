// Command basement-setup is the double-clickable installer: no terminal,
// ever. It binds a loopback-only HTTP server (internal/setupweb), opens the
// operator's default browser at the wizard page, and runs the same install
// flow as `basement setup` behind it. Built with -H=windowsgui on Windows so
// no console window appears; on macOS it ships inside a minimal .app bundle
// (packaging, not this binary) so Finder never opens Terminal either.
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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := log.New(os.Stderr, "", log.LstdFlags)
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
