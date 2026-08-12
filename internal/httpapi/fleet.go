package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/punkjazz-labs/basement/internal/discovery"
	"github.com/punkjazz-labs/basement/internal/fleet"
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

	// basementHealthTimeout bounds the health check that follows a positive
	// fingerprint. The machine has already answered once by then, so a
	// manager that is really running answers this well inside a second.
	basementHealthTimeout = time.Second

	// maxVersionLength is what a version another machine reported for itself
	// is held to before it is stored or shown.
	maxVersionLength = 32

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
// basement, so the owner is offered pairing rather than an install. Running
// says the manager there answered its health check just now, which is what
// lets the console offer Add instead of Install; Version is what it reported.
type discoveredBasement struct {
	BaseURL string `json:"base_url"`
	Running bool   `json:"running"`
	Version string `json:"version"`
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
//
// It is deliberately split into its two transformations, so that a caller
// holding a secret can scrub between them and around them. Every transformation
// that rewrites the bytes of a secret needs a scrub on both sides of it; see
// adoptionRun.safeRemoteText, which is the only way remote text reaches
// anything this manager stores or shows.
func sanitizeMachineName(raw string) string {
	return capMachineName(stripUnsafeRunes(raw))
}

// stripUnsafeRunes removes control characters and invalid UTF-8 from text
// another machine chose. Removing bytes can join what they separated, so
// nothing may rely on a scrub that ran before this.
func stripUnsafeRunes(raw string) string {
	var builder strings.Builder
	for _, symbol := range strings.TrimSpace(raw) {
		if unicode.IsControl(symbol) || symbol == utf8.RuneError {
			continue
		}
		builder.WriteRune(symbol)
	}
	return strings.TrimSpace(builder.String())
}

// capMachineName holds text to the length the store accepts. Cutting text can
// leave a fragment of a secret that no later scrub recognises, so nothing may
// rely on a scrub that ran before this either.
func capMachineName(value string) string {
	return capText(value, maxMachineNameLength)
}

// capText holds text to a byte budget, always on a rune boundary: a cap that
// counted bytes alone would cut a multi-byte character in half and leave a
// broken fragment behind, which is both unrenderable and, when the text came
// from a machine trying to hand a secret back, a piece of that secret in a
// shape nothing recognises.
func capText(value string, limit int) string {
	var builder strings.Builder
	for _, symbol := range value {
		if builder.Len()+utf8.RuneLen(symbol) > limit {
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
	found := &discoveredBasement{BaseURL: baseURL}
	found.Running, found.Version = s.probeBasementHealth(probeCtx, baseURL)
	return found
}

// probeBasementHealth asks a console that already reads as basement whether
// its manager is answering, and which version it is. It is a second, shorter
// request on a machine that has just proved it is one of ours, so the sweep's
// tail grows by about a second at worst and nothing new is scanned.
func (s *Server) probeBasementHealth(ctx context.Context, baseURL string) (bool, string) {
	healthCtx, cancel := context.WithTimeout(ctx, basementHealthTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(healthCtx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return false, ""
	}
	response, err := s.peerClient.Do(request)
	if err != nil {
		return false, ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, ""
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return false, ""
	}
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if json.Unmarshal(payload, &health) != nil || health.Status != "ok" {
		return false, ""
	}
	// The version came off another machine, so it is held to something safe
	// to store and to render, like every other name a sweep collects.
	return true, capText(stripUnsafeRunes(health.Version), maxVersionLength)
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
	{Key: "pair", Label: "Pair managers securely"},
	{Key: "peer", Label: "Add it to the fleet"},
}

// adoptionResult is what the console shows when it worked.
type adoptionResult struct {
	Peer       peerView         `json:"peer"`
	Node       *store.FleetNode `json:"node,omitempty"`
	ConsoleURL string           `json:"console_url"`
	AltURL     string           `json:"alt_url,omitempty"`
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
//
// It removes more than the exact bytes the owner typed, because an exact match
// is only a match while the text is still exactly what it was. This path
// transforms remote text as well as scrubbing it, and every transformation
// leaves the secret in a shape a literal comparison walks straight past. So the
// scrubber matches the password and the normalized forms the transformations on
// this path can produce:
//
//   - the password with its surrounding whitespace gone. Both transformations
//     here trim, so a password typed as " correct-horse " that a machine reports
//     back as its hostname arrives at the store as "correct-horse".
//   - the password with control characters and invalid UTF-8 gone, which is
//     what stripUnsafeRunes leaves of a password that contains any.
//   - either of those in any mix of case, because case folding is the cheapest
//     rewrite there is and hostnames are compared and lowercased all over this
//     file.
//
// That list is closed on purpose. It is the set of rewrites this code actually
// performs, not a guess at every normalization that exists; an open-ended list
// would be reassurance rather than a defence. Anything else is handled by the
// other half of the rule, which is that scrubbing runs before a transformation
// and again after it (see adoptionRun.safeRemoteText).
//
// There is no minimum match length: a four-character password is as much the
// owner's password as a forty-character one.
//
// The derived forms live in this closure and nowhere else. They are the
// password wearing slightly different clothes, so they are never stored,
// logged or returned.
func passwordScrubber(password string) func(string) string {
	forms := passwordForms(password)
	return func(value string) string {
		for _, form := range forms {
			value = replaceFold(value, form, "[redacted]")
		}
		return redact.String(value)
	}
}

// passwordForms derives, once, every shape of the typed password this manager's
// own transformations can produce. Longest first, so the fullest form of the
// secret is the one that gets matched when several of them would.
func passwordForms(password string) []string {
	forms := make([]string, 0, 3)
	seen := make(map[string]bool, 3)
	// In descending length by construction: trimming and stripping only ever
	// remove characters.
	for _, form := range []string{password, strings.TrimSpace(password), stripUnsafeRunes(password)} {
		if form == "" || seen[form] {
			continue
		}
		seen[form] = true
		forms = append(forms, form)
	}
	return forms
}

// replaceFold replaces every case-insensitive occurrence of secret in value.
// It walks value rune by rune rather than lowercasing both sides and comparing
// byte offsets, because lowercasing can change a string's length and the
// offsets would stop meaning what they said. Bytes that are not valid UTF-8 are
// copied through one at a time, so text this manager has not stripped yet
// survives the pass unchanged.
func replaceFold(value, secret, replacement string) string {
	if secret == "" {
		return value
	}
	var builder strings.Builder
	for index := 0; index < len(value); {
		if length := foldPrefixLength(value[index:], secret); length > 0 {
			builder.WriteString(replacement)
			index += length
			continue
		}
		_, size := utf8.DecodeRuneInString(value[index:])
		builder.WriteString(value[index : index+size])
		index += size
	}
	return builder.String()
}

// foldPrefixLength reports how many bytes of value are a case-insensitive match
// for the whole of secret, or -1 when value does not start with it.
func foldPrefixLength(value, secret string) int {
	consumed := 0
	for len(secret) > 0 {
		wanted, size := utf8.DecodeRuneInString(secret)
		secret = secret[size:]
		if consumed >= len(value) {
			return -1
		}
		found, foundSize := utf8.DecodeRuneInString(value[consumed:])
		if !equalFoldRune(found, wanted) {
			return -1
		}
		consumed += foundSize
	}
	return consumed
}

// equalFoldRune compares two runes under Unicode simple folding, which is the
// same rule strings.EqualFold applies.
func equalFoldRune(first, second rune) bool {
	if first == second {
		return true
	}
	if first > second {
		first, second = second, first
	}
	for folded := unicode.SimpleFold(first); folded != first; folded = unicode.SimpleFold(folded) {
		if folded == second {
			return true
		}
	}
	return false
}

// safeRemoteText is the only way text another machine chose becomes text this
// manager stores, shows or puts in a sentence. The rule is one line long: scrub
// before any transformation, and scrub again after every transformation. Both
// halves are load bearing, and each covers what the other cannot.
//
// A scrub that runs only before a transformation is not a scrub, because the
// transformation can put the secret back together:
//
//   - stripping control characters joins what they separated. A machine that
//     answers `hostname` with "sec<esc>ret" when the password is "secret"
//     defeats a scrub that ran first, and the strip that ran after it hands the
//     password to whatever stores the result.
//   - capping the length cuts a secret into a fragment no later scrub
//     recognises. A name padded past the cap so the password straddles it would
//     otherwise leave a prefix of the password in the peers table.
//
// A scrub that runs only after a transformation is not a scrub either, because
// the transformation can turn text that did not match into text that is the
// secret in all but the bytes that were removed. A password typed as
// " correct-horse ", reported back as the hostname " correct-horse ", is
// trimmed to "correct-horse", and every exact-match scrub afterwards sees a
// string it has never been told about. The same holds for any password holding
// characters the strip deletes.
//
// So: scrub, strip, scrub, cap, scrub. The first scrub sees the remote text as
// it arrived, the middle one makes the cap safe, and the last one is what
// everything persisted or displayed is held to. The scrubber itself also knows
// the normalized forms of the password (see passwordScrubber), so neither half
// of the rule is carrying this alone.
func (r *adoptionRun) safeRemoteText(raw string) string {
	return r.scrub(capMachineName(r.scrub(stripUnsafeRunes(r.scrub(raw)))))
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
	if active, err := s.store.FleetUpgradeMaintenanceActive(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	} else if active {
		if run, runErr := s.store.LatestFleetUpgradeRun(r.Context()); runErr == nil && run.State == "failed" {
			writeError(w, http.StatusConflict, errors.New("a fleet upgrade failed and needs attention; resolve it from the update screen before adding a machine"))
			return
		}
		writeError(w, http.StatusConflict, errors.New("fleet maintenance is active; wait until every node runs the target version before adding a machine"))
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
	// The address this run is pinned to. Everything after this point talks to
	// this IP and never resolves the typed name again.
	pinned, err := checkAdoptionTarget(r.Context(), address)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// A manager without Phase B membership still has the one-peer limit. A
	// fleet-capable controller admits additional machines through fleet_nodes
	// while leaving peers as the designated compatibility worker for the
	// existing two-node executor.
	peers, err := s.store.Peers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(peers) > 0 && s.fleetManager == nil {
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
		s.runAdoption(ctx, run, address, pinned, username, request.Password)
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
//
// It returns the one address the rest of the run is pinned to. Checking a name
// and then letting every later step look it up again is not checking it: a
// name whose answers the attacker controls can be private for the check and
// loopback or public for the SSH login that carries the owner's password, the
// install, and the fleet key. Every record has to pass, so a mixed answer is
// still refused, and exactly one of them is what this run then talks to.
func checkAdoptionTarget(ctx context.Context, address string) (net.IP, error) {
	resolved, err := resolveHost(ctx, address)
	if err != nil || len(resolved) == 0 {
		return nil, fmt.Errorf("this Spark could not find %s on the network, so check the address", address)
	}
	own := localIPs()
	for _, ip := range resolved {
		if ip.IsLoopback() {
			return nil, errSelfAdoption
		}
		for _, mine := range own {
			if mine.Equal(ip) {
				return nil, errSelfAdoption
			}
		}
	}
	for _, ip := range resolved {
		if !discovery.IsLocalFabric(ip) {
			return nil, fmt.Errorf("%s is not on your own network or tailnet, and basement only adopts machines that are", address)
		}
	}
	return resolved[0], nil
}

// sshDialAddress spells a pinned address the way setup.DialSSH expects it. An
// IPv6 literal already holds colons, and DialSSH reads a colon as "the port is
// in there", so that one case is handed over with its port already attached.
func sshDialAddress(ip net.IP) string {
	if ip.To4() == nil {
		return net.JoinHostPort(ip.String(), "22")
	}
	return ip.String()
}

// runAdoption is the whole flow, one step at a time, each reported as it
// happens. Nothing is written to the store until the last step, so a failure
// anywhere leaves no peer row behind.
//
// address is what the owner typed and is only ever read by a person. pinned is
// the address the handler validated, and it is the only thing this run
// connects to: the SSH login, the console wait, the pairing, the fleet key and
// the recorded peer URL are all built from it, so a name whose answers change
// between the check and the run cannot move any of them.
func (s *Server) runAdoption(ctx context.Context, run *adoptionRun, address string, pinned net.IP, username, password string) {
	target := pinned.String()
	// What the owner reads names both spellings when they differ, so a run
	// against a hostname says which address it settled on.
	where := address
	if target != address {
		where = address + " (" + target + ")"
	}
	run.begin("connect")
	run.progress("connecting to %s as %s", where, username)
	runner, closeRunner, err := adoptDial(ctx, sshDialAddress(pinned), username, password)
	if err != nil {
		run.fail("connect", fmt.Sprintf("could not sign in to %s as %s, so check the address, the username and the password", address, username), err)
		return
	}
	defer closeRunner()
	run.done("connect", "signed in as "+username)

	run.begin("verify")
	identity := adoptProbe(ctx, runner)
	if !identity.IsGB10() {
		found := run.safeRemoteText(identity.Product())
		if gpu := run.safeRemoteText(identity.GPUName); gpu != "" && gpu != found {
			found += " with " + gpu
		}
		run.fail("verify", fmt.Sprintf("%s is not a GB10 machine (it reports %s), so basement will not install there", address, found), nil)
		return
	}
	// The peer's name in this console is the name the machine reports for
	// itself. That machine is not ours yet and its answers are whatever it
	// chose to answer, so the name goes through safeRemoteText, which holds it
	// to printable, bounded text and scrubs the typed password out of it last,
	// before it is stored or shown anywhere.
	name := run.safeRemoteText(identity.Hostname)
	if name == "" {
		name = address
	}
	product := run.safeRemoteText(identity.Product())
	run.done("verify", product)
	run.progress("confirmed %s (%s)", product, name)

	run.begin("install")
	source, err := adoptBinarySource()
	if err != nil {
		run.fail("install", "this Spark cannot install basement on another machine", err)
		return
	}
	listen := s.siblingListenMode()
	run.progress("installing this manager's own build, listening on %s", listen)
	result, err := adoptInstall(ctx, runner, source, setup.Options{Listen: listen, ConsoleHost: target}, func(format string, args ...any) {
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
	// Everything from here on talks to the pinned address: the one this
	// manager validated and then signed in to over SSH, spelled as a literal
	// so no name is looked up twice. Never an address the machine reported for
	// itself either. A hostile SSH endpoint can answer `hostname -I` or
	// `tailscale ip` with an accomplice's address; believing it would point
	// the pairing, the fleet key and the stored peer row at that other host.
	consoleURL := consoleBaseURL(target)
	// Addresses the machine reported are kept for display only, and only
	// when they are a bare origin like every other peer URL this manager
	// accepts. Scrubbed after that parse rather than before it: parsing
	// unescapes what it is given, so a reported URL is one more way to hand
	// back a password with its bytes rearranged.
	altURL := ""
	for _, reported := range []string{result.AltURL, result.ConsoleURL} {
		if alternate, err := normalizedPeerBaseURL(reported); err == nil && alternate != consoleURL {
			altURL = run.scrub(alternate)
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
		run.fail("pair", fmt.Sprintf("%s did not produce a pairing token, so this Spark cannot pair with it", address), nil)
		return
	}
	var adoptedNode *store.FleetNode
	if s.fleetManager != nil {
		joinCode, err := s.newSparkJoinCode(ctx, consoleURL, result.Token)
		if err != nil {
			run.fail("pair", fmt.Sprintf("could not create a fleet join code on %s", consoleURL), err)
			return
		}
		nodeURL, err := adjacentFleetNodeURL(consoleURL)
		if err != nil {
			run.fail("pair", fmt.Sprintf("could not derive the fleet address for %s", consoleURL), err)
			return
		}
		adopted, err := s.fleetManager.Adopt(ctx, fleet.AdoptRequest{DisplayName: name, ConsoleURL: consoleURL, NodeURL: nodeURL, JoinCode: joinCode.Code})
		if err != nil {
			run.fail("pair", fmt.Sprintf("could not adopt the manager on %s", consoleURL), err)
			return
		}
		adoptedNode = &adopted.Node
		run.done("pair", "manager identity pinned for "+name)
		peers, err := s.store.Peers(ctx)
		if err != nil {
			run.fail("peer", "could not read this Spark's compatibility worker", err)
			return
		}
		if len(peers) > 0 {
			run.begin("peer")
			run.done("peer", name+" added as a read-only fleet member")
			run.succeed(adoptionResult{Node: adoptedNode, ConsoleURL: consoleURL, AltURL: altURL, OwnerPairingURL: consoleURL, OwnerPairingToken: result.Token})
			return
		}
	}
	key, err := s.mintFleetKey(ctx, consoleURL, result.Token)
	if err != nil {
		message := fmt.Sprintf("could not pair with the new console on %s", consoleURL)
		// A mint request that was sent is the point of no return, whatever
		// came back: the other machine may have committed the key before the
		// connection dropped or before it answered with something this manager
		// could not read. mintFleetKey says so by handing back the bootstrap
		// session, and the key is chased down and handed in the same way a
		// later failure hands one back.
		if key.pending() {
			run.failNoted("pair", message, err, s.revokeFleetKey(ctx, consoleURL, key))
			return
		}
		run.fail("pair", message, err)
		return
	}
	if adoptedNode == nil {
		run.done("pair", "fleet key created on "+name)
	}

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
		Node:              adoptedNode,
		ConsoleURL:        consoleURL,
		AltURL:            altURL,
		OwnerPairingURL:   consoleURL,
		OwnerPairingToken: result.Token,
	})
}

func (s *Server) newSparkJoinCode(ctx context.Context, baseURL, token string) (fleet.JoinCode, error) {
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return fleet.JoinCode{}, err
	}
	response, err := s.callNewSpark(ctx, http.MethodPost, baseURL, "/api/v1/auth/pair", body, nil)
	if err != nil {
		return fleet.JoinCode{}, err
	}
	var paired struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(response.body, &paired); err != nil || paired.CSRF == "" {
		return fleet.JoinCode{}, errors.New("the installed manager did not accept its pairing token")
	}
	pairs := make([]string, 0, len(response.cookies))
	for _, cookie := range response.cookies {
		pairs = append(pairs, cookie.Name+"="+cookie.Value)
	}
	session := strings.Join(pairs, "; ")
	if session == "" {
		return fleet.JoinCode{}, errors.New("the installed manager did not open a console session")
	}
	response, err = s.callNewSpark(ctx, http.MethodPost, baseURL, "/api/v1/fleet/join-code", nil, map[string]string{
		"Cookie": session, "X-CSRF-Token": paired.CSRF,
	})
	if err != nil {
		return fleet.JoinCode{}, err
	}
	var code fleet.JoinCode
	if err := json.Unmarshal(response.body, &code); err != nil || code.Code == "" {
		return fleet.JoinCode{}, errors.New("the installed manager returned an unreadable fleet join code")
	}
	return code, nil
}

func adjacentFleetNodeURL(consoleURL string) (string, error) {
	parsed, err := url.Parse(consoleURL)
	if err != nil || parsed.Host == "" {
		return "", errors.New("the installed manager console URL is invalid")
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return "", errors.New("the installed manager console URL has no explicit port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port >= 65535 {
		return "", errors.New("the installed manager console port cannot address its fleet listener")
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port+1)), nil
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
	// The name this run's key will carry, unique to this run, and the ids that
	// were on that machine before this run minted anything. Both are decided
	// here, before the request is sent, because both are what a later cleanup
	// proves authorship with. The snapshot is best effort: a machine that will
	// not list its keys is not a reason to refuse to adopt it.
	name := newFleetKeyName()
	existing := s.snapshotKeyIDs(ctx, baseURL, session)
	body, err = json.Marshal(map[string]string{"name": name})
	if err != nil {
		return fleetKey{}, err
	}
	// Sending this request is the point of no return. The other machine commits
	// the key and then answers, so a transport error or an answer this manager
	// cannot read tells us nothing about whether the key exists: only that we
	// will not be told its id. Every failure from here carries the bootstrap
	// session, the name and the snapshot back, so the caller can go and find out
	// what actually happened rather than guess.
	pending := fleetKey{name: name, session: session, csrf: paired.CSRF, existing: existing}
	response, err = s.callNewSpark(ctx, http.MethodPost, baseURL, "/api/v1/keys", body, map[string]string{
		"Cookie":       session,
		"X-CSRF-Token": paired.CSRF,
	})
	if err != nil {
		return pending, err
	}
	var created struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(response.body, &created); err != nil || created.Secret == "" {
		// It answered the key request successfully and then said nothing
		// usable. The id is taken if the answer happened to carry one.
		pending.id = created.Key.ID
		return pending, errors.New("it accepted the request for a fleet key and did not return one")
	}
	minted := pending
	minted.secret = created.Secret
	minted.id = created.Key.ID
	return minted, nil
}

// snapshotKeyIDs records which keys the other machine already held, so a
// cleanup can tell a key this run created from a key that was already there.
// A nil answer means the question could not be asked, which is different from
// an answer of none.
func (s *Server) snapshotKeyIDs(ctx context.Context, baseURL, session string) map[string]bool {
	listed, ok := s.listKeys(ctx, baseURL, session)
	if !ok {
		return nil
	}
	ids := make(map[string]bool, len(listed))
	for _, entry := range listed {
		ids[entry.ID] = true
	}
	return ids
}

// listedKey is one entry of the other machine's key listing.
type listedKey struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// listKeys reads the other machine's keys with the bootstrap session. The
// second return says whether the listing was read at all: an empty list and a
// failed request are opposite answers and must never be confused, because one
// of them is proof that nothing was left behind and the other is proof of
// nothing.
func (s *Server) listKeys(ctx context.Context, baseURL, session string) ([]listedKey, bool) {
	response, err := s.callNewSpark(ctx, http.MethodGet, baseURL, "/api/v1/keys", nil, map[string]string{
		"Cookie": session,
	})
	if err != nil {
		return nil, false
	}
	var listed []listedKey
	if json.Unmarshal(response.body, &listed) != nil {
		return nil, false
	}
	return listed, true
}

// fleetKey is the API key this manager created on the machine it is adopting,
// together with everything it takes to prove later that the key is this run's
// own. The session is held so that a run failing after this point can hand the
// key straight back instead of leaving it on that machine forever, unseen by
// the owner and unremovable without a second bootstrap. The name and the
// snapshot are held because handing back a key means naming one, and naming
// the wrong one deletes a credential this run never created.
type fleetKey struct {
	secret  string
	id      string
	name    string
	session string
	csrf    string
	// existing is every key id that was on that machine before this run minted
	// anything. A nil map means the listing could not be read, which is not the
	// same as a machine with no keys on it.
	existing map[string]bool
}

// pending reports that the request to mint the key was sent, so a key may
// exist on the other machine whether or not this manager ever saw its id.
// The bootstrap session is what says so: mintFleetKey only fills it in once
// there is something on the other side worth cleaning up.
func (k fleetKey) pending() bool { return k.session != "" }

// predates reports that id was already on the other machine before this run
// minted anything, so this run cannot have created it. Unknown when the
// snapshot could not be taken, in which case the name carries the proof alone.
func (k fleetKey) predates(id string) bool { return k.existing != nil && k.existing[id] }

// revokeFleetKey deletes the key this manager minted on the other machine and
// returns the sentence that says what happened, for the failure the owner is
// about to read. Best effort by nature: the machine may already be gone.
//
// It deletes a key only when it can prove that key is this run's own, and it
// deletes nothing at all otherwise. That is not caution for its own sake. The
// other machine allows duplicate key names and lists them oldest first, so a
// cleanup that deleted the first key carrying this head's name would, whenever
// the owner already had one, delete the older legitimate key, report success,
// and leave the key this run just minted behind and orphaned: the exact
// opposite of what a cleanup is for. Two things have to agree instead. The name
// is unique to this run (newFleetKeyName), so nothing the owner made by hand
// and nothing an earlier adoption left can carry it; and the id must be absent
// from the snapshot taken before this run minted anything, so a key that was
// already there is never a candidate however it is named.
//
// When neither the delete nor the proof can be had, the sentence says so
// plainly and names what to look for and where, rather than claiming a cleanup
// that did not happen.
func (s *Server) revokeFleetKey(ctx context.Context, baseURL string, key fleetKey) string {
	unproven := "This Spark could not tell which fleet key it created on that machine, so look for a key named " + key.name + " under Connect on that Spark's own console and delete it if it is there"
	orphaned := "A fleet key named " + key.name + " was left on that machine and could not be removed, so delete it under Connect on that Spark's own console"
	if key.session == "" {
		return unproven
	}
	// Detached from the run's context: an adoption that failed because it ran
	// out of budget should still get its key back.
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	id := key.id
	// An id the other machine handed back is still checked against the
	// snapshot. A machine that answers the mint with the id of a key it already
	// had is either confused or lying, and either way that id is not proof.
	if id != "" && key.predates(id) {
		id = ""
	}
	if id == "" {
		found, known := s.findMintedKeyID(revokeCtx, baseURL, key)
		switch {
		case !known:
			return unproven
		case found == "":
			// The listing was read and holds no key this run created, which is
			// what a mint request that never arrived looks like from here.
			return "No fleet key was left on that machine"
		default:
			id = found
		}
	}
	_, err := s.callNewSpark(revokeCtx, http.MethodDelete, baseURL, "/api/v1/keys/"+url.PathEscape(id), nil, map[string]string{
		"Cookie":       key.session,
		"X-CSRF-Token": key.csrf,
	})
	if err != nil {
		return orphaned
	}
	return "The fleet key this Spark had created on that machine has been removed"
}

// findMintedKeyID asks the other machine to list its keys and picks out the one
// this run created, which is the only one it may ever delete: a key carrying
// this run's own name and absent from the snapshot taken before the mint.
//
// The second return says whether the listing was read. An empty id with a true
// beside it is a real answer, the machine holds nothing this run created, and
// the pre-mint failure path depends on it: a mint request that never arrived
// left nothing behind and nothing may be deleted in its name. A false means the
// question could not be answered, and nothing may be deleted then either.
// Ambiguity counts as unanswered: two keys this run cannot tell apart are two
// keys it cannot claim, and picking one is a guess with a credential at stake.
func (s *Server) findMintedKeyID(ctx context.Context, baseURL string, key fleetKey) (string, bool) {
	if key.name == "" {
		return "", false
	}
	listed, ok := s.listKeys(ctx, baseURL, key.session)
	if !ok {
		return "", false
	}
	found := ""
	for _, entry := range listed {
		if entry.ID == "" || entry.Name != key.name || key.predates(entry.ID) {
			continue
		}
		if found != "" && found != entry.ID {
			return "", false
		}
		found = entry.ID
	}
	return found, true
}

// maxFleetKeyNameLength is what the other machine's key store accepts.
const maxFleetKeyNameLength = 64

// fleetKeySuffix is what makes one run's key name that run's own. It is
// random rather than derived from anything, so a name match on the other
// machine is proof of authorship: no earlier adoption, no key the owner typed
// in by hand and no second head can be carrying it. A seam so tests can know
// the name a run will use.
var fleetKeySuffix = func() string {
	var raw [4]byte
	// crypto/rand.Read does not return an error on any supported platform; it
	// fails the process instead. The branch is here so a future where it can
	// still produces a name rather than an empty suffix, and the clock is
	// unpredictable enough for that never-taken case.
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 16)
	}
	return hex.EncodeToString(raw[:])
}

// fleetKeyBaseName names the key after this machine, so the owner reading the
// other console's Connect tab can tell whose key it is and what it is for.
func fleetKeyBaseName() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "basement"
	}
	short, _, _ := strings.Cut(host, ".")
	return capText("fleet-"+short, maxFleetKeyNameLength)
}

// newFleetKeyName is the name one adoption gives the key it mints:
// "fleet-<head hostname>-<random>". The head's name is still the first thing
// the owner reads in the other Spark's Connect tab, because that tab is their
// UI and "fleet-spark-head-4b7d1e02" tells them whose key it is; the suffix is
// what lets this manager prove, later, that a key on that machine is the one
// this run created and not one that happened to be named the same.
func newFleetKeyName() string {
	suffix := fleetKeySuffix()
	return capText(fleetKeyBaseName(), maxFleetKeyNameLength-len(suffix)-1) + "-" + suffix
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
	// door both kinds come through, so it is held to shape here. Adoption has
	// already run its name through safeRemoteText, and this is idempotent on
	// text that has been through it: nothing is stripped or cut a second time,
	// so it cannot rearrange a name back into a secret on the way to the store.
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
