package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
	t.Cleanup(func() {
		discoverCandidates, adoptProbe, adoptInstall = discoverBefore, probeBefore, installBefore
		adoptDial, adoptBinarySource = dialBefore, sourceBefore
		consoleBaseURL, selfAddresses = urlBefore, selfBefore
		resolveHost, localIPs = resolveBefore, localBefore
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
	// refuseHandshake makes the reachability check at the end of adoption
	// fail, which is how a test injects a failure after the fleet key exists.
	refuseHandshake bool
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
		if err != nil || cookie.Value != "session-value" || r.Header.Get("X-CSRF-Token") != "csrf-value" {
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
		writeJSON(w, http.StatusCreated, map[string]any{"key": map[string]any{"id": "key_1", "name": request.Name}, "secret": spark.secret})
	})
	// Key revocation, the same shape this manager serves it in.
	mux.HandleFunc("/api/v1/keys/", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("basement_session")
		if r.Method != http.MethodDelete || err != nil || cookie.Value != "session-value" || r.Header.Get("X-CSRF-Token") != "csrf-value" {
			writeError(w, http.StatusForbidden, errors.New("valid CSRF token required"))
			return
		}
		spark.mu.Lock()
		spark.revoked = append(spark.revoked, strings.TrimPrefix(r.URL.Path, "/api/v1/keys/"))
		spark.mu.Unlock()
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
	// The machines this product is about are still adoptable.
	for _, address := range []string{"192.168.99.137", "100.64.0.14", "10.0.0.5"} {
		if err := checkAdoptionTarget(context.Background(), address); err != nil {
			t.Errorf("%s was refused: %v", address, err)
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

// And when the key cannot be handed back, the owner is told what to delete
// rather than left with a credential nobody knows about.
func TestFleetAdoptSaysSoWhenTheFleetKeyCannotBeRevoked(t *testing.T) {
	// No key id and no session: nothing to revoke with.
	message := (&Server{}).revokeFleetKey(context.Background(), "http://192.168.99.137:7070", fleetKey{secret: "rosk_x"})
	if !strings.Contains(message, "could not be removed") || !strings.Contains(message, fleetKeyName()) {
		t.Errorf("the honest message does not name the key to delete: %q", message)
	}
}
