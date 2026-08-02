package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/punkjazz-labs/runonspark-manager/internal/auth"
	"github.com/punkjazz-labs/runonspark-manager/internal/config"
	"github.com/punkjazz-labs/runonspark-manager/internal/engine"
	"github.com/punkjazz-labs/runonspark-manager/internal/httpapi"
	"github.com/punkjazz-labs/runonspark-manager/internal/inventory"
	"github.com/punkjazz-labs/runonspark-manager/internal/operations"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipefeed"
	"github.com/punkjazz-labs/runonspark-manager/internal/store"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Parse(version)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		exit(2)
	}
	if cfg.Command == "version" {
		fmt.Println(cfg.Version)
		exit(0)
	}
	if cfg.Command == "pairing-url" {
		printPairingInfo(cfg)
		exit(0)
	}
	if cfg.Command == "setup" {
		exit(runSetup(flag.Args()[1:]))
	}
	if cfg.Command != "" {
		logger.Error("unknown command", "command", cfg.Command)
		exit(2)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		logger.Error("create data directory", "error", err)
		exit(1)
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "manager.db"))
	if err != nil {
		logger.Error("open state database", "error", err)
		exit(1)
	}
	defer db.Close()
	authManager, err := auth.Open(cfg.DataDir)
	if err != nil {
		logger.Error("initialize local identity", "error", err)
		exit(1)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		logger.Error("load recipes", "error", err)
		exit(1)
	}
	provider := inventory.Host{DataDir: cfg.DataDir, DockerSocket: "/var/run/docker.sock"}
	executor := operations.NewHostExecutor(cfg.DataDir, "/var/run/docker.sock", provider)
	jobEngine := engine.New(db, executor, recipes)

	// The recipe feed seeds from the embedded set plus whatever verified
	// cache exists on disk (no network involved, so this never blocks or
	// fails startup) and only then starts fetching in the background: the
	// embedded recipes remain the permanent offline floor either way.
	feed := recipefeed.NewFetcher(recipes, cfg.DataDir, logger)
	cachedAll, cachedEffective := feed.Snapshot()
	jobEngine.SetRecipes(cachedAll, cachedEffective)

	if err := jobEngine.ResumeInterrupted(context.Background()); err != nil {
		logger.Error("resume interrupted jobs", "error", err)
		exit(1)
	}
	if err := jobEngine.ReconcileActiveModel(context.Background()); err != nil {
		logger.Error("reconcile active model", "error", err)
		exit(1)
	}
	api := httpapi.New(cfg.Version, cfg.DataDir, authManager, db, provider, executor, jobEngine, cachedEffective)
	api.SetRecipes(cachedAll, cachedEffective)
	server := &http.Server{Addr: cfg.Listen, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20}
	// Progress streams stay open indefinitely by design; without this hook a
	// restart waits out the whole drain timeout whenever a console is open.
	server.RegisterOnShutdown(api.Close)

	feedCtx, stopFeed := context.WithCancel(context.Background())
	defer stopFeed()
	go feed.Run(feedCtx, recipefeed.RefreshInterval, func(all, effective []recipe.Recipe) {
		jobEngine.SetRecipes(all, effective)
		api.SetRecipes(all, effective)
	})

	go func() {
		logger.Info("manager listening", "address", cfg.Listen, "pairing_token_path", authManager.PairingTokenPath(), "version", cfg.Version)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
			exit(1)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	stopFeed()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	// Not exit(0): the deferred db.Close() above must run on this path (the
	// systemctl stop / SIGTERM path on every GB10 machine), so pause first,
	// then let main return and its defers fire normally.
	pauseBeforeExit()
}

// printPairingInfo re-prints the pairing card so nobody ever has to hunt for
// the token file by hand after installation.
func printPairingInfo(cfg config.Config) {
	port := "7070"
	if _, listenPort, err := net.SplitHostPort(cfg.Listen); err == nil && listenPort != "" {
		port = listenPort
	}
	fmt.Println("RunOnSpark Manager — pairing")
	fmt.Println()
	if hostname, err := os.Hostname(); err == nil {
		short, _, _ := strings.Cut(hostname, ".")
		fmt.Printf("  Console:  http://%s.local:%s\n", short, port)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil || ipNet.IP.IsLoopback() {
				continue
			}
			label := "LAN"
			if tailscaleRange.Contains(ipNet.IP) {
				label = "Tailscale"
			}
			fmt.Printf("  %-9s http://%s:%s\n", label+":", ipNet.IP, port)
		}
	}
	fmt.Println()
	tokenPath := filepath.Join(cfg.DataDir, "pairing-token")
	if token, err := os.ReadFile(tokenPath); err == nil {
		fmt.Printf("  Pairing token: %s\n", strings.TrimSpace(string(token)))
	} else {
		fmt.Printf("  Pairing token: not created yet — start the service first (%s)\n", tokenPath)
	}
	fmt.Println()
	fmt.Println("  The console is reachable only on the interface the service listens on.")
}

// tailscaleRange is the CGNAT block Tailscale assigns addresses from.
var tailscaleRange = func() *net.IPNet {
	_, block, _ := net.ParseCIDR("100.64.0.0/10")
	return block
}()
