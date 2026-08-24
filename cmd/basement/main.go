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

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/config"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/httpapi"
	"github.com/punkjazz-labs/basement/internal/inventory"
	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/power"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/recipefeed"
	"github.com/punkjazz-labs/basement/internal/store"
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
	// The recipe feed seeds from the embedded set plus whatever verified
	// cache exists on disk. Fleet migration needs that same history so an
	// active distributed recipe from the cache is not missed as a legacy
	// candidate merely because it was not in the original embedded set.
	// The database is also where accepted revocations land, permanently: a
	// version this machine has been told not to install stays refused across
	// restarts and across later indexes that no longer mention it.
	feed := recipefeed.NewFetcher(recipes, cfg.DataDir, logger, db)
	cachedAll, cachedEffective := feed.Snapshot()
	provider := inventory.Host{DataDir: cfg.DataDir, DockerSocket: "/var/run/docker.sock"}
	buildIdentity, err := fleet.BinaryBuildIdentity(cfg.Version)
	if err != nil {
		logger.Error("identify manager build", "error", err)
		exit(1)
	}
	hostname, _ := os.Hostname()
	fleetManager, err := fleet.NewManager(context.Background(), fleet.Options{
		DataDir: cfg.DataDir, Database: db, Inventory: provider, Version: cfg.Version,
		BuildIdentity: buildIdentity, DisplayName: hostname,
		ConsoleURL: "http://" + cfg.PrimaryListen(), NodeURL: "https://" + cfg.FleetListen, Recipes: cachedAll, EffectiveRecipes: cachedEffective,
	})
	if err != nil {
		logger.Error("initialize fleet identity", "error", err)
		exit(1)
	}
	// A two-Spark recipe needs exactly one configured peer to act as the
	// worker node. Refusing to choose is deliberate: placement across a
	// larger fleet is ADR 0005 work that does not exist yet.
	worker := func(ctx context.Context) (operations.PeerTarget, error) {
		peers, err := db.Peers(ctx)
		if err != nil {
			return operations.PeerTarget{}, err
		}
		if len(peers) == 0 {
			return operations.PeerTarget{}, errors.New("this model needs two Sparks, so add the other Spark under Fleet first")
		}
		if len(peers) > 1 {
			return operations.PeerTarget{}, errors.New("this model needs exactly two Sparks, and more than one other Spark is configured")
		}
		peer, apiKey, err := db.PeerCredentials(ctx, peers[0].ID)
		if err != nil {
			return operations.PeerTarget{}, err
		}
		return operations.PeerTarget{ID: peer.ID, Name: peer.Name, BaseURL: peer.BaseURL, APIKey: apiKey}, nil
	}
	executor := operations.NewFleetExecutor(operations.NewHostExecutor(cfg.DataDir, "/var/run/docker.sock", provider), operations.NewPeerClient(worker))
	jobEngine := engine.New(db, executor, recipes)
	jobEngine.SetReservationAllocator(fleetManager.Allocator())

	// No network was involved above. Fetching starts only in the background;
	// the embedded recipes remain the permanent offline floor either way.
	jobEngine.SetRecipes(cachedAll, cachedEffective)

	// The GPU power mode of this machine. One controller serves both consoles:
	// this Spark's own, and the fleet dashboard of the controller that adopted
	// it, so the setting has one owner however it is changed.
	powerControl := power.NewController(db, power.Command)
	fleetManager.SetPowerRuntime(powerControl)

	api := httpapi.New(cfg.Version, cfg.DataDir, authManager, db, provider, executor, jobEngine, cachedEffective)
	api.SetFleetManager(fleetManager)
	api.SetPowerController(powerControl)
	api.SetRecipes(cachedAll, cachedEffective)
	// A Spark adopted from this console is installed to listen the same way
	// this one does (ADR 0014), so the API needs to know how that is.
	api.SetListenAddress(cfg.PrimaryListen())
	// The console reports feed health from the fetcher's own state, so an
	// index that has not been refreshed in a month can say so rather than
	// look like a healthy feed with nothing new in it.
	api.SetRecipeFeedHealth(feed.Health)
	// The same fetch the 6 hour cycle runs, on the owner's word. Wired here,
	// before anything can listen, together with the publication hook below, so
	// a forced fetch always reaches the engine and the API as well.
	feed.SetOnUpdate(func(all, effective []recipe.Recipe) {
		jobEngine.SetRecipes(all, effective)
		api.SetRecipes(all, effective)
		if err := fleetManager.SetRecipes(all, effective); err != nil {
			logger.Error("refresh fleet catalogue digest", "error", err)
		}
	})
	api.SetRecipeFeedRefresh(feed.FetchNow)
	// Token counters die with the container that publishes them, so the
	// engine reads them one last time before it stops one. Installed before
	// ResumeInterrupted below runs a single step: a resumed stop job can
	// reach stop_container as soon as it starts, and this must never be nil
	// when that happens.
	jobEngine.SetTokenSampler(api.CaptureFinalTokenUsage)

	// A staging attempt the previous process abandoned mid-download would
	// otherwise read as an update in progress forever, refusing every
	// install and generation until someone deletes the status file by hand.
	if err := api.RecoverAbandonedUpdate(); err != nil {
		logger.Warn("recover abandoned manager update", "error", err)
	}
	if err := jobEngine.ReconcileReservations(context.Background()); err != nil {
		logger.Error("reconcile node reservations", "error", err)
		exit(1)
	}
	if err := jobEngine.ResumeInterrupted(context.Background()); err != nil {
		logger.Error("resume interrupted jobs", "error", err)
		exit(1)
	}
	if err := jobEngine.ReconcileActiveModel(context.Background()); err != nil {
		logger.Error("reconcile active model", "error", err)
		exit(1)
	}
	// The GPU clock cap does not survive a reboot, so the stored mode goes back
	// on the GPU at every start. It runs beside the rest of the start and never
	// inside it: a machine with no GPU, and one whose driver has stopped
	// answering, must both start exactly as they always did.
	go func() {
		state, err := powerControl.ApplyStored(context.Background())
		if err != nil {
			logger.Error("apply stored GPU power mode", "error", err)
			return
		}
		logger.Info("GPU power mode", "mode", state.Mode, "failure", state.Failure)
	}()
	// One server, one handler, one listener per configured address: the
	// console answers the same way on every address it binds. Binding
	// happens here, before anything starts serving, so an address that
	// cannot be bound stops the manager instead of disappearing quietly.
	consoleListeners, err := listenConsole(cfg.Listen)
	if err != nil {
		logger.Error("bind console address", "error", err)
		exit(1)
	}
	server := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20}
	fleetServer := &http.Server{Addr: cfg.FleetListen, Handler: fleetManager.Handler(), TLSConfig: fleetManager.TLSConfig(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20}
	// Progress streams stay open indefinitely by design; without this hook a
	// restart waits out the whole drain timeout whenever a console is open.
	server.RegisterOnShutdown(api.Close)

	feedCtx, stopFeed := context.WithCancel(context.Background())
	defer stopFeed()
	go feed.Run(feedCtx, recipefeed.RefreshInterval)

	fleetCtx, stopFleet := context.WithCancel(context.Background())
	defer stopFleet()
	go fleetManager.Run(fleetCtx)
	// Driver renewal and orphan cleanup share the fleet heartbeat cadence. An
	// initial synchronous pass stops an already-expired worker rank before the
	// listeners can admit another runtime owner after restart.
	if err := api.ReclaimExpiredDriverReservations(fleetCtx); err != nil {
		logger.Error("reclaim expired driver reservations", "error", err)
	}
	go api.RunReservationMaintenance(fleetCtx)

	countCtx, stopCounting := context.WithCancel(context.Background())
	defer stopCounting()
	counting := make(chan struct{})
	go func() {
		defer close(counting)
		api.CountTokens(countCtx)
	}()

	for _, listener := range consoleListeners {
		go func(listener net.Listener) {
			address := listener.Addr().String()
			logger.Info("manager listening", "address", address, "pairing_token_path", authManager.PairingTokenPath(), "version", cfg.Version)
			// Shutdown below closes every listener this server was given,
			// so a normal stop arrives here as ErrServerClosed.
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("HTTP server failed", "error", err, "address", address)
				exit(1)
			}
		}(listener)
	}
	go func() {
		logger.Info("fleet manager listening", "address", cfg.FleetListen, "version", cfg.Version)
		if err := fleetServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("fleet TLS server failed", "error", err)
			exit(1)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	stopFeed()
	stopFleet()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	if err := fleetServer.Shutdown(ctx); err != nil {
		logger.Error("fleet graceful shutdown failed", "error", err)
	}
	// Waited on, not just cancelled, and only after the drain above: the
	// accounting loop takes one final token reading on its way out, and
	// taking it before the drain finished would miss whatever tokens
	// in-flight requests generated while the server was still handling them.
	// The database it writes to is closed by this function's defers.
	stopCounting()
	<-counting
	// Not exit(0): the deferred db.Close() above must run on this path (the
	// systemctl stop / SIGTERM path on every GB10 machine), so pause first,
	// then let main return and its defers fire normally.
	pauseBeforeExit()
}

// listenConsole binds every console address before the manager serves on any
// of them, and hands back the listeners in the configured order.
//
// A bind failure is fatal for the whole start. An address the owner named and
// the manager quietly skipped is an address they believe in and cannot reach,
// which is worse than a manager that refuses to start and says why. Listeners
// already open are closed again, so a refused start holds no port.
func listenConsole(addresses []string) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(addresses))
	for _, address := range addresses {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			for _, open := range listeners {
				open.Close()
			}
			return nil, fmt.Errorf("listen on %s: %w", address, err)
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

// printPairingInfo re-prints the pairing card so nobody ever has to hunt for
// the token file by hand after installation.
//
// The console addresses come from the configuration, which is what the
// service actually binds. The machine's own name is offered after them as an
// alternative, and only when at least one bound address leaves this machine:
// a loopback-only console cannot be opened by that name from anywhere else.
func printPairingInfo(cfg config.Config) {
	port := "7070"
	if _, listenPort, err := net.SplitHostPort(cfg.PrimaryListen()); err == nil && listenPort != "" {
		port = listenPort
	}
	fmt.Println("basement — pairing")
	fmt.Println()
	label := "Console:"
	reachable := false
	for _, address := range cfg.Listen {
		fmt.Printf("  %-9s http://%s\n", label, address)
		label = ""
		if host, _, err := net.SplitHostPort(address); err == nil {
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				reachable = true
			}
		}
	}
	if hostname, err := os.Hostname(); err == nil && reachable {
		short, _, _ := strings.Cut(hostname, ".")
		fmt.Printf("  %-9s http://%s.local:%s\n", "Also try:", short, port)
	}
	fmt.Println()
	tokenPath := filepath.Join(cfg.DataDir, "pairing-token")
	if token, err := os.ReadFile(tokenPath); err == nil {
		fmt.Printf("  Pairing token: %s\n", strings.TrimSpace(string(token)))
	} else {
		fmt.Printf("  Pairing token: not created yet — start the service first (%s)\n", tokenPath)
	}
	fmt.Println()
	fmt.Println("  The console answers only on the addresses above.")
}
