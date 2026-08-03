package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/punkjazz-labs/basement/internal/discovery"
	"github.com/punkjazz-labs/basement/internal/redact"
	"github.com/punkjazz-labs/basement/internal/setup"
	"github.com/punkjazz-labs/basement/internal/store"
)

// Adopting a second Spark from the console. The owner buys another GB10
// machine, plugs it in, and types its SSH credentials into this console; this
// manager finds it, installs itself on it, pairs with it and records it as
// the fleet peer. See docs/decisions/0014-console-spark-adoption.md.
//
// Both endpoints are console-session mutations with CSRF, exactly like every
// other console mutation. Neither accepts a bearer API key and neither is on
// peerAllowedPaths: adoption spends the owner's own authority over their own
// machines, and a fleet key held by another manager is not that.

const (
	// consolePort is the port every basement install listens on (see
	// internal/setup/install.go). A discovered machine is probed there, and
	// the one this manager installs answers there.
	consolePort = "7070"

	// fleetDiscoverBudget bounds one sweep. The console calls this on a
	// button press and waits for the answer, so it has to end well inside a
	// person's patience even when most of a /24 is dark.
	fleetDiscoverBudget = 9 * time.Second

	// basementProbeTimeout bounds one console fingerprint. Every candidate
	// is probed in parallel, so this is the sweep's tail, not its sum.
	basementProbeTimeout = 2 * time.Second

	// adoptionBudget bounds a whole adoption. Uploading a manager binary and
	// starting a service over a home network is minutes, not hours.
	adoptionBudget = 20 * time.Minute

	// consoleWait bounds how long the newly installed manager has to answer
	// on its own console port after systemd starts it.
	consoleWait = 90 * time.Second

	// maxDiscoveredCandidates caps one sweep's answer. A home network with
	// more than this many SSH hosts on it is not a home network, and mDNS
	// announcements are unauthenticated: anything on the segment can send as
	// many as it likes, and every candidate costs this machine a probe.
	maxDiscoveredCandidates = 64

	// maxProbeWorkers caps how many console fingerprints run at once. The
	// sweep is bounded by fleetDiscoverBudget either way; this bounds the
	// sockets and goroutines it takes to get there.
	maxProbeWorkers = 8

	// maxMachineNameLength is what the store accepts for a peer name, and
	// what a name another machine reported for itself is held to.
	maxMachineNameLength = 64
)

// tailscaleFleetRange is the CGNAT block Tailscale assigns from; a head that
// listens on one of those addresses installs its sibling the same way.
var tailscaleFleetRange = func() *net.IPNet {
	_, block, _ := net.ParseCIDR("100.64.0.0/10")
	return block
}()

// The seams. Production never reassigns these; tests stand in a fixed
// network, a fake machine and a fake console instead of sweeping a real LAN
// and dialling real SSH.
var (
	discoverCandidates = discovery.Discover
	adoptProbe         = setup.Probe
	adoptInstall       = setup.Install

	// consoleBaseURL is where a machine at address serves its console.
	consoleBaseURL = func(address string) string {
		return "http://" + net.JoinHostPort(address, consolePort)
	}

	// selfAddresses reports the names this machine answers to, so neither a
	// sweep nor an adoption ever points the owner at the Spark they are
	// already looking at.
	selfAddresses = localAddresses

	// resolveHost turns the address the owner typed into the addresses this
	// manager would actually connect to. Comparing spellings is not enough:
	// localhost., 127.0.0.2 and a DNS name pointing back here are all this
	// machine, and installing over itself mid-run is not a mistake anyone
	// recovers from gracefully.
	resolveHost = func(ctx context.Context, host string) ([]net.IP, error) {
		if ip := net.ParseIP(host); ip != nil {
			return []net.IP{ip}, nil
		}
		return net.DefaultResolver.LookupIP(ctx, "ip", host)
	}

	// localIPs reports every address this machine holds on its interfaces.
	localIPs = func() []net.IP {
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			return nil
		}
		ips := make([]net.IP, 0, len(addrs))
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				ips = append(ips, ipNet.IP)
			}
		}
		return ips
	}

	// adoptDial opens an SSH session to the machine being adopted. The
	// password is used for this connection (and for sudo on the other side)
	// and is never returned, stored or logged.
	adoptDial = func(ctx context.Context, address, username, password string) (setup.Runner, func(), error) {
		runner, err := setup.DialSSH(ctx, address, username, &typedCredentials{password: password})
		if err != nil {
			return nil, func() {}, err
		}
		return runner, func() { _ = runner.Close() }, nil
	}

	// adoptBinarySource stages the running manager's own binary onto the
	// machine being adopted, so the two Sparks can never end up on different
	// versions of basement.
	adoptBinarySource = func() (setup.BinarySource, error) {
		if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
			return nil, fmt.Errorf("this manager is a %s/%s build, so it has no linux/arm64 binary to install on the other Spark", runtime.GOOS, runtime.GOARCH)
		}
		executable, err := os.Executable()
		if err != nil {
			return nil, errors.New("this manager could not find its own binary to copy across")
		}
		return setup.UploadSource{Path: executable}, nil
	}
)

// typedCredentials answers every SSH prompt with the one password the owner
// typed into the console: the login itself, and sudo on the other side. It
// accepts an unknown host key on first sight, because there is no operator at
// a terminal to show a fingerprint to; a host key that is already recorded
// and no longer matches still fails inside setup, which is the case that
// matters.
type typedCredentials struct{ password string }

func (c *typedCredentials) Password(string) (string, error) { return c.password, nil }
func (c *typedCredentials) Confirm(string) (bool, error)    { return true, nil }

// localAddresses collects this machine's own IPv4/IPv6 addresses and names.
func localAddresses() map[string]bool {
	self := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				self[ipNet.IP.String()] = true
			}
		}
	}
	if hostname, err := os.Hostname(); err == nil {
		short, _, _ := strings.Cut(hostname, ".")
		self[strings.ToLower(hostname)] = true
		self[strings.ToLower(short)] = true
	}
	return self
}

// SetListenAddress records the address this manager was configured to serve
// on. It is what decides how a machine adopted from this console listens:
// the sibling matches the head. Called once at startup, before serving.
func (s *Server) SetListenAddress(address string) { s.listenAddress.Store(&address) }

// siblingListenMode maps the head's own listen address onto the mode the
// machine it adopts is installed with. A head on a Tailscale address gets a
// sibling on Tailscale; everything else gets the local network. That
// includes a head that only listens on loopback: the sibling still has to be
// reachable from this machine for the fleet to mean anything, and loopback is
// the one mode that could never be, so lan is the closest workable match.
// lan is also what the installer recommends for a machine set up remotely.
func (s *Server) siblingListenMode() setup.ListenMode {
	listen := ""
	if stored := s.listenAddress.Load(); stored != nil {
		listen = *stored
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		host = strings.TrimSpace(listen)
	}
	if ip := net.ParseIP(host); ip != nil && tailscaleFleetRange.Contains(ip) {
		return setup.ListenTailscale
	}
	return setup.ListenLAN
}

// discoveredBasement is the console of a machine that is already running
// basement, so the owner is offered pairing rather than an install.
type discoveredBasement struct {
	BaseURL string `json:"base_url"`
}

// discoveredCandidate is one machine a sweep found. Basement is null when
// nothing on the console port answered like a basement manager.
type discoveredCandidate struct {
	Name     string              `json:"name"`
	Address  string              `json:"address"`
	GB10Hint bool                `json:"gb10_hint"`
	Basement *discoveredBasement `json:"basement"`
}

// fleetDiscover sweeps the local network for GB10-class machines and reports
// which of them already run basement. It changes nothing, so it is safe to
// call as often as the console likes; it is a POST because it makes this
// machine talk to every address on its network, which is not something a
// link or a prefetch should be able to trigger.
func (s *Server) fleetDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), fleetDiscoverBudget)
	defer cancel()
	found, err := discoverCandidates(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("this Spark could not scan its network, so check that it has an active network connection"))
		return
	}
	self := selfAddresses()
	candidates := make([]discoveredCandidate, 0, maxDiscoveredCandidates)
	for _, candidate := range found {
		if len(candidates) >= maxDiscoveredCandidates {
			break
		}
		address := candidate.IP.String()
		// The name comes off the network, from a machine that is not ours
		// yet, so it is held to something safe to store and to render.
		name := sanitizeMachineName(setup.DisplayHost(candidate))
		if self[address] || self[strings.ToLower(name)] {
			continue
		}
		candidates = append(candidates, discoveredCandidate{
			Name:     name,
			Address:  address,
			GB10Hint: discovery.LikelyGB10Name(candidate.Hostname),
		})
	}
	probeCandidates(ctx, candidates, s.probeBasement)
	writeJSON(w, http.StatusOK, map[string]any{"candidates": candidates})
}

// probeCandidates fingerprints every candidate through a small worker pool.
// One goroutine and one socket per advertised address is a flood waiting to
// happen; a fixed pool makes the cost of a sweep the same whether the network
// holds three machines or three hundred.
func probeCandidates(ctx context.Context, candidates []discoveredCandidate, probe func(context.Context, string) *discoveredBasement) {
	workers := min(maxProbeWorkers, len(candidates))
	if workers == 0 {
		return
	}
	queue := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range queue {
				candidates[index].Basement = probe(ctx, candidates[index].Address)
			}
		}()
	}
	for index := range candidates {
		queue <- index
	}
	close(queue)
	wg.Wait()
}

// sanitizeMachineName holds a name another machine reported for itself to
// something this manager can store and show: control characters removed and
// the length capped to what the store accepts. It is remote-controlled data
// in a field that ends up in the peers table and on screen, so it is treated
// as untrusted text rather than as a hostname.
func sanitizeMachineName(raw string) string {
	var builder strings.Builder
	for _, symbol := range strings.TrimSpace(raw) {
		if unicode.IsControl(symbol) || symbol == utf8.RuneError {
			continue
		}
		if builder.Len()+utf8.RuneLen(symbol) > maxMachineNameLength {
			break
		}
		builder.WriteRune(symbol)
	}
	return strings.TrimSpace(builder.String())
}

// probeBasement reports whether a machine is already running basement. The
// proof is a 401 in this manager's own error shape on /api/v1/system: that
// endpoint answers nothing else without credentials, and none are offered
// here. Anything else on that port (another service, a plain web server,
// silence) reads as not basement, which costs the owner an install offer
// they can decline rather than a pairing that could never work.
func (s *Server) probeBasement(ctx context.Context, address string) *discoveredBasement {
	probeCtx, cancel := context.WithTimeout(ctx, basementProbeTimeout)
	defer cancel()
	baseURL := consoleBaseURL(address)
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, baseURL+"/api/v1/system", nil)
	if err != nil {
		return nil
	}
	response, err := s.peerClient.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		return nil
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return nil
	}
	var shape struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(payload, &shape) != nil || strings.TrimSpace(shape.Error) == "" {
		return nil
	}
	return &discoveredBasement{BaseURL: baseURL}
}

// Adoption progress lives in memory on purpose. It is the narration of one
// run that a person is watching, not a record: it survives page reloads,
// which is what the console needs, and it does not survive a manager
// restart, which nothing needs. The jobs table is the engine's, keyed by
// recipe id and replayed by ResumeInterrupted on startup; an adoption has no
// recipe and cannot be resumed after a restart (the SSH password that made
// it possible is gone by then, deliberately), so borrowing that table would
// mean teaching it about a job kind it can never finish.
const (
	adoptionIdle      = "idle"
	adoptionRunning   = "running"
	adoptionSucceeded = "succeeded"
	adoptionFailed    = "failed"
)

const (
	stepPending = "pending"
	stepRunning = "running"
	stepDone    = "done"
	stepFailed  = "failed"
)

// adoptionStep is one stage of the flow as the console renders it.
type adoptionStep struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// adoptionPlan is every stage, in the order they run.
var adoptionPlan = []adoptionStep{
	{Key: "connect", Label: "Sign in over SSH"},
	{Key: "verify", Label: "Confirm it is a GB10 machine"},
	{Key: "install", Label: "Install basement on it"},
	{Key: "start", Label: "Wait for its console"},
	{Key: "pair", Label: "Pair and create a fleet key"},
	{Key: "peer", Label: "Add it to the fleet"},
}

// adoptionResult is what the console shows when it worked.
type adoptionResult struct {
	Peer       peerView `json:"peer"`
	ConsoleURL string   `json:"console_url"`
	AltURL     string   `json:"alt_url,omitempty"`
	// OwnerPairingURL is the new console, and OwnerPairingToken is the
	// pairing token to type into it. Pairing does not consume the token
	// (internal/auth: it is a file-backed shared secret compared on every
	// pair, not a one-shot nonce), so the token this manager used to create
	// its fleet key is still the token the owner's browser pairs with.
	OwnerPairingURL   string `json:"owner_pairing_url"`
	OwnerPairingToken string `json:"owner_pairing_token,omitempty"`
}

// adoptionView is the whole status payload the console polls.
type adoptionView struct {
	State      string          `json:"state"`
	Address    string          `json:"address,omitempty"`
	StartedAt  string          `json:"started_at,omitempty"`
	FinishedAt string          `json:"finished_at,omitempty"`
	Steps      []adoptionStep  `json:"steps"`
	Progress   []string        `json:"progress"`
	Error      string          `json:"error,omitempty"`
	Result     *adoptionResult `json:"result,omitempty"`
}

// adoptionState holds the one adoption this manager will run at a time.
type adoptionState struct {
	mu   sync.Mutex
	view adoptionView
}

func newAdoptionState() *adoptionState {
	return &adoptionState{view: adoptionView{State: adoptionIdle, Steps: freshSteps(), Progress: []string{}}}
}

func freshSteps() []adoptionStep {
	steps := make([]adoptionStep, 0, len(adoptionPlan))
	for _, step := range adoptionPlan {
		step.State = stepPending
		steps = append(steps, step)
	}
	return steps
}

// claim starts a run, or refuses because one is already going. One at a
// time: two adoptions would race for the single peer row, and each one holds
// an SSH session that installs a systemd service.
func (a *adoptionState) claim(address string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.view.State == adoptionRunning {
		return errors.New("this Spark is already adopting another machine, so wait for that to finish")
	}
	a.view = adoptionView{
		State:     adoptionRunning,
		Address:   address,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Steps:     freshSteps(),
		Progress:  []string{},
	}
	return nil
}

func (a *adoptionState) snapshot() adoptionView {
	a.mu.Lock()
	defer a.mu.Unlock()
	view := a.view
	view.Steps = append([]adoptionStep(nil), a.view.Steps...)
	view.Progress = append([]string(nil), a.view.Progress...)
	return view
}

func (a *adoptionState) update(mutate func(view *adoptionView)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	mutate(&a.view)
}

func (v *adoptionView) markStep(key, state, detail string) {
	for index := range v.Steps {
		if v.Steps[index].Key == key {
			v.Steps[index].State = state
			if detail != "" {
				v.Steps[index].Detail = detail
			}
			return
		}
	}
}

// adoptionRun is the only handle the flow has on its own progress, and it
// scrubs every string on the way in. The SSH password never reaches the
// state object, so it cannot reach the status endpoint, an error message or
// a progress line even if an SSH library echoes it back at us.
type adoptionRun struct {
	state *adoptionState
	scrub func(string) string
}

func newAdoptionRun(state *adoptionState, password string) *adoptionRun {
	return &adoptionRun{state: state, scrub: passwordScrubber(password)}
}

// passwordScrubber removes one typed password from any text, then applies
// the manager's ordinary secret redaction on top.
func passwordScrubber(password string) func(string) string {
	return func(value string) string {
		if password != "" {
			value = strings.ReplaceAll(value, password, "[redacted]")
		}
		return redact.String(value)
	}
}

func (r *adoptionRun) begin(key string) {
	r.state.update(func(view *adoptionView) { view.markStep(key, stepRunning, "") })
}

func (r *adoptionRun) done(key, detail string) {
	detail = r.scrub(detail)
	r.state.update(func(view *adoptionView) { view.markStep(key, stepDone, detail) })
}

func (r *adoptionRun) progress(format string, args ...any) {
	line := r.scrub(fmt.Sprintf(format, args...))
	r.state.update(func(view *adoptionView) {
		if len(view.Progress) < 200 {
			view.Progress = append(view.Progress, line)
		}
	})
}

// fail ends the run. message is the sentence the owner reads; cause, when
// there is one, is appended so the failure is diagnosable, scrubbed first.
func (r *adoptionRun) fail(key, message string, cause error) {
	r.failNoted(key, message, cause, "")
}

// failNoted is fail with one more sentence after the cause: what this manager
// did about the state it left on the other machine.
func (r *adoptionRun) failNoted(key, message string, cause error, note string) {
	if cause != nil {
		message += ": " + cause.Error()
	}
	if note != "" {
		message += ". " + note
	}
	message = r.scrub(message)
	r.state.update(func(view *adoptionView) {
		view.markStep(key, stepFailed, "")
		view.State = adoptionFailed
		view.Error = message
		view.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	})
}

// succeed ends the run. The happy path is scrubbed exactly like the failure
// path: some of these strings came from the machine on the other end, which
// is free to answer `hostname` with the password it was just handed, and a
// success result is as much outbound state as an error message is.
func (r *adoptionRun) succeed(result adoptionResult) {
	result.Peer.Name = r.scrub(result.Peer.Name)
	result.Peer.BaseURL = r.scrub(result.Peer.BaseURL)
	result.ConsoleURL = r.scrub(result.ConsoleURL)
	result.AltURL = r.scrub(result.AltURL)
	result.OwnerPairingURL = r.scrub(result.OwnerPairingURL)
	result.OwnerPairingToken = r.scrub(result.OwnerPairingToken)
	r.state.update(func(view *adoptionView) {
		view.State = adoptionSucceeded
		view.Result = &result
		view.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	})
}

// fleetAdopt starts the adoption of one machine and answers immediately with
// the first status snapshot; the console polls the status endpoint from
// there. The SSH password lives in this handler's arguments and in the
// goroutine it starts, and nowhere else: not in the store, not in the jobs
// table, not in a log line, not in any payload this manager ever writes.
func (s *Server) fleetAdopt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var request struct {
		Address  string `json:"address"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	address, err := adoptionAddress(request.Address)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	username := strings.TrimSpace(request.Username)
	if username == "" {
		writeError(w, http.StatusBadRequest, errors.New("the username to sign in with is required"))
		return
	}
	if request.Password == "" {
		writeError(w, http.StatusBadRequest, errors.New("the password for that account is required"))
		return
	}
	if self := selfAddresses(); self[strings.ToLower(address)] {
		writeError(w, http.StatusBadRequest, errSelfAdoption)
		return
	}
	if err := checkAdoptionTarget(r.Context(), address); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// One peer, per the ADR 0005 deferral: cmd/basement/main.go refuses to
	// pick a worker when more than one is configured, so a second peer would
	// be a row that breaks every two-Spark model rather than a fleet.
	peers, err := s.store.Peers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(peers) > 0 {
		writeError(w, http.StatusConflict, errors.New("another Spark is already in the fleet, so remove it under Fleet before adopting a different one"))
		return
	}
	if err := s.adoption.claim(address); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	run := newAdoptionRun(s.adoption, request.Password)
	// Detached from the request context on purpose: the console navigating
	// away, or the fetch being cancelled, must not abandon a half-installed
	// machine. The run is bounded by adoptionBudget instead.
	ctx, cancel := context.WithTimeout(context.Background(), adoptionBudget)
	go func() {
		defer cancel()
		s.runAdoption(ctx, run, address, username, request.Password)
	}()
	writeJSON(w, http.StatusAccepted, s.adoption.snapshot())
}

// fleetAdoptStatus is how the console follows a run, including after a page
// reload. Read-only, console session required.
func (s *Server) fleetAdoptStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, s.adoption.snapshot())
}

// adoptionAddress accepts a bare host or IP and nothing else. A scheme, a
// port, a path or embedded credentials would each mean the console URL this
// manager builds from it points somewhere other than where it just installed.
// The character set is the one a hostname or an IPv4 address is spelled with,
// so nothing that reaches an error sentence, a log line or a URL carries
// anything but letters, digits, dots and dashes.
func adoptionAddress(raw string) (string, error) {
	address := strings.TrimSpace(raw)
	if address == "" {
		return "", errors.New("the address of the other Spark is required")
	}
	if len(address) > 253 {
		return "", errors.New("that address is too long to be a machine name")
	}
	for _, symbol := range address {
		switch {
		case symbol >= 'a' && symbol <= 'z', symbol >= 'A' && symbol <= 'Z',
			symbol >= '0' && symbol <= '9', symbol == '.', symbol == '-', symbol == '_':
		default:
			return "", errors.New("enter just the machine's address, with no scheme, port or path")
		}
	}
	return address, nil
}

// errSelfAdoption is the one sentence both self-checks give: the spelling
// check on the way in, and the resolved check that catches the spellings.
var errSelfAdoption = errors.New("that address is this Spark itself, so enter the address of the other machine")

// checkAdoptionTarget decides whether this manager will spend an SSH session
// on the address the owner typed. Two rules, both about what the address
// actually resolves to rather than how it is written.
//
// It is never this machine. localhost., 127.0.0.2, any other loopback address
// and any name that resolves to an address this machine holds all mean the
// manager would install over itself, restarting the service running the
// adoption halfway through.
//
// It is on a network the owner could own: a private range, a link-local
// segment, or the CGNAT block Tailscale assigns from. That is what the
// product is (Sparks on your own network), and it keeps a console session
// from being used as a general-purpose SSH prober against the internet.
func checkAdoptionTarget(ctx context.Context, address string) error {
	resolved, err := resolveHost(ctx, address)
	if err != nil || len(resolved) == 0 {
		return fmt.Errorf("this Spark could not find %s on the network, so check the address", address)
	}
	own := localIPs()
	for _, ip := range resolved {
		if ip.IsLoopback() {
			return errSelfAdoption
		}
		for _, mine := range own {
			if mine.Equal(ip) {
				return errSelfAdoption
			}
		}
	}
	for _, ip := range resolved {
		if !discovery.IsLocalFabric(ip) {
			return fmt.Errorf("%s is not on your own network or tailnet, and basement only adopts machines that are", address)
		}
	}
	return nil
}

// runAdoption is the whole flow, one step at a time, each reported as it
// happens. Nothing is written to the store until the last step, so a failure
// anywhere leaves no peer row behind.
func (s *Server) runAdoption(ctx context.Context, run *adoptionRun, address, username, password string) {
	run.begin("connect")
	run.progress("connecting to %s as %s", address, username)
	runner, closeRunner, err := adoptDial(ctx, address, username, password)
	if err != nil {
		run.fail("connect", fmt.Sprintf("could not sign in to %s as %s, so check the address, the username and the password", address, username), err)
		return
	}
	defer closeRunner()
	run.done("connect", "signed in as "+username)

	run.begin("verify")
	identity := adoptProbe(ctx, runner)
	if !identity.IsGB10() {
		found := sanitizeMachineName(identity.Product())
		if gpu := sanitizeMachineName(identity.GPUName); gpu != "" && gpu != found {
			found += " with " + gpu
		}
		run.fail("verify", fmt.Sprintf("%s is not a GB10 machine (it reports %s), so basement will not install there", address, found), nil)
		return
	}
	// The peer's name in this console is the name the machine reports for
	// itself. That machine is not ours yet and its answers are whatever it
	// chose to answer, so the name is scrubbed of the typed password and held
	// to printable, bounded text before it is stored or shown anywhere.
	name := sanitizeMachineName(run.scrub(identity.Hostname))
	if name == "" {
		name = address
	}
	run.done("verify", sanitizeMachineName(identity.Product()))
	run.progress("confirmed %s (%s)", sanitizeMachineName(identity.Product()), name)

	run.begin("install")
	source, err := adoptBinarySource()
	if err != nil {
		run.fail("install", "this Spark cannot install basement on another machine", err)
		return
	}
	listen := s.siblingListenMode()
	run.progress("installing this manager's own build, listening on %s", listen)
	result, err := adoptInstall(ctx, runner, source, setup.Options{Listen: listen, ConsoleHost: address}, func(format string, args ...any) {
		run.progress(format, args...)
	})
	if err != nil {
		run.fail("install", fmt.Sprintf("could not install basement on %s", address), err)
		return
	}
	if result.Loopback {
		run.fail("install", fmt.Sprintf("%s installed a console that only it can reach, so it cannot join the fleet", address), nil)
		return
	}
	// Everything from here on talks to the address the owner adopted and this
	// manager has just signed in to over SSH, never to an address the machine
	// reported for itself. A hostile SSH endpoint can answer `hostname -I` or
	// `tailscale ip` with an accomplice's address; believing it would point
	// the pairing, the fleet key and the stored peer row at that other host.
	consoleURL := consoleBaseURL(address)
	// Addresses the machine reported are kept for display only, and only
	// when they are a bare origin like every other peer URL this manager
	// accepts.
	altURL := ""
	for _, reported := range []string{result.AltURL, result.ConsoleURL} {
		if alternate, err := normalizedPeerBaseURL(reported); err == nil && alternate != consoleURL {
			altURL = alternate
			break
		}
	}
	run.done("install", consoleURL)

	run.begin("start")
	run.progress("waiting for %s to answer", consoleURL)
	if err := s.waitForConsole(ctx, consoleURL); err != nil {
		run.fail("start", fmt.Sprintf("%s installed basement but its console did not come up", address), err)
		return
	}
	run.done("start", "console is up")

	run.begin("pair")
	if strings.TrimSpace(result.Token) == "" {
		run.fail("pair", fmt.Sprintf("%s did not produce a pairing token, so this Spark cannot get a key from it", address), nil)
		return
	}
	key, err := s.mintFleetKey(ctx, consoleURL, result.Token)
	if err != nil {
		run.fail("pair", fmt.Sprintf("could not pair with the new console on %s", consoleURL), err)
		return
	}
	run.done("pair", "fleet key created on "+name)

	// From here a fleet key exists on the other machine. Every failure below
	// hands it back rather than leaving a credential behind that nobody can
	// see and nobody asked for.
	run.begin("peer")
	// The single-peer rule is checked again here: the handler refused a
	// second adoption, but a peer could have been added by hand while this
	// one ran, and the store is the only thing that decides.
	peers, err := s.store.Peers(ctx)
	if err != nil {
		run.failNoted("peer", "could not read this Spark's fleet", err, s.revokeFleetKey(ctx, consoleURL, key))
		return
	}
	if len(peers) > 0 {
		run.failNoted("peer", "another Spark was added to the fleet while this one was being set up, so nothing was recorded", nil, s.revokeFleetKey(ctx, consoleURL, key))
		return
	}
	peer, err := s.addPeer(ctx, name, consoleURL, key.secret)
	if err != nil {
		run.failNoted("peer", fmt.Sprintf("%s is set up, but this Spark could not record it as the fleet peer", address), err, s.revokeFleetKey(ctx, consoleURL, key))
		return
	}
	run.done("peer", peer.Name)
	run.succeed(adoptionResult{
		Peer:              peerView{ID: peer.ID, Name: peer.Name, BaseURL: peer.BaseURL},
		ConsoleURL:        consoleURL,
		AltURL:            altURL,
		OwnerPairingURL:   consoleURL,
		OwnerPairingToken: result.Token,
	})
}

// waitForConsole polls the newly started manager's health endpoint until it
// answers. systemd has already been told to start it; this is the gap
// between that and the process being ready to serve.
func (s *Server) waitForConsole(ctx context.Context, baseURL string) error {
	deadline := time.Now().Add(consoleWait)
	var last error
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			return err
		}
		response, err := s.peerClient.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("it answered with status %d", response.StatusCode)
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			if last == nil {
				last = errors.New("it never answered")
			}
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// mintFleetKey pairs with the freshly installed manager using the pairing
// token its install produced, and creates one API key on it named after this
// machine. Those two paths are deliberately NOT in peerAllowedPaths: that
// allowlist governs calls made with a stored peer credential, and nothing on
// it may mutate the other machine. This is a one-time bootstrap against a
// machine this manager just installed, with a token that machine just
// minted, and both paths are fixed strings rather than anything a caller can
// steer.
//
// Pairing does not consume the token: internal/auth compares every pair
// against the same file-backed secret, so the owner can still pair their own
// browser with the same token afterwards.
func (s *Server) mintFleetKey(ctx context.Context, baseURL, token string) (fleetKey, error) {
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return fleetKey{}, err
	}
	response, err := s.callNewSpark(ctx, http.MethodPost, baseURL, "/api/v1/auth/pair", body, nil)
	if err != nil {
		return fleetKey{}, err
	}
	var paired struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(response.body, &paired); err != nil || paired.CSRF == "" {
		return fleetKey{}, errors.New("it did not accept the pairing token it had just created")
	}
	// Rebuilt as a request cookie (name=value) rather than echoing the
	// Set-Cookie line, whose attributes belong to the response.
	pairs := make([]string, 0, len(response.cookies))
	for _, cookie := range response.cookies {
		pairs = append(pairs, cookie.Name+"="+cookie.Value)
	}
	session := strings.Join(pairs, "; ")
	if session == "" {
		return fleetKey{}, errors.New("it did not open a session for this Spark")
	}
	body, err = json.Marshal(map[string]string{"name": fleetKeyName()})
	if err != nil {
		return fleetKey{}, err
	}
	response, err = s.callNewSpark(ctx, http.MethodPost, baseURL, "/api/v1/keys", body, map[string]string{
		"Cookie":       session,
		"X-CSRF-Token": paired.CSRF,
	})
	if err != nil {
		return fleetKey{}, err
	}
	var created struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(response.body, &created); err != nil || created.Secret == "" {
		// It answered the key request successfully and then said nothing
		// usable, so a key may exist over there with no way to name it.
		return fleetKey{}, errors.New("it accepted the request for a fleet key and did not return one, so check for a key named " + fleetKeyName() + " under Connect on its own console")
	}
	return fleetKey{secret: created.Secret, id: created.Key.ID, session: session, csrf: paired.CSRF}, nil
}

// fleetKey is the API key this manager created on the machine it is adopting,
// together with the bootstrap session it was created with. The session is
// held for one reason: if the run fails after this point, the key can be
// handed straight back instead of living on that machine forever, unseen by
// the owner and unremovable without a second bootstrap.
type fleetKey struct {
	secret  string
	id      string
	session string
	csrf    string
}

// revokeFleetKey deletes the key this manager minted on the other machine and
// returns the sentence that says what happened, for the failure the owner is
// about to read. Best effort by nature: the machine may already be gone. When
// it cannot be done, the sentence names the key so the owner can remove it
// themselves rather than being left with a credential they never knew about.
func (s *Server) revokeFleetKey(ctx context.Context, baseURL string, key fleetKey) string {
	orphaned := "A fleet key named " + fleetKeyName() + " was left on that machine and could not be removed, so delete it under Connect on its own console"
	if key.id == "" || key.session == "" {
		return orphaned
	}
	// Detached from the run's context: an adoption that failed because it ran
	// out of budget should still get its key back.
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_, err := s.callNewSpark(revokeCtx, http.MethodDelete, baseURL, "/api/v1/keys/"+url.PathEscape(key.id), nil, map[string]string{
		"Cookie":       key.session,
		"X-CSRF-Token": key.csrf,
	})
	if err != nil {
		return orphaned
	}
	return "The fleet key this Spark had created on that machine has been removed"
}

// fleetKeyName names the key after this machine, so the owner reading the
// other console's Connect tab can tell what it is for.
func fleetKeyName() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "basement"
	}
	short, _, _ := strings.Cut(host, ".")
	name := "fleet-" + short
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

type newSparkResponse struct {
	body    []byte
	cookies []*http.Cookie
}

// callNewSpark makes one bootstrap call to the machine being adopted. The
// Origin header matches the URL this manager is calling, which is what that
// manager's own CSRF and origin checks require of a console session.
func (s *Server) callNewSpark(ctx context.Context, method, baseURL, endpoint string, body []byte, headers map[string]string) (newSparkResponse, error) {
	request, err := http.NewRequestWithContext(ctx, method, baseURL+endpoint, strings.NewReader(string(body)))
	if err != nil {
		return newSparkResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", baseURL)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := s.peerClient.Do(request)
	if err != nil {
		return newSparkResponse{}, errors.New("this Spark could not reach the new console")
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return newSparkResponse{}, errors.New("the new console started answering and then stopped")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var problem struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &problem) == nil && problem.Error != "" {
			return newSparkResponse{}, fmt.Errorf("the new console answered %d: %s", response.StatusCode, problem.Error)
		}
		return newSparkResponse{}, fmt.Errorf("the new console answered %d", response.StatusCode)
	}
	return newSparkResponse{body: payload, cookies: response.Cookies()}, nil
}

// addPeer is the one path that records a peer, shared by the manual
// add-by-address form and by adoption: normalize the URL, prove the URL and
// key actually reach a Spark together, then store. A peer entry that has
// never been confirmed reachable is worse than no entry at all.
func (s *Server) addPeer(ctx context.Context, name, rawBaseURL, apiKey string) (store.Peer, error) {
	// A peer name is either typed by the owner or reported by the machine
	// being adopted. The second kind is untrusted text, and this is the one
	// door both kinds come through, so it is held to shape here.
	name = sanitizeMachineName(name)
	baseURL, err := normalizedPeerBaseURL(rawBaseURL)
	if err != nil {
		return store.Peer{}, err
	}
	if strings.TrimSpace(apiKey) == "" {
		return store.Peer{}, errors.New("an API key is required")
	}
	if _, err := s.fetchPeerJSON(ctx, baseURL, apiKey, "/api/v1/system"); err != nil {
		return store.Peer{}, errors.New("could not reach that Spark with this key, so check the URL and key")
	}
	return s.store.CreatePeer(ctx, name, baseURL, apiKey)
}
