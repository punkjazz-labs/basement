package setupweb

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/discovery"
	"github.com/punkjazz-labs/basement/internal/setup"
)

// newTestServer starts a real loopback server (no flow goroutine — tests
// drive WizardUI methods and answers directly) and returns it plus a buffer
// capturing everything the request logger writes.
func newTestServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	s, err := New(log.New(&logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s.flowCtx = context.Background()
	go s.server.Serve(s.listener)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.server.Shutdown(ctx)
	})
	return s, &logBuf
}

func waitForPending(t *testing.T, s *Server, kind string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		p := s.pending
		s.mu.Unlock()
		if p != nil && p.kind == kind {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a pending %q question", kind)
}

func postAnswer(t *testing.T, s *Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post("http://"+s.addr+"/setup/"+s.token+"/api/answer", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// --- Security: the acceptance-listed checks, line by line ---

func TestPageRequestWithoutTokenIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	// No token segment at all: the route pattern itself does not match.
	resp, err := http.Get("http://" + s.addr + "/setup/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPageRequestWithWrongTokenIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	for _, path := range []string{
		"/setup/not-the-token",
		"/setup/" + s.token + "x",
		"/setup/" + s.token[:len(s.token)-1],
	} {
		resp, err := http.Get("http://" + s.addr + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestValidTokenServesThePage(t *testing.T) {
	s, _ := newTestServer(t)
	resp, err := http.Get("http://" + s.addr + "/setup/" + s.token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "basement setup") {
		t.Error("page body does not look like the wizard page")
	}
}

func TestStateAndAnswerAlsoRequireTheToken(t *testing.T) {
	s, _ := newTestServer(t)
	resp, err := http.Get("http://" + s.addr + "/setup/wrong/api/state")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("state with wrong token: status = %d, want 404", resp.StatusCode)
	}

	resp2, err := http.Post("http://"+s.addr+"/setup/wrong/api/answer", "application/json", strings.NewReader(`{"kind":"confirm","proceed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("answer with wrong token: status = %d, want 404", resp2.StatusCode)
	}
}

func TestWrongHostHeaderIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	req, err := http.NewRequest(http.MethodGet, "http://"+s.addr+"/setup/"+s.token, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A DNS name that resolves to 127.0.0.1 but is not the literal address
	// this server bound — exactly the DNS-rebinding shape the Host check
	// exists to catch.
	req.Host = "attacker-controlled.example:80"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestForeignOriginIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	req, err := http.NewRequest(http.MethodGet, "http://"+s.addr+"/setup/"+s.token+"/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestMatchingOriginIsAccepted(t *testing.T) {
	s, _ := newTestServer(t)
	req, err := http.NewRequest(http.MethodGet, "http://"+s.addr+"/setup/"+s.token+"/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://"+s.addr)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (same-origin fetch must work)", resp.StatusCode)
	}
}

func TestNoOriginHeaderIsAccepted(t *testing.T) {
	// Direct browser navigation sends no Origin header at all.
	s, _ := newTestServer(t)
	resp, err := http.Get("http://" + s.addr + "/setup/" + s.token)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAnswerEndpointRejectsGET(t *testing.T) {
	s, _ := newTestServer(t)
	resp, err := http.Get("http://" + s.addr + "/setup/" + s.token + "/api/answer?value=secret")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/answer: status = %d, want 405", resp.StatusCode)
	}
}

func TestAnswerWithoutAPendingQuestionIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	resp := postAnswer(t, s, `{"kind":"password","value":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestAnswerWithMismatchedKindIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	go func() { _, _ = s.Confirm("trust this host?") }()
	waitForPending(t, s, "confirm")

	resp := postAnswer(t, s, `{"kind":"password","value":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// TestPasswordNeverLeavesThePOSTBodyOrTheLogs is the acceptance test for
// the hardest security rule: the SSH/sudo password travels only in a POST
// body over loopback, is never echoed back, and never appears in the
// server's own request log (which records method + token-redacted path
// only — see (*Server).logged).
func TestPasswordNeverLeavesThePOSTBodyOrTheLogs(t *testing.T) {
	s, logBuf := newTestServer(t)
	const secret = "correct horse battery staple 🔒"

	resultCh := make(chan string, 1)
	go func() {
		value, _ := s.Password("alice@spark-head password: ")
		resultCh <- value
	}()
	waitForPending(t, s, "password")

	payload, err := json.Marshal(map[string]string{"kind": "password", "value": secret})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post("http://"+s.addr+"/setup/"+s.token+"/api/answer", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("answer status = %d body=%s", resp.StatusCode, responseBody)
	}
	if strings.Contains(string(responseBody), secret) {
		t.Error("the password was echoed back in the HTTP response")
	}

	select {
	case got := <-resultCh:
		if got != secret {
			t.Errorf("Password() returned %q, want %q", got, secret)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Password() never returned")
	}

	// Poll /api/state once too: the prompt text may legitimately appear
	// there (it is not the secret), but the secret itself never should.
	stateResp, err := http.Get("http://" + s.addr + "/setup/" + s.token + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	stateBody, _ := io.ReadAll(stateResp.Body)
	stateResp.Body.Close()
	if strings.Contains(string(stateBody), secret) {
		t.Error("the password leaked into the polled state")
	}

	logged := logBuf.String()
	if strings.Contains(logged, secret) {
		t.Errorf("the password leaked into the request log: %q", logged)
	}
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if !strings.HasPrefix(line, "GET ") && !strings.HasPrefix(line, "POST ") {
			continue
		}
		if strings.Contains(line, "?") {
			t.Errorf("a logged path carried a query string: %q", line)
		}
	}
}

// --- WizardUI implementation: each method blocks for its matching answer ---

func TestChooseMachineReturnsThePostedIndex(t *testing.T) {
	s, _ := newTestServer(t)
	candidates := []discovery.Candidate{
		{Hostname: "spark-head.local", IP: net.ParseIP("192.168.99.134")},
		{Hostname: "some-nas.local", IP: net.ParseIP("192.168.99.140")},
	}
	resultCh := make(chan int, 1)
	go func() {
		index, err := s.ChooseMachine(candidates)
		if err != nil {
			t.Error(err)
		}
		resultCh <- index
	}()
	waitForPending(t, s, "machines")

	resp := postAnswer(t, s, `{"kind":"machines","index":1}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := <-resultCh; got != 1 {
		t.Errorf("ChooseMachine = %d, want 1", got)
	}
}

func TestChooseMachineRejectsOutOfRangeIndex(t *testing.T) {
	s, _ := newTestServer(t)
	candidates := []discovery.Candidate{{Hostname: "spark-head.local"}}
	errCh := make(chan error, 1)
	go func() {
		_, err := s.ChooseMachine(candidates)
		errCh <- err
	}()
	waitForPending(t, s, "machines")

	resp := postAnswer(t, s, `{"kind":"machines","index":5}`)
	resp.Body.Close()
	if err := <-errCh; err == nil {
		t.Error("expected an error for an out-of-range index")
	}
}

func TestConfirmNonGB10DeclineReturnsErrDeclined(t *testing.T) {
	s, _ := newTestServer(t)
	errCh := make(chan error, 1)
	go func() {
		_, err := s.ConfirmNonGB10("some-nas")
		errCh <- err
	}()
	waitForPending(t, s, "nongb10")

	resp := postAnswer(t, s, `{"kind":"nongb10","proceed":false}`)
	resp.Body.Close()
	if err := <-errCh; err != setup.ErrDeclined {
		t.Errorf("err = %v, want setup.ErrDeclined", err)
	}
}

func TestAskUsernameFallsBackToSuggestedOnEmptyAnswer(t *testing.T) {
	s, _ := newTestServer(t)
	resultCh := make(chan string, 1)
	go func() {
		username, _ := s.AskUsername("spark-head", "nvidia")
		resultCh <- username
	}()
	waitForPending(t, s, "username")

	resp := postAnswer(t, s, `{"kind":"username","username":""}`)
	resp.Body.Close()
	if got := <-resultCh; got != "nvidia" {
		t.Errorf("AskUsername = %q, want the suggested default %q", got, "nvidia")
	}
}

func TestChooseListenMapsModeString(t *testing.T) {
	s, _ := newTestServer(t)
	resultCh := make(chan setup.ListenMode, 1)
	go func() {
		mode, _ := s.ChooseListen(true)
		resultCh <- mode
	}()
	waitForPending(t, s, "listen")

	resp := postAnswer(t, s, `{"kind":"listen","mode":"lan"}`)
	resp.Body.Close()
	if got := <-resultCh; got != setup.ListenLAN {
		t.Errorf("ChooseListen = %q, want lan", got)
	}
}

func TestProgressAccumulatesAndSummaryEndsThePolledState(t *testing.T) {
	s, _ := newTestServer(t)
	s.Progress("uploading manager binary")
	s.Progress("starting service")
	s.Summary(setup.InstallResult{ConsoleURL: "http://192.168.99.134:7070", Token: "PAIR-000000"})

	resp, err := http.Get("http://" + s.addr + "/setup/" + s.token + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got stateView
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Phase != "summary" {
		t.Errorf("Phase = %q, want summary", got.Phase)
	}
	if len(got.Progress) != 2 {
		t.Errorf("Progress = %v, want 2 accumulated lines", got.Progress)
	}
	if got.Summary == nil || got.Summary.Token != "PAIR-000000" {
		t.Errorf("Summary = %+v", got.Summary)
	}
}

func TestRedactPathHidesTheToken(t *testing.T) {
	cases := map[string]string{
		"/setup/abc123":            "/setup/<token>",
		"/setup/abc123/api/state":  "/setup/<token>/api/state",
		"/setup/abc123/api/answer": "/setup/<token>/api/answer",
		"/unrelated/path":          "/unrelated/path",
		"/setup/":                  "/setup/<token>",
	}
	for input, want := range cases {
		if got := redactPath(input); got != want {
			t.Errorf("redactPath(%q) = %q, want %q", input, got, want)
		}
	}
}
