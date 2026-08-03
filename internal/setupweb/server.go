// Package setupweb serves the browser wizard: a loopback-only HTTP server
// that runs the same flow as `basement setup` (internal/setup) behind a
// single page instead of a terminal. It never talks to a manager API — like
// the terminal wizard, it only ever speaks SSH outward to the machine being
// installed.
package setupweb

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/punkjazz-labs/basement/internal/setup"
)

//go:embed assets/index.html
var pageAsset embed.FS

// tokenBytes sizes the single-use URL token. 24 bytes is 192 bits, well
// above the ≥128 bit floor the spec requires.
const tokenBytes = 24

// idleAfterFinish is how long the server keeps serving the summary (or
// error) page after the flow ends, so the operator has time to read it and
// copy the pairing token, before the process exits on its own rather than
// lingering as an orphaned background process with no window to close.
const idleAfterFinish = 15 * time.Minute

// Server is one wizard run: one process, one token, one flow. Binding
// 127.0.0.1:0 and minting a fresh random token happen in New; nothing here
// is reachable off the loopback interface.
type Server struct {
	listener net.Listener
	addr     string // host:port this server is bound to; the only Host header accepted
	token    string
	server   *http.Server
	logger   *log.Logger // method + path only, path with the token redacted; never bodies

	// flowCtx is set once in run, before the flow makes its first WizardUI
	// call, and only read afterward from that same goroutine chain — safe
	// without a lock since the flow is single-threaded by construction
	// (internal/setup only ever calls one WizardUI method at a time).
	flowCtx context.Context

	mu       sync.Mutex
	state    stateView
	pending  *pendingAnswer
	finished bool
	doneCh   chan struct{}
}

// pendingAnswer is the single in-flight question the flow is blocked on.
type pendingAnswer struct {
	kind   string
	answer chan json.RawMessage
}

// New binds a random loopback port and mints the wizard's token. It does
// not start serving or run the flow yet — call Start for that.
func New(logger *log.Logger) (*Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind loopback port: %w", err)
	}
	tokenRaw := make([]byte, tokenBytes)
	if _, err := rand.Read(tokenRaw); err != nil {
		listener.Close()
		return nil, fmt.Errorf("generate token: %w", err)
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	s := &Server{
		listener: listener,
		addr:     listener.Addr().String(),
		token:    base64.RawURLEncoding.EncodeToString(tokenRaw),
		logger:   logger,
		state:    stateView{Phase: "scanning", Progress: []string{}},
		doneCh:   make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /setup/{token}", s.handlePage)
	mux.HandleFunc("GET /setup/{token}/api/state", s.handleState)
	mux.HandleFunc("POST /setup/{token}/api/answer", s.handleAnswer)
	s.server = &http.Server{
		Handler:           s.logged(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// URL is the single-use address the wizard page lives at.
func (s *Server) URL() string {
	return "http://" + s.addr + "/setup/" + s.token
}

// Start serves the wizard in the background and begins the install flow
// immediately (discovery starts right away, same as the terminal wizard;
// the page catches up to whatever phase the flow has already reached when
// it starts polling).
func (s *Server) Start(ctx context.Context) {
	go func() {
		if err := s.server.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Printf("wizard server stopped: %v", err)
		}
	}()
	go s.run(ctx)
}

// Wait blocks until the flow has finished and the idle grace period has
// elapsed, or ctx is cancelled — then shuts the server down.
func (s *Server) Wait(ctx context.Context) {
	select {
	case <-s.doneCh:
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.server.Shutdown(shutdownCtx)
}

// run drives the shared setup flow (internal/setup) with this server as its
// WizardUI, exactly like basement setup does on the terminal.
func (s *Server) run(ctx context.Context) {
	s.flowCtx = ctx
	defer s.markFinished()

	found, err := setup.DiscoverAndChoose(ctx, s)
	if err != nil {
		s.fail(err)
		return
	}
	username, err := setup.ResolveUsername(s, found.Target, setup.DefaultSSHUser())
	if err != nil {
		s.fail(err)
		return
	}
	runner, err := setup.ConnectAndVerify(ctx, s, found.Target, username)
	if err != nil {
		s.fail(err)
		return
	}
	defer runner.Close()

	// The browser wizard only ever installs a remote machine over SSH — it
	// never runs on the GB10 itself (this binary only builds for darwin and
	// windows) — so remote is always true, and PickSource("") resolves to a
	// release download on the target since this process is never
	// linux/arm64.
	source := setup.PickSource("")
	result, err := setup.FinishInstall(ctx, s, runner, source, found.Peers, true)
	if err != nil {
		s.fail(err)
		return
	}
	// Same guided second machine as the terminal wizard, through the same
	// questions: the page needs no new phase for it.
	setup.InstallMore(ctx, s, setup.Machine{Target: found.Target, Result: result}, found.Offer, source, username)
}

// markFinished records that the flow is over. The page keys its polling off
// Done rather than off the first summary: a run can install a second machine
// after the first machine's card, so a summary is not the end of anything
// until this says so.
func (s *Server) markFinished() {
	s.mu.Lock()
	s.finished = true
	s.state.Done = true
	s.state.Seq++
	s.mu.Unlock()
	go func() {
		<-time.After(idleAfterFinish)
		close(s.doneCh)
	}()
}

func (s *Server) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Phase = "error"
	s.state.Error = err.Error()
	s.state.Seq++
}
