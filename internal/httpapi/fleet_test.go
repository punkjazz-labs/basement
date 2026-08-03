package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/discovery"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/setup"
	"github.com/punkjazz-labs/basement/internal/store"
)

// fleetFixture is a manager with a paired console session, which is the only
// authority these endpoints accept.
type fleetFixture struct {
	api     *Server
	server  *httptest.Server
	store   *store.Store
	cookies []*http.Cookie
	csrf    string
	token   string
}

func newFleetFixture(t *testing.T) *fleetFixture {
	t.Helper()
	dataDir := t.TempDir()
	database, err := store.Open(filepath.Join(dataDir, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	authManager, err := auth.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	executor := &apiExecutor{done: map[string]bool{}}
	api := New("test-version", dataDir, authManager, database, readyInventory{}, executor, engine.New(database, executor, recipes), recipes)
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)

	raw, err := os.ReadFile(authManager.PairingTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	fixture := &fleetFixture{api: api, server: server, store: database, token: token}
	response := doRequest(t, http.MethodPost, server.URL+"/api/v1/auth/pair", `{"token":"`+token+`"}`, nil, map[string]string{"Origin": server.URL})
	var paired struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&paired); err != nil {
		t.Fatal(err)
	}
	fixture.cookies = response.Cookies()
	fixture.csrf = paired.CSRF
	response.Body.Close()
	if fixture.csrf == "" || len(fixture.cookies) == 0 {
		t.Fatal("pairing did not open a console session")
	}
	return fixture
}

// call makes an authenticated console call with CSRF, the way the browser does.
func (f *fleetFixture) call(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	headers := map[string]string{"Origin": f.server.URL, "X-CSRF-Token": f.csrf}
	response := doRequest(t, method, f.server.URL+path, body, f.cookies, headers)
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, payload
}

func (f *fleetFixture) status(t *testing.T) adoptionView {
	t.Helper()
	code, payload := f.call(t, http.MethodGet, "/api/v1/fleet/adopt/status", "")
	if code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", code, payload)
	}
	var view adoptionView
	if err := json.Unmarshal(payload, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

// waitAdoption polls the status endpoint the way the console does, until the
// run stops being in flight.
func (f *fleetFixture) waitAdoption(t *testing.T) adoptionView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		view := f.status(t)
		if view.State != adoptionRunning {
			return view
		}
		if time.Now().After(deadline) {
			t.Fatalf("adoption never finished: %+v", view)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stubRunner stands in for an SSH session to the machine being adopted.
type stubRunner struct{}

func (stubRunner) Run(context.Context, string, io.Reader) (string, error)           { return "", nil }
func (stubRunner) RunPrivileged(context.Context, string, io.Reader) (string, error) { return "", nil }
func (stubRunner) Describe() string                                                 { return "fake machine" }

// holdSeams restores every adoption seam after the test, so one test's fake
// network can never leak into another's, and stands this machine on the fixed
// network these tests describe: the head is 192.168.99.134, and nothing else
// in 192.168.99.0/24 belongs to it. Without that, whether a test passes would
// depend on the addresses of the laptop running it.
func holdSeams(t *testing.T) {
	t.Helper()
	discoverBefore, probeBefore, installBefore := discoverCandidates, adoptProbe, adoptInstall
	dialBefore, sourceBefore := adoptDial, adoptBinarySource
	urlBefore, selfBefore := consoleBaseURL, selfAddresses
	resolveBefore, localBefore := resolveHost, localIPs
	suffixBefore := fleetKeySuffix
	t.Cleanup(func() {
		discoverCandidates, adoptProbe, adoptInstall = discoverBefore, probeBefore, installBefore
		adoptDial, adoptBinarySource = dialBefore, sourceBefore
		consoleBaseURL, selfAddresses = urlBefore, selfBefore
		resolveHost, localIPs = resolveBefore, localBefore
		fleetKeySuffix = suffixBefore
	})
	localIPs = func() []net.IP { return []net.IP{net.ParseIP("192.168.99.134")} }
}

// gb10Machine is what Probe reports for a real second Spark.
func gb10Machine() setup.Identity {
	return setup.Identity{Hostname: "spark-worker", GPUName: "NVIDIA GB10", DeviceModel: "NVIDIA DGX Spark", OSName: "DGX OS 7"}
}

// fakeSpark is the machine on the other end: it answers the console
// fingerprint, the health check, pairing, key creation and the fleet
// reachability handshake exactly the way a real basement manager does.
type fakeSpark struct {
	server     *httptest.Server
	token      string
	secret     string
	hostname   string
	mu         sync.Mutex
	pairs      int
	badOrigins int
	requests   int
	revoked    []string
	attempted  []string
	minted     []map[string]string
	// refuseHandshake makes the reachability check at the end of adoption
	// fail, which is how a test injects a failure after the fleet key exists.
	refuseHandshake bool
	// mintFailure breaks a key creation: "transport" drops the connection after
	// the key is committed and "garbage" answers 201 with a body nothing can
	// parse, so in both the key exists here and the caller never learns its id.
	// "unreachable" drops the connection before the key is written, which is
	// the request that never arrived: nothing was created and nothing may be
	// deleted in its name.
	mintFailure string
	// mintDecoy makes a key creation commit a second key under the same name,
	// so the caller cannot tell which of the two it asked for.
	mintDecoy bool
	// refuseListsBefore is how many key listings fail before the rest answer.
	// One of them is what a run whose pre-mint snapshot never happened looks
	// like from the manager's side.
	refuseListsBefore int
	// refuseDelete makes key removal fail, which is how a test reaches the
	// sentence the owner is left to act on themselves.
	refuseDelete bool
}

// seed puts a key on this machine before an adoption starts: a key from an
// earlier adoption, or one the owner made by hand.
func (s *fakeSpark) seed(id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.minted = append(s.minted, map[string]string{"id": id, "name": name})
}

// keyNames is every key still on this machine, in listing order.
func (s *fakeSpark) keyNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.minted))
	for _, entry := range s.minted {
		names = append(names, entry["id"]+"="+entry["name"])
	}
	return names
}

func newFakeSpark(t *testing.T, token string) *fakeSpark {
	t.Helper()
	spark := &fakeSpark{token: token, secret: "rosk_fleetkeyfleetkeyfleetkey", hostname: "spark-worker"}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/api/v1/auth/pair", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host {
			spark.mu.Lock()
			spark.badOrigins++
			spark.mu.Unlock()
			writeError(w, http.StatusForbidden, errors.New("cross-origin mutation denied"))
			return
		}
		var request struct {
			Token string `json:"token"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Token != spark.token {
			writeError(w, http.StatusUnauthorized, errors.New("invalid pairing token"))
			return
		}
		// Like the real auth manager, the token is compared and kept: it is a
		// file-backed shared secret, not a one-shot nonce.
		spark.mu.Lock()
		spark.pairs++
		spark.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "basement_session", Value: "session-value", Path: "/"})
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": "csrf-value"})
	})
	mux.HandleFunc("/api/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("basement_session")
		if err != nil || cookie.Value != "session-value" {
			writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		// Listing, the same shape this manager serves it in: a bare array of
		// keys, oldest first, and no CSRF, because it changes nothing.
		if r.Method == http.MethodGet {
			spark.mu.Lock()
			if spark.refuseListsBefore > 0 {
				spark.refuseListsBefore--
				spark.mu.Unlock()
				writeError(w, http.StatusInternalServerError, errors.New("the keys could not be listed"))
				return
			}
			listed := append([]map[string]string(nil), spark.minted...)
			spark.mu.Unlock()
			writeJSON(w, http.StatusOK, listed)
			return
		}
		if r.Header.Get("X-CSRF-Token") != "csrf-value" {
			writeError(w, http.StatusForbidden, errors.New("valid CSRF token required"))
			return
		}
		var request struct {
			Name string `json:"name"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Name == "" {
			writeError(w, http.StatusBadRequest, errors.New("a key name is required"))
			return
		}
		// The request that never arrived: the connection drops before anything
		// is written, so this machine holds no key from it.
		spark.mu.Lock()
		if spark.mintFailure == "unreachable" {
			spark.mu.Unlock()
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					conn.Close()
				}
			}
			return
		}
		// The key is committed before anything is said about it, which is the
		// order a real manager writes in and the reason a failed answer does
		// not mean a failed creation.
		id := fmt.Sprintf("key_%d", len(spark.minted)+1)
		spark.minted = append(spark.minted, map[string]string{"id": id, "name": request.Name})
		if spark.mintDecoy {
			spark.minted = append(spark.minted, map[string]string{
				"id":   fmt.Sprintf("key_%d", len(spark.minted)+1),
				"name": request.Name,
			})
		}
		failure := spark.mintFailure
		spark.mu.Unlock()
		switch failure {
		case "transport":
			// The connection drops after the write, before any answer.
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					conn.Close()
				}
			}
		case "garbage":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"key":{"id":`))
		default:
			writeJSON(w, http.StatusCreated, map[string]any{"key": map[string]any{"id": id, "name": request.Name}, "secret": spark.secret})
		}
	})
	// Key revocation, the same shape this manager serves it in.
	mux.HandleFunc("/api/v1/keys/", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("basement_session")
		if r.Method != http.MethodDelete || err != nil || cookie.Value != "session-value" || r.Header.Get("X-CSRF-Token") != "csrf-value" {
			writeError(w, http.StatusForbidden, errors.New("valid CSRF token required"))
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/keys/")
		spark.mu.Lock()
		spark.attempted = append(spark.attempted, id)
		refused := spark.refuseDelete
		if !refused {
			spark.revoked = append(spark.revoked, id)
			kept := spark.minted[:0]
			for _, entry := range spark.minted {
				if entry["id"] != id {
					kept = append(kept, entry)
				}
			}
			spark.minted = kept
		}
		spark.mu.Unlock()
		if refused {
			writeError(w, http.StatusInternalServerError, errors.New("the key could not be removed"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
	})
	mux.HandleFunc("/api/v1/system", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+spark.secret || spark.refuseHandshake {
			writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"hostname": spark.hostname, "dgx_spark": true})
	})
	spark.server = httptest.NewServer(&countingHandler{spark: spark, next: mux})
	t.Cleanup(spark.server.Close)
	return spark
}

// countingHandler records that this machine was talked to at all, which is
// what an accomplice in the bootstrap-redirection tests is measured by.
type countingHandler struct {
	spark *fakeSpark
	next  http.Handler
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.spark.mu.Lock()
	h.spark.requests++
	h.spark.mu.Unlock()
	h.next.ServeHTTP(w, r)
}

func (s *fakeSpark) called() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *fakeSpark) handshakes() (pairs, badOrigins int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pairs, s.badOrigins
}

func (s *fakeSpark) revocations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.revoked...)
}

// deleteAttempts is every key this machine was asked to remove, whether or not
// the removal was allowed to work.
func (s *fakeSpark) deleteAttempts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.attempted...)
}

func TestFleetDiscoverFingerprintsBasementAndExcludesThisMachine(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	basement := newFakeSpark(t, "irrelevant")
	stranger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>denied</html>"))
	}))
	defer stranger.Close()

	discoverCandidates = func(context.Context, func(string, ...any)) ([]discovery.Candidate, error) {
		return []discovery.Candidate{
			{IP: net.ParseIP("192.168.99.134"), Hostname: "spark-head.local"},
			{IP: net.ParseIP("192.168.99.137"), Hostname: "spark-worker.local"},
			{IP: net.ParseIP("192.168.99.200"), Hostname: "printer.local"},
		}, nil
	}
	selfAddresses = func() map[string]bool { return map[string]bool{"192.168.99.134": true, "spark-head": true} }
	consoleBaseURL = func(address string) string {
		switch address {
		case "192.168.99.137":
			return basement.server.URL
		case "192.168.99.200":
			return stranger.URL
		}
		return "http://" + net.JoinHostPort(address, consolePort)
	}

	code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/discover", "{}")
	if code != http.StatusOK {
		t.Fatalf("discover code = %d, body = %s", code, payload)
	}
	var result struct {
		Candidates []discoveredCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 2 {
		t.Fatalf("candidates = %+v, want this machine excluded", result.Candidates)
	}
	spark, printer := result.Candidates[0], result.Candidates[1]
	if spark.Name != "spark-worker" || spark.Address != "192.168.99.137" || !spark.GB10Hint {
		t.Errorf("GB10 candidate = %+v", spark)
	}
	if spark.Basement == nil || spark.Basement.BaseURL != basement.server.URL {
		t.Errorf("a running basement was not recognized: %+v", spark.Basement)
	}
	if printer.GB10Hint {
		t.Errorf("printer.local should not read as GB10-class: %+v", printer)
	}
	if printer.Basement != nil {
		t.Errorf("a 401 that is not basement's error shape must not read as basement: %+v", printer.Basement)
	}
	if !strings.Contains(string(payload), `"basement":null`) {
		t.Errorf("a machine without basement must report basement: null, got %s", payload)
	}
	// Safe to call repeatedly: nothing about the first call changes the second.
	if code, _ := fixture.call(t, http.MethodPost, "/api/v1/fleet/discover", "{}"); code != http.StatusOK {
		t.Errorf("second discover code = %d", code)
	}
}

func TestFleetAdoptInstallsPairsAndRecordsThePeer(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	fixture.api.SetListenAddress("192.168.99.134:7070")
	spark := newFakeSpark(t, "pairing-token-value")

	// The machine being adopted lives at 192.168.99.137 on this test's
	// network, and that address is where its console is reached.
	consoleBaseURL = func(address string) string {
		if address == "192.168.99.137" {
			return spark.server.URL
		}
		return "http://" + net.JoinHostPort(address, consolePort)
	}
	adoptDial = func(_ context.Context, address, username, _ string) (setup.Runner, func(), error) {
		if address != "192.168.99.137" || username != "nvidia" {
			t.Errorf("dialled %s@%s", username, address)
		}
		return stubRunner{}, func() {}, nil
	}
	adoptProbe = func(context.Context, setup.Runner) setup.Identity { return gb10Machine() }
	adoptBinarySource = func() (setup.BinarySource, error) {
		return setup.LocalFileSource{Path: "/usr/lib/basement/basement"}, nil
	}
	var installedWith setup.Options
	adoptInstall = func(_ context.Context, _ setup.Runner, _ setup.BinarySource, opts setup.Options, logf func(string, ...any)) (setup.InstallResult, error) {
		installedWith = opts
		logf("installing binary and service user")
		return setup.InstallResult{ConsoleURL: spark.server.URL, Token: "pairing-token-value"}, nil
	}

	code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
		`{"address":"192.168.99.137","username":"nvidia","password":"typed-by-the-owner"}`)
	if code != http.StatusAccepted {
		t.Fatalf("adopt code = %d, body = %s", code, payload)
	}
	view := fixture.waitAdoption(t)
	if view.State != adoptionSucceeded {
		t.Fatalf("state = %s, error = %q, steps = %+v", view.State, view.Error, view.Steps)
	}
	if installedWith.Listen != setup.ListenLAN {
		t.Errorf("sibling listen mode = %q, want the head's own lan mode", installedWith.Listen)
	}
	if installedWith.ConsoleHost != "192.168.99.137" {
		t.Errorf("install was not anchored to the adopted address: %q", installedWith.ConsoleHost)
	}
	for _, step := range view.Steps {
		if step.State != stepDone {
			t.Errorf("step %s = %s, want every step done", step.Key, step.State)
		}
	}
	if len(view.Progress) == 0 {
		t.Error("the install produced no progress lines")
	}
	if view.Result == nil {
		t.Fatal("a successful adoption reported no result")
	}
	if view.Result.ConsoleURL != spark.server.URL || view.Result.OwnerPairingURL != spark.server.URL {
		t.Errorf("result URLs = %+v", view.Result)
	}
	if view.Result.OwnerPairingToken != "pairing-token-value" {
		t.Errorf("the owner was not given a way into the new console: %+v", view.Result)
	}
	if pairs, bad := spark.handshakes(); pairs != 1 || bad != 0 {
		t.Errorf("pairs = %d, bad origins = %d", pairs, bad)
	}
	peers, err := fixture.store.Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Name != "spark-worker" || peers[0].BaseURL != spark.server.URL {
		t.Fatalf("peers = %+v", peers)
	}
	if peers[0].ID != view.Result.Peer.ID {
		t.Errorf("the reported peer is not the stored one: %+v vs %+v", view.Result.Peer, peers[0])
	}
	_, storedKey, err := fixture.store.PeerCredentials(context.Background(), peers[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedKey != spark.secret {
		t.Errorf("stored key = %q, want the key the new Spark minted", storedKey)
	}
	// The status survives a reload: a fresh GET tells the same story.
	if again := fixture.status(t); again.State != adoptionSucceeded || again.Result == nil {
		t.Errorf("status after reload = %+v", again)
	}
}

func TestFleetAdoptRefusesWhenAPeerIsAlreadyConfigured(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	adoptDial = func(context.Context, string, string, string) (setup.Runner, func(), error) {
		t.Error("adoption dialled a machine even though a peer was already configured")
		return nil, func() {}, errors.New("unreachable")
	}
	if _, err := fixture.store.CreatePeer(context.Background(), "spark-worker", "http://192.168.99.137:7070", "rosk_existing"); err != nil {
		t.Fatal(err)
	}
	code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
		`{"address":"192.168.99.200","username":"nvidia","password":"typed-by-the-owner"}`)
	if code != http.StatusConflict {
		t.Fatalf("code = %d, body = %s", code, payload)
	}
	if !strings.Contains(string(payload), "already in the fleet") {
		t.Errorf("refusal did not explain itself: %s", payload)
	}
	if view := fixture.status(t); view.State != adoptionIdle {
		t.Errorf("a refused adoption started anyway: %+v", view)
	}
}

func TestFleetAdoptRefusesThisMachinesOwnAddress(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	selfAddresses = func() map[string]bool { return map[string]bool{"192.168.99.134": true, "spark-head": true} }
	adoptDial = func(context.Context, string, string, string) (setup.Runner, func(), error) {
		t.Error("adoption dialled this machine itself")
		return nil, func() {}, errors.New("unreachable")
	}
	for _, address := range []string{"192.168.99.134", "spark-head"} {
		code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
			`{"address":"`+address+`","username":"nvidia","password":"typed-by-the-owner"}`)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, body = %s", address, code, payload)
		}
		if !strings.Contains(string(payload), "this Spark itself") {
			t.Errorf("%s: refusal did not explain itself: %s", address, payload)
		}
	}
}

func TestFleetAdoptRefusesAMachineThatIsNotGB10(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	adoptDial = func(context.Context, string, string, string) (setup.Runner, func(), error) {
		return stubRunner{}, func() {}, nil
	}
	adoptProbe = func(context.Context, setup.Runner) setup.Identity {
		return setup.Identity{Hostname: "workshop", GPUName: "NVIDIA GeForce RTX 4090", OSName: "Ubuntu 24.04"}
	}
	adoptInstall = func(context.Context, setup.Runner, setup.BinarySource, setup.Options, func(string, ...any)) (setup.InstallResult, error) {
		t.Error("a machine that is not a GB10 was installed on anyway")
		return setup.InstallResult{}, nil
	}
	if code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
		`{"address":"192.168.99.200","username":"nvidia","password":"typed-by-the-owner"}`); code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %s", code, payload)
	}
	view := fixture.waitAdoption(t)
	if view.State != adoptionFailed {
		t.Fatalf("state = %s", view.State)
	}
	if !strings.Contains(view.Error, "not a GB10 machine") || !strings.Contains(view.Error, "RTX 4090") {
		t.Errorf("the refusal did not name what it found: %q", view.Error)
	}
	if view.Steps[1].State != stepFailed || view.Steps[0].State != stepDone {
		t.Errorf("steps = %+v", view.Steps)
	}
	peers, err := fixture.store.Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Errorf("a failed adoption left a peer row behind: %+v", peers)
	}
}

// The SSH password is the one secret the owner types into this console. It
// must not come back out of it, not through progress, not through status,
// and not through an error an SSH library built out of the credentials it
// was handed.
func TestFleetAdoptNeverReportsTheSSHPassword(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	const password = "correct-horse-battery-staple"
	adoptDial = func(_ context.Context, address, username, given string) (setup.Runner, func(), error) {
		return nil, func() {}, errors.New("ssh: handshake failed: password " + given + " rejected for " + username + "@" + address)
	}

	code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
		`{"address":"192.168.99.137","username":"nvidia","password":"`+password+`"}`)
	if code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %s", code, payload)
	}
	if strings.Contains(string(payload), password) {
		t.Fatalf("the accept response echoed the password: %s", payload)
	}
	view := fixture.waitAdoption(t)
	if view.State != adoptionFailed {
		t.Fatalf("state = %s", view.State)
	}
	if !strings.Contains(view.Error, "could not sign in") {
		t.Errorf("the failure did not read as a plain sentence: %q", view.Error)
	}
	if !strings.Contains(view.Error, "[redacted]") {
		t.Errorf("the echoed credential was not redacted: %q", view.Error)
	}
	// The whole status payload, byte for byte, including every progress line.
	_, raw := fixture.call(t, http.MethodGet, "/api/v1/fleet/adopt/status", "")
	if strings.Contains(string(raw), password) {
		t.Fatalf("the status payload contains the SSH password: %s", raw)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), password) {
		t.Fatalf("the adoption state holds the SSH password: %s", encoded)
	}
	jobs, err := fixture.store.ListJobs(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		encoded, _ := json.Marshal(job)
		if strings.Contains(string(encoded), password) {
			t.Fatalf("the jobs table holds the SSH password: %s", encoded)
		}
	}
}

func TestFleetAdoptRunsOneAtATime(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	release := make(chan struct{})
	adoptDial = func(context.Context, string, string, string) (setup.Runner, func(), error) {
		<-release
		return nil, func() {}, errors.New("ssh: connection refused")
	}
	body := `{"address":"192.168.99.137","username":"nvidia","password":"typed-by-the-owner"}`
	if code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt", body); code != http.StatusAccepted {
		t.Fatalf("first adopt code = %d, body = %s", code, payload)
	}
	code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt", body)
	if code != http.StatusConflict {
		t.Fatalf("second adopt code = %d, body = %s", code, payload)
	}
	if !strings.Contains(string(payload), "already adopting") {
		t.Errorf("the refusal did not explain itself: %s", payload)
	}
	close(release)
	if view := fixture.waitAdoption(t); view.State != adoptionFailed {
		t.Errorf("state = %s", view.State)
	}
	// The lane is free again once the first run ends.
	if code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt", body); code != http.StatusAccepted {
		t.Fatalf("third adopt code = %d, body = %s", code, payload)
	}
	fixture.waitAdoption(t)
}

// These three endpoints spend the owner's authority over their own machines.
// A fleet API key held by another manager is not that, and neither is an
// unauthenticated caller.
func TestFleetEndpointsRefuseBearerKeysAndStrangers(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	adoptDial = func(context.Context, string, string, string) (setup.Runner, func(), error) {
		t.Error("a bearer key started an adoption")
		return nil, func() {}, errors.New("unreachable")
	}
	discoverCandidates = func(context.Context, func(string, ...any)) ([]discovery.Candidate, error) {
		t.Error("a bearer key started a network sweep")
		return nil, nil
	}
	_, secret, err := fixture.store.CreateAPIKey(context.Background(), "other-spark")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/fleet/discover", "{}"},
		{http.MethodPost, "/api/v1/fleet/adopt", `{"address":"192.168.99.137","username":"nvidia","password":"x"}`},
		{http.MethodGet, "/api/v1/fleet/adopt/status", ""},
	}
	for _, test := range cases {
		bearer := doRequest(t, test.method, fixture.server.URL+test.path, test.body, nil,
			map[string]string{"Authorization": "Bearer " + secret, "Origin": fixture.server.URL})
		bearer.Body.Close()
		if bearer.StatusCode != http.StatusUnauthorized && bearer.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with a bearer key = %d, want a refusal", test.method, test.path, bearer.StatusCode)
		}
		anonymous := doRequest(t, test.method, fixture.server.URL+test.path, test.body, nil,
			map[string]string{"Origin": fixture.server.URL})
		anonymous.Body.Close()
		if anonymous.StatusCode != http.StatusUnauthorized && anonymous.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s unauthenticated = %d, want a refusal", test.method, test.path, anonymous.StatusCode)
		}
	}
	// A console session without its CSRF token is not enough for the two
	// mutations either.
	for _, path := range []string{"/api/v1/fleet/discover", "/api/v1/fleet/adopt"} {
		response := doRequest(t, http.MethodPost, fixture.server.URL+path, "{}", fixture.cookies,
			map[string]string{"Origin": fixture.server.URL})
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("%s without CSRF = %d", path, response.StatusCode)
		}
	}
	for _, path := range []string{"/api/v1/fleet/discover", "/api/v1/fleet/adopt", "/api/v1/fleet/adopt/status"} {
		if peerPathAllowed(http.MethodGet, path) || peerPathAllowed(http.MethodPost, path) {
			t.Errorf("%s is reachable through the peer allowlist", path)
		}
	}
}

// The finding this whole flow depends on: pairing compares the token and
// keeps it, so the head using it to mint a fleet key does not lock the owner
// out of the new console.
func TestPairingTokenSurvivesRepeatedPairing(t *testing.T) {
	fixture := newFleetFixture(t)
	for attempt := 0; attempt < 3; attempt++ {
		response := doRequest(t, http.MethodPost, fixture.server.URL+"/api/v1/auth/pair",
			`{"token":"`+fixture.token+`"}`, nil, map[string]string{"Origin": fixture.server.URL})
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("pairing attempt %d = %d, want the token to stay valid", attempt+1, response.StatusCode)
		}
	}
}

func TestAdoptBinarySourceRefusesAForeignBuild(t *testing.T) {
	source, err := adoptBinarySource()
	if runtime.GOOS == "linux" && runtime.GOARCH == "arm64" {
		if err != nil || source == nil {
			t.Fatalf("a linux/arm64 manager must be able to install itself: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("a %s/%s build offered itself as a GB10 install source", runtime.GOOS, runtime.GOARCH)
	}
	if !strings.Contains(err.Error(), "linux/arm64") {
		t.Errorf("the refusal did not say what is wrong: %v", err)
	}
}

func TestSiblingListenModeFollowsTheHead(t *testing.T) {
	fixture := newFleetFixture(t)
	cases := []struct {
		listen string
		want   setup.ListenMode
	}{
		{"192.168.99.134:7070", setup.ListenLAN},
		{"100.64.0.13:7070", setup.ListenTailscale},
		{"127.0.0.1:7070", setup.ListenLAN},
		{"", setup.ListenLAN},
	}
	for _, test := range cases {
		fixture.api.SetListenAddress(test.listen)
		if got := fixture.api.siblingListenMode(); got != test.want {
			t.Errorf("listen %q gave sibling mode %q, want %q", test.listen, got, test.want)
		}
	}
}

func TestAdoptionAddressRejectsAnythingButAHost(t *testing.T) {
	for _, bad := range []string{"", "  ", "http://192.168.99.137", "192.168.99.137:7070", "nvidia@spark", "spark/console", "spark a393"} {
		if _, err := adoptionAddress(bad); err == nil {
			t.Errorf("adoptionAddress(%q) was accepted", bad)
		}
	}
	for _, good := range []string{"192.168.99.137", "spark-worker", "spark-worker.local"} {
		if _, err := adoptionAddress(good); err != nil {
			t.Errorf("adoptionAddress(%q) = %v", good, err)
		}
	}
}

// The machine being adopted answers `hostname -I` and `tailscale ip`, and it
// is not ours yet. If this manager believed those answers, a hostile SSH
// endpoint could name an accomplice and get the pairing, the fleet key and
// the stored peer row pointed at that other host. The bootstrap follows the
// address the owner adopted and this manager actually signed in to.
func TestFleetAdoptBootstrapsTheAddressItSignedInTo(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	fixture.api.SetListenAddress("192.168.99.134:7070")
	spark := newFakeSpark(t, "pairing-token-value")
	accomplice := newFakeSpark(t, "pairing-token-value")
	accomplice.secret = "rosk_accomplicekeyaccomplicekey"

	consoleBaseURL = func(address string) string {
		if address == "192.168.99.137" {
			return spark.server.URL
		}
		return "http://" + net.JoinHostPort(address, consolePort)
	}
	adoptDial = func(context.Context, string, string, string) (setup.Runner, func(), error) {
		return stubRunner{}, func() {}, nil
	}
	adoptProbe = func(context.Context, setup.Runner) setup.Identity { return gb10Machine() }
	adoptBinarySource = func() (setup.BinarySource, error) {
		return setup.LocalFileSource{Path: "/usr/lib/basement/basement"}, nil
	}
	// The hostile install: every address it reports is the accomplice's.
	adoptInstall = func(context.Context, setup.Runner, setup.BinarySource, setup.Options, func(string, ...any)) (setup.InstallResult, error) {
		return setup.InstallResult{ConsoleURL: accomplice.server.URL, AltURL: accomplice.server.URL, Token: "pairing-token-value"}, nil
	}

	if code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
		`{"address":"192.168.99.137","username":"nvidia","password":"typed-by-the-owner"}`); code != http.StatusAccepted {
		t.Fatalf("adopt code = %d, body = %s", code, payload)
	}
	view := fixture.waitAdoption(t)
	if view.State != adoptionSucceeded {
		t.Fatalf("state = %s, error = %q", view.State, view.Error)
	}
	if accomplice.called() != 0 {
		t.Errorf("the address the target reported was contacted %d times", accomplice.called())
	}
	if pairs, _ := spark.handshakes(); pairs != 1 {
		t.Errorf("the adopted machine was paired %d times", pairs)
	}
	if view.Result.ConsoleURL != spark.server.URL || view.Result.OwnerPairingURL != spark.server.URL {
		t.Errorf("the owner was sent to an address the target chose: %+v", view.Result)
	}
	peers, err := fixture.store.Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].BaseURL != spark.server.URL {
		t.Fatalf("stored peer = %+v, want the adopted address", peers)
	}
	_, storedKey, err := fixture.store.PeerCredentials(context.Background(), peers[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedKey != spark.secret {
		t.Errorf("the stored key came from the wrong machine: %q", storedKey)
	}
}

// A hostile machine controls everything it says about itself, including its
// hostname, and the hostname becomes the peer's name: stored, listed and
// echoed back in the success result. The scrubbing that guards the failure
// path guards the happy path too.
func TestFleetAdoptScrubsAHostileSuccessResult(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	const password = "correct-horse-battery-staple"
	spark := newFakeSpark(t, "pairing-token-value")
	spark.hostname = password

	consoleBaseURL = func(address string) string {
		if address == "192.168.99.137" {
			return spark.server.URL
		}
		return "http://" + net.JoinHostPort(address, consolePort)
	}
	adoptDial = func(context.Context, string, string, string) (setup.Runner, func(), error) {
		return stubRunner{}, func() {}, nil
	}
	// The machine reports the password as its hostname, with control
	// characters and far more of them than a name can hold.
	adoptProbe = func(context.Context, setup.Runner) setup.Identity {
		return setup.Identity{
			Hostname:    "\x1b[2J" + password + strings.Repeat("-padding", 40),
			GPUName:     "NVIDIA GB10",
			DeviceModel: "NVIDIA DGX Spark",
		}
	}
	adoptBinarySource = func() (setup.BinarySource, error) {
		return setup.LocalFileSource{Path: "/usr/lib/basement/basement"}, nil
	}
	adoptInstall = func(context.Context, setup.Runner, setup.BinarySource, setup.Options, func(string, ...any)) (setup.InstallResult, error) {
		return setup.InstallResult{ConsoleURL: spark.server.URL, Token: "pairing-token-value"}, nil
	}

	if code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
		`{"address":"192.168.99.137","username":"nvidia","password":"`+password+`"}`); code != http.StatusAccepted {
		t.Fatalf("adopt code = %d, body = %s", code, payload)
	}
	view := fixture.waitAdoption(t)
	if view.State != adoptionSucceeded {
		t.Fatalf("state = %s, error = %q", view.State, view.Error)
	}
	// The whole status payload, byte for byte, result included.
	_, raw := fixture.call(t, http.MethodGet, "/api/v1/fleet/adopt/status", "")
	if strings.Contains(string(raw), password) {
		t.Fatalf("the status payload gave the SSH password back: %s", raw)
	}
	encoded, err := json.Marshal(view.Result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), password) {
		t.Fatalf("the success result gave the SSH password back: %s", encoded)
	}

	peers, err := fixture.store.Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 {
		t.Fatalf("peers = %+v", peers)
	}
	stored, _ := json.Marshal(peers)
	if strings.Contains(string(stored), password) {
		t.Fatalf("the peers table holds the SSH password: %s", stored)
	}
	name := peers[0].Name
	if len(name) > maxMachineNameLength {
		t.Errorf("the stored name is %d characters: %q", len(name), name)
	}
	if strings.ContainsAny(name, "\x1b\x00\n\r") {
		t.Errorf("the stored name kept its control characters: %q", name)
	}
	// And what the console lists is the same sanitized name.
	_, listed := fixture.call(t, http.MethodGet, "/api/v1/peers", "")
	if strings.Contains(string(listed), password) {
		t.Fatalf("the peers endpoint gave the SSH password back: %s", listed)
	}
}

// A sweep must cost the same whether the network holds three machines or a
// flood of mDNS announcements from one hostile host: candidates are capped
// and probes go through a fixed pool.
func TestFleetDiscoverCapsCandidatesAndProbeConcurrency(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	var inFlight, peak atomic.Int64
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inFlight.Add(1)
		for {
			highest := peak.Load()
			if current <= highest || peak.CompareAndSwap(highest, current) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer probe.Close()

	discoverCandidates = func(context.Context, func(string, ...any)) ([]discovery.Candidate, error) {
		flood := make([]discovery.Candidate, 0, 500)
		for index := 0; index < 500; index++ {
			flood = append(flood, discovery.Candidate{
				IP:       net.IPv4(10, byte(index/256), byte(index%256), 9),
				Hostname: "spark-flood.local",
			})
		}
		return flood, nil
	}
	selfAddresses = func() map[string]bool { return map[string]bool{} }
	consoleBaseURL = func(string) string { return probe.URL }

	code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/discover", "{}")
	if code != http.StatusOK {
		t.Fatalf("discover code = %d, body = %s", code, payload)
	}
	var result struct {
		Candidates []discoveredCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != maxDiscoveredCandidates {
		t.Errorf("a flood of %d announcements produced %d candidates, want the cap of %d",
			500, len(result.Candidates), maxDiscoveredCandidates)
	}
	if got := peak.Load(); got > maxProbeWorkers {
		t.Errorf("%d fingerprints ran at once, want at most %d", got, maxProbeWorkers)
	}
}

// Self-exclusion by spelling is not exclusion: a manager that installs over
// itself restarts the service running the adoption halfway through.
func TestFleetAdoptRefusesItselfHoweverItIsSpelled(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	selfAddresses = func() map[string]bool { return map[string]bool{"192.168.99.134": true, "spark-head": true} }
	localIPs = func() []net.IP { return []net.IP{net.ParseIP("192.168.99.134")} }
	resolveHost = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "localhost.":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		case "head.example.internal":
			// A name in the owner's own DNS that points back at this machine.
			return []net.IP{net.ParseIP("192.168.99.134")}, nil
		}
		if ip := net.ParseIP(host); ip != nil {
			return []net.IP{ip}, nil
		}
		return nil, errors.New("no such host")
	}
	adoptDial = func(context.Context, string, string, string) (setup.Runner, func(), error) {
		t.Error("adoption dialled this machine itself")
		return nil, func() {}, errors.New("unreachable")
	}
	for _, address := range []string{"127.0.0.2", "localhost.", "head.example.internal", "192.168.99.134"} {
		code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
			`{"address":"`+address+`","username":"nvidia","password":"typed-by-the-owner"}`)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, body = %s", address, code, payload)
		}
		if !strings.Contains(string(payload), "this Spark itself") {
			t.Errorf("%s: refusal did not explain itself: %s", address, payload)
		}
		if view := fixture.status(t); view.State != adoptionIdle {
			t.Errorf("%s: a refused adoption started anyway: %+v", address, view)
		}
	}
}

// Adoption is for Sparks on the owner's own network. Without that rule a
// console session is an SSH prober aimed at anywhere on the internet.
func TestFleetAdoptRefusesAddressesOffYourOwnNetwork(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	resolveHost = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "customer.example.com" {
			return []net.IP{net.ParseIP("198.51.100.7")}, nil
		}
		if ip := net.ParseIP(host); ip != nil {
			return []net.IP{ip}, nil
		}
		return nil, errors.New("no such host")
	}
	adoptDial = func(context.Context, string, string, string) (setup.Runner, func(), error) {
		t.Error("adoption dialled a machine on the public internet")
		return nil, func() {}, errors.New("unreachable")
	}
	for _, address := range []string{"203.0.113.9", "customer.example.com"} {
		code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
			`{"address":"`+address+`","username":"nvidia","password":"typed-by-the-owner"}`)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, body = %s", address, code, payload)
		}
		if !strings.Contains(string(payload), "your own network") {
			t.Errorf("%s: refusal did not explain itself: %s", address, payload)
		}
	}
	// The machines this product is about are still adoptable, and the address
	// the run is pinned to is the one that was checked.
	for _, address := range []string{"192.168.99.137", "100.64.0.14", "10.0.0.5"} {
		pinned, err := checkAdoptionTarget(context.Background(), address)
		if err != nil {
			t.Errorf("%s was refused: %v", address, err)
			continue
		}
		if pinned.String() != address {
			t.Errorf("%s pinned %s", address, pinned)
		}
	}
}

// A run that fails after minting the fleet key would otherwise leave a
// credential on the other machine that the owner never saw and cannot find.
func TestFleetAdoptRevokesTheFleetKeyWhenTheRunFailsAfterMintingIt(t *testing.T) {
	holdSeams(t)
	fixture := newFleetFixture(t)
	spark := newFakeSpark(t, "pairing-token-value")
	// The failure injected after the key exists: the reachability handshake
	// that records the peer is refused.
	spark.refuseHandshake = true

	consoleBaseURL = func(address string) string {
		if address == "192.168.99.137" {
			return spark.server.URL
		}
		return "http://" + net.JoinHostPort(address, consolePort)
	}
	adoptDial = func(context.Context, string, string, string) (setup.Runner, func(), error) {
		return stubRunner{}, func() {}, nil
	}
	adoptProbe = func(context.Context, setup.Runner) setup.Identity { return gb10Machine() }
	adoptBinarySource = func() (setup.BinarySource, error) {
		return setup.LocalFileSource{Path: "/usr/lib/basement/basement"}, nil
	}
	adoptInstall = func(context.Context, setup.Runner, setup.BinarySource, setup.Options, func(string, ...any)) (setup.InstallResult, error) {
		return setup.InstallResult{ConsoleURL: spark.server.URL, Token: "pairing-token-value"}, nil
	}

	if code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
		`{"address":"192.168.99.137","username":"nvidia","password":"typed-by-the-owner"}`); code != http.StatusAccepted {
		t.Fatalf("adopt code = %d, body = %s", code, payload)
	}
	view := fixture.waitAdoption(t)
	if view.State != adoptionFailed {
		t.Fatalf("state = %s", view.State)
	}
	revoked := spark.revocations()
	if len(revoked) != 1 || revoked[0] != "key_1" {
		t.Fatalf("the fleet key was left behind: revocations = %v", revoked)
	}
	if !strings.Contains(view.Error, "has been removed") {
		t.Errorf("the failure did not say what became of the key: %q", view.Error)
	}
	peers, err := fixture.store.Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Errorf("a failed adoption left a peer row behind: %+v", peers)
	}
}

// And when the key cannot be handed back, the owner is told what to look for
// rather than left with a credential nobody knows about.
func TestFleetAdoptSaysSoWhenTheFleetKeyCannotBeRevoked(t *testing.T) {
	// No key id and no session: nothing to revoke with, and nothing to prove
	// anything with either.
	key := fleetKey{secret: "rosk_x", name: "fleet-spark-head-4b7d1e02"}
	message := (&Server{}).revokeFleetKey(context.Background(), "http://192.168.99.137:7070", key)
	if !strings.Contains(message, key.name) || !strings.Contains(message, "Connect") {
		t.Errorf("the honest message does not say what to look for and where: %q", message)
	}
}

// pinFleetKeyName fixes the random half of the name this run's fleet key will
// carry, so a test can know it in advance, and returns that name.
func pinFleetKeyName(t *testing.T, suffix string) string {
	t.Helper()
	before := fleetKeySuffix
	t.Cleanup(func() { fleetKeySuffix = before })
	fleetKeySuffix = func() string { return suffix }
	return newFleetKeyName()
}

// Cleanup deletes the key this run created and nothing else. The other machine
// allows duplicate key names and lists them oldest first, so a cleanup that
// went by name alone would, whenever the owner already had a key named after
// this head, delete that older key, report success, and leave the key this run
// just minted behind: the opposite of the intent. The name a run mints under is
// its own, and the ids that were already there are snapshotted before the mint;
// a key is deleted only when both say it is ours.
func TestFleetAdoptRevokesOnlyTheKeyItCreated(t *testing.T) {
	// The name every run below mints under, known here so the machines can be
	// seeded with a key that collides with it.
	keyName := pinFleetKeyName(t, "4b7d1e02")
	cases := []struct {
		name string
		// setUp seeds the other machine and chooses how the mint fails. It is
		// given the name this run's key will carry.
		setUp func(spark *fakeSpark, keyName string)
		// deleted is what the run may remove, and kept is what must survive.
		deleted []string
		kept    []string
		// says is what the failure sentence has to tell the owner.
		says []string
	}{
		{
			// The reported bypass: a key already carrying the name this run
			// would use, and a mint whose answer is lost. The snapshot is what
			// says the old key is not this run's to delete.
			name: "a key of the same name was already there",
			setUp: func(spark *fakeSpark, keyName string) {
				spark.seed("key_pre", keyName)
				spark.mintFailure = "transport"
			},
			deleted: []string{"key_2"},
			kept:    []string{"key_pre=" + keyName},
			says:    []string{"has been removed"},
		},
		{
			// The same collision with no snapshot to compare against, because
			// the machine would not list its keys before the mint. The name is
			// then the only proof there is, and it is enough: a key from an
			// earlier adoption by this same head carries the old plain name.
			name: "with no snapshot to compare against",
			setUp: func(spark *fakeSpark, _ string) {
				spark.seed("key_pre", fleetKeyBaseName())
				spark.refuseListsBefore = 1
				spark.mintFailure = "transport"
			},
			deleted: []string{"key_2"},
			kept:    []string{"key_pre=" + fleetKeyBaseName()},
			says:    []string{"has been removed"},
		},
		{
			// The mint never reached the machine, so there is nothing of this
			// run's to delete and the colliding key is not a consolation prize.
			name: "the mint never reached the machine",
			setUp: func(spark *fakeSpark, _ string) {
				spark.seed("key_pre", fleetKeyBaseName())
				spark.mintFailure = "unreachable"
			},
			deleted: nil,
			kept:    []string{"key_pre=" + fleetKeyBaseName()},
			says:    []string{"No fleet key was left"},
		},
		{
			// Two keys of this run's own name, so which one it created is not
			// knowable. Deleting either is a guess, so it deletes neither and
			// says what to look for and where.
			name: "authorship cannot be told apart",
			setUp: func(spark *fakeSpark, _ string) {
				spark.mintDecoy = true
				spark.mintFailure = "transport"
			},
			deleted: nil,
			kept:    []string{"key_1=" + keyName, "key_2=" + keyName},
			says:    []string{keyName, "Connect", "could not tell"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			holdSeams(t)
			fixture := newFleetFixture(t)
			spark := newFakeSpark(t, "pairing-token-value")
			test.setUp(spark, keyName)
			adoptionSeams(spark)

			if code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
				`{"address":"192.168.99.137","username":"nvidia","password":"typed-by-the-owner"}`); code != http.StatusAccepted {
				t.Fatalf("adopt code = %d, body = %s", code, payload)
			}
			view := fixture.waitAdoption(t)
			if view.State != adoptionFailed {
				t.Fatalf("state = %s", view.State)
			}
			if got := spark.deleteAttempts(); !slices.Equal(got, test.deleted) {
				t.Errorf("delete attempts = %v, want %v", got, test.deleted)
			}
			for _, wanted := range test.kept {
				if !slices.Contains(spark.keyNames(), wanted) {
					t.Errorf("key %q is gone: the machine holds %v", wanted, spark.keyNames())
				}
			}
			for _, sentence := range test.says {
				if !strings.Contains(view.Error, sentence) {
					t.Errorf("the failure does not say %q: %q", sentence, view.Error)
				}
			}
		})
	}
}

// adoptionSeams stands up the fake machine every adoption test needs: an SSH
// session that answers as a GB10, an install that reports the fake console,
// and a console reachable at 192.168.99.137. Tests override what they are
// about afterwards.
func adoptionSeams(spark *fakeSpark) {
	consoleBaseURL = func(address string) string {
		if address == "192.168.99.137" {
			return spark.server.URL
		}
		return "http://" + net.JoinHostPort(address, consolePort)
	}
	adoptDial = func(context.Context, string, string, string) (setup.Runner, func(), error) {
		return stubRunner{}, func() {}, nil
	}
	adoptProbe = func(context.Context, setup.Runner) setup.Identity { return gb10Machine() }
	adoptBinarySource = func() (setup.BinarySource, error) {
		return setup.LocalFileSource{Path: "/usr/lib/basement/basement"}, nil
	}
	adoptInstall = func(context.Context, setup.Runner, setup.BinarySource, setup.Options, func(string, ...any)) (setup.InstallResult, error) {
		return setup.InstallResult{ConsoleURL: spark.server.URL, Token: "pairing-token-value"}, nil
	}
}

// leakedFragment reports the longest run of secret, of minLength characters or
// more, that survived into text, ignoring case. A whole password is the obvious
// leak; a fragment of one is still a fragment of the owner's password, and it
// is what a length cap applied to unscrubbed text leaves behind.
func leakedFragment(text, secret string, minLength int) string {
	folded := strings.ToLower(text)
	for length := len(secret); length >= minLength; length-- {
		for start := 0; start+length <= len(secret); start++ {
			if piece := secret[start : start+length]; strings.Contains(folded, strings.ToLower(piece)) {
				return piece
			}
		}
	}
	return ""
}

// A hostile machine chooses the bytes of the name it reports, and this manager
// both transforms that name (control characters out, length capped) and scrubs
// the typed password out of it. Whichever order those two run in has to leave
// the password gone: a transformation that runs after the scrub can put a
// secret back together out of pieces the scrub did not recognise, and a cap
// that runs before it can leave a fragment behind. The peers table is the
// thing that matters, because the run's own result is scrubbed again on the
// way out and the stored row is not.
func TestFleetAdoptCannotBeTrickedIntoStoringThePassword(t *testing.T) {
	const password = "correct-horse-battery-staple"
	cases := []struct {
		name string
		// typed is what the owner typed into the console, and hostname is what
		// the machine on the other end answers `hostname` with. An empty typed
		// means the ordinary password above.
		typed    string
		hostname string
	}{
		{
			// The password split by a control character. A scrub that runs
			// first does not match it, and the strip that runs after joins it.
			name:     "split by a control character",
			hostname: "correct-horse\x1b-battery-staple",
		},
		{
			// The same trick with an invalid UTF-8 byte, which the strip
			// deletes for a different reason.
			name:     "split by an invalid byte",
			hostname: "correct-horse\xff-battery-staple",
		},
		{
			// Padded so the password straddles the length cap, with twelve of
			// its characters left inside it. A cap that runs before the scrub
			// cuts the password into a fragment no later scrub can recognise,
			// and stores that.
			name:     "padded past the length cap",
			hostname: strings.Repeat("a", maxMachineNameLength-12) + password,
		},
		{
			// The trick the other way round, and the stronger form of it: the
			// password was typed with whitespace around it, and the machine
			// reports what this manager's own trimming would have produced. No
			// scrub that compares against the typed bytes ever sees a match, at
			// any point in the order, so the ordering rule cannot save this one
			// and the scrubber has to know the trimmed form itself.
			name:     "typed with whitespace, reported without it",
			typed:    "  " + password + "\t",
			hostname: password,
		},
		{
			// The same password reported whole, which is what the ordering rule
			// catches: the first scrub sees it before the trimming does.
			name:     "typed with whitespace, reported whole",
			typed:    "  " + password + "\t",
			hostname: "  " + password + "\t",
		},
		{
			// A password holding a character the strip removes, reported as
			// what the strip would leave of it.
			name:     "typed with a control character, reported without it",
			typed:    "correct-horse\x07-battery-staple",
			hostname: "correct-horse-battery-staple",
		},
		{
			// Case is the cheapest rewrite of all, and hostnames are lowercased
			// and compared all over this file.
			name:     "differing only in case",
			hostname: strings.ToUpper(password),
		},
		{
			// A short password is as much the owner's password as a long one,
			// and nothing here may have a minimum length below which it stops
			// being scrubbed.
			name:     "short enough to look like noise",
			typed:    "s3cr3",
			hostname: " s3cr3 ",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			holdSeams(t)
			fixture := newFleetFixture(t)
			spark := newFakeSpark(t, "pairing-token-value")
			adoptionSeams(spark)
			adoptProbe = func(context.Context, setup.Runner) setup.Identity {
				return setup.Identity{
					Hostname:    test.hostname,
					GPUName:     "NVIDIA GB10",
					DeviceModel: "NVIDIA DGX Spark",
				}
			}
			typed := test.typed
			if typed == "" {
				typed = password
			}
			// Marshalled rather than pasted into a literal: some of these
			// passwords hold bytes a JSON string cannot carry raw, and the
			// request has to be the one the owner's browser would send.
			body, err := json.Marshal(map[string]string{
				"address": "192.168.99.137", "username": "nvidia", "password": typed,
			})
			if err != nil {
				t.Fatal(err)
			}

			if code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt", string(body)); code != http.StatusAccepted {
				t.Fatalf("adopt code = %d, body = %s", code, payload)
			}
			view := fixture.waitAdoption(t)
			if view.State != adoptionSucceeded {
				t.Fatalf("state = %s, error = %q", view.State, view.Error)
			}

			peers, err := fixture.store.Peers(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(peers) != 1 {
				t.Fatalf("peers = %+v", peers)
			}
			stored, err := json.Marshal(peers)
			if err != nil {
				t.Fatal(err)
			}
			result, err := json.Marshal(view.Result)
			if err != nil {
				t.Fatal(err)
			}
			_, listed := fixture.call(t, http.MethodGet, "/api/v1/peers", "")
			_, raw := fixture.call(t, http.MethodGet, "/api/v1/fleet/adopt/status", "")
			for _, place := range []struct{ what, text string }{
				{"the peers table", string(stored)},
				{"the peers endpoint", string(listed)},
				{"the status payload", string(raw)},
				{"the adoption result", string(result)},
				// The stored name as the store holds it, not as JSON spells
				// it: a password carrying a control character comes back from
				// json.Marshal escaped, and an escaped secret is still the
				// secret.
				{"the stored peer name", peers[0].Name},
			} {
				// Every shape of the password, not just the bytes the owner
				// typed. Whitespace and control characters are exactly what
				// this manager's own transformations remove, so the trimmed and
				// stripped forms are the ones a bypass would leave behind.
				for _, form := range passwordForms(typed) {
					if strings.Contains(strings.ToLower(place.text), strings.ToLower(form)) {
						t.Fatalf("%s holds %q, the SSH password: %s", place.what, form, place.text)
					}
					if piece := leakedFragment(place.text, form, min(8, len(form))); piece != "" {
						t.Fatalf("%s holds %q, a piece of the SSH password: %s", place.what, piece, place.text)
					}
				}
			}
			if name := peers[0].Name; len(name) > maxMachineNameLength || strings.ContainsAny(name, "\x1b\x00\n\r") {
				t.Errorf("the stored name is not the shape the store accepts: %q", name)
			}
		})
	}
}

// The address is checked once and used many times: an SSH login carrying the
// owner's password, an install, a console wait, a pairing, a fleet key and a
// stored peer row. If every one of those looked the name up again, a name with
// answers the attacker controls would be private for the check and whatever it
// liked afterwards. The run is pinned to the address that passed.
func TestFleetAdoptPinsTheAddressItValidated(t *testing.T) {
	const rebind = "rebind.example.internal"
	cases := []struct {
		name  string
		later net.IP
	}{
		{name: "loopback afterwards", later: net.ParseIP("127.0.0.1")},
		{name: "public afterwards", later: net.ParseIP("198.51.100.7")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			holdSeams(t)
			fixture := newFleetFixture(t)
			spark := newFakeSpark(t, "pairing-token-value")
			accomplice := newFakeSpark(t, "pairing-token-value")
			accomplice.secret = "rosk_accomplicekeyaccomplicekey"
			adoptionSeams(spark)

			var lookups atomic.Int64
			resolveHost = func(_ context.Context, host string) ([]net.IP, error) {
				if host != rebind {
					if ip := net.ParseIP(host); ip != nil {
						return []net.IP{ip}, nil
					}
					return nil, errors.New("no such host")
				}
				if lookups.Add(1) == 1 {
					return []net.IP{net.ParseIP("192.168.99.137")}, nil
				}
				return []net.IP{test.later}, nil
			}
			// Anything that resolves the name a second time, or that keeps the
			// name in a URL, reaches the accomplice instead of the Spark.
			consoleBaseURL = func(address string) string {
				switch address {
				case "192.168.99.137":
					return spark.server.URL
				case rebind, test.later.String():
					return accomplice.server.URL
				}
				return "http://" + net.JoinHostPort(address, consolePort)
			}
			var dialled atomic.Value
			adoptDial = func(_ context.Context, address, _, _ string) (setup.Runner, func(), error) {
				dialled.Store(address)
				return stubRunner{}, func() {}, nil
			}

			if code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
				`{"address":"`+rebind+`","username":"nvidia","password":"typed-by-the-owner"}`); code != http.StatusAccepted {
				t.Fatalf("adopt code = %d, body = %s", code, payload)
			}
			view := fixture.waitAdoption(t)
			if view.State != adoptionSucceeded {
				t.Fatalf("state = %s, error = %q", view.State, view.Error)
			}
			if got, _ := dialled.Load().(string); got != "192.168.99.137" {
				t.Errorf("the SSH password was carried to %q, not to the address that was validated", got)
			}
			if accomplice.called() != 0 {
				t.Errorf("the rebound address was contacted %d times", accomplice.called())
			}
			if got := lookups.Load(); got != 1 {
				t.Errorf("the name was resolved %d times, want exactly the one validation lookup", got)
			}
			if view.Result.ConsoleURL != spark.server.URL || view.Result.OwnerPairingURL != spark.server.URL {
				t.Errorf("the owner was sent to the rebound address: %+v", view.Result)
			}
			peers, err := fixture.store.Peers(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(peers) != 1 || peers[0].BaseURL != spark.server.URL {
				t.Fatalf("stored peer = %+v, want the pinned address", peers)
			}
			_, storedKey, err := fixture.store.PeerCredentials(context.Background(), peers[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if storedKey != spark.secret {
				t.Errorf("the stored key came from the rebound address: %q", storedKey)
			}
		})
	}
}

// The other machine commits the fleet key and then answers. A dropped
// connection or an answer nothing can parse therefore says nothing about
// whether the key exists, so sending the request is the point of no return:
// the key is chased down by the name this run used and handed back, and when
// even that fails the owner is told exactly what to delete and where.
func TestFleetAdoptHandsBackAKeyItNeverSawTheIdOf(t *testing.T) {
	cases := []struct {
		name         string
		failure      string
		refuseDelete bool
	}{
		{name: "the connection drops", failure: "transport"},
		{name: "the answer is unreadable", failure: "garbage"},
		{name: "and the delete fails too", failure: "transport", refuseDelete: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			holdSeams(t)
			fixture := newFleetFixture(t)
			keyName := pinFleetKeyName(t, "4b7d1e02")
			spark := newFakeSpark(t, "pairing-token-value")
			spark.mintFailure = test.failure
			spark.refuseDelete = test.refuseDelete
			adoptionSeams(spark)

			if code, payload := fixture.call(t, http.MethodPost, "/api/v1/fleet/adopt",
				`{"address":"192.168.99.137","username":"nvidia","password":"typed-by-the-owner"}`); code != http.StatusAccepted {
				t.Fatalf("adopt code = %d, body = %s", code, payload)
			}
			view := fixture.waitAdoption(t)
			if view.State != adoptionFailed {
				t.Fatalf("state = %s", view.State)
			}
			attempted := spark.deleteAttempts()
			if len(attempted) != 1 || attempted[0] != "key_1" {
				t.Fatalf("the minted key was not handed back: delete attempts = %v", attempted)
			}
			if test.refuseDelete {
				if !strings.Contains(view.Error, keyName) || !strings.Contains(view.Error, "could not be removed") {
					t.Errorf("the failure did not name the key the owner has to delete: %q", view.Error)
				}
				if !strings.Contains(view.Error, "Connect") {
					t.Errorf("the failure did not say where to delete it: %q", view.Error)
				}
				return
			}
			if !strings.Contains(view.Error, "has been removed") {
				t.Errorf("the failure did not say what became of the key: %q", view.Error)
			}
			if revoked := spark.revocations(); len(revoked) != 1 || revoked[0] != "key_1" {
				t.Errorf("revocations = %v", revoked)
			}
		})
	}
}
