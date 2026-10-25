package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/recipefeed"
	"github.com/punkjazz-labs/basement/internal/store"
)

// feedHarness is newPairedTestServer with the server object kept, because
// feed health is reported by the server and the tests swap its feed hooks.
type feedHarness struct {
	server  *httptest.Server
	api     *Server
	cookies []*http.Cookie
	csrf    string
}

func newFeedHarness(t *testing.T) *feedHarness {
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

	tokenBytes, err := os.ReadFile(authManager.PairingTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	paired := doRequest(t, http.MethodPost, server.URL+"/api/v1/auth/pair",
		`{"token":"`+strings.TrimSpace(string(tokenBytes))+`"}`, nil, map[string]string{"Origin": server.URL})
	if paired.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(paired.Body)
		t.Fatalf("pair status=%d body=%s", paired.StatusCode, data)
	}
	var pairResult struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(paired.Body).Decode(&pairResult); err != nil {
		t.Fatal(err)
	}
	cookies := paired.Cookies()
	paired.Body.Close()
	return &feedHarness{server: server, api: api, cookies: cookies, csrf: pairResult.CSRF}
}

func (h *feedHarness) get(t *testing.T, path string, into any) {
	t.Helper()
	response := doRequest(t, http.MethodGet, h.server.URL+path, "", h.cookies, nil)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, response.StatusCode, data)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("GET %s returned %s: %v", path, data, err)
	}
}

// fakeFeed stands in for recipefeed.Fetcher on the endpoint's side of the
// seam: it counts the fetches the endpoint really started, can be held open so
// two calls overlap on purpose, and reports health that only moves when a
// fetch happens — which is what the real fetcher does.
type fakeFeed struct {
	mu      sync.Mutex
	calls   int
	fetched bool
	// release holds a fetch open until the test closes it; started reports
	// that a fetch has begun, so a test can make the second call arrive while
	// the first is still running rather than hoping it does.
	release chan struct{}
	started chan struct{}
}

func (f *fakeFeed) fetch(context.Context) recipefeed.Health {
	f.mu.Lock()
	f.calls++
	release, started := f.release, f.started
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	f.mu.Lock()
	f.fetched = true
	f.mu.Unlock()
	return f.health()
}

func (f *fakeFeed) health() recipefeed.Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.fetched {
		return recipefeed.Health{State: recipefeed.StateNeverFetched}
	}
	accepted := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	fetched := time.Now().UTC()
	return recipefeed.Health{State: recipefeed.StateOK, AcceptedGeneratedAt: &accepted, FetchedAt: &fetched}
}

func (f *fakeFeed) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// wireFakeFeed gives the harness a feed whose fetches the test controls.
func wireFakeFeed(h *feedHarness) *fakeFeed {
	feed := &fakeFeed{}
	h.api.SetRecipeFeedHealth(feed.health)
	h.api.SetRecipeFeedRefresh(feed.fetch)
	return feed
}

type refreshAnswer struct {
	status            int
	State             string `json:"state"`
	RefreshedRecently bool   `json:"refreshed_recently"`
}

// checkFeed posts the console's feed check. It never calls t.Fatal, so the
// concurrency test can run it from its own goroutines.
func checkFeed(h *feedHarness, cookies []*http.Cookie, csrf string) (refreshAnswer, error) {
	request, err := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/recipes/refresh", strings.NewReader("{}"))
	if err != nil {
		return refreshAnswer{}, err
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	request.Header.Set("Origin", h.server.URL)
	request.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return refreshAnswer{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return refreshAnswer{}, err
	}
	answer := refreshAnswer{status: response.StatusCode}
	if response.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &answer); err != nil {
			return refreshAnswer{}, err
		}
	}
	return answer, nil
}

func TestCheckingTheFeedFetchesNowAndAnswersWithItsHealth(t *testing.T) {
	h := newFeedHarness(t)
	feed := wireFakeFeed(h)

	answer, err := checkFeed(h, h.cookies, h.csrf)
	if err != nil {
		t.Fatal(err)
	}
	if answer.status != http.StatusOK {
		t.Fatalf("check status=%d, want 200", answer.status)
	}
	if feed.fetchCount() != 1 {
		t.Fatalf("the check started %d fetches, want 1", feed.fetchCount())
	}
	// The answer is the health that fetch produced, not the health from
	// before it: the console shows this without a second read.
	if answer.State != recipefeed.StateOK {
		t.Fatalf("state=%q after a check, want %q", answer.State, recipefeed.StateOK)
	}
	if answer.RefreshedRecently {
		t.Fatal("a check that really fetched claimed the feed had just been checked")
	}
}

func TestTwoChecksAtOnceShareOneFetch(t *testing.T) {
	h := newFeedHarness(t)
	feed := wireFakeFeed(h)
	feed.release = make(chan struct{})
	feed.started = make(chan struct{})

	type result struct {
		answer refreshAnswer
		err    error
	}
	first := make(chan result, 1)
	go func() {
		answer, err := checkFeed(h, h.cookies, h.csrf)
		first <- result{answer, err}
	}()
	// The second call is only sent once the first fetch is genuinely running,
	// so this tests sharing rather than luck.
	select {
	case <-feed.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first check never started a fetch")
	}
	second := make(chan result, 1)
	go func() {
		answer, err := checkFeed(h, h.cookies, h.csrf)
		second <- result{answer, err}
	}()
	// The joining call must be waiting on the running fetch, not fetching.
	time.Sleep(100 * time.Millisecond)
	if feed.fetchCount() != 1 {
		t.Fatalf("two concurrent checks started %d fetches, want 1", feed.fetchCount())
	}
	close(feed.release)

	for name, waiting := range map[string]chan result{"first": first, "second": second} {
		select {
		case got := <-waiting:
			if got.err != nil {
				t.Fatalf("%s check: %v", name, got.err)
			}
			if got.answer.status != http.StatusOK {
				t.Fatalf("%s check status=%d, want 200", name, got.answer.status)
			}
			if got.answer.State != recipefeed.StateOK {
				t.Fatalf("%s check state=%q, want %q", name, got.answer.State, recipefeed.StateOK)
			}
			// Both shared one real fetch, so neither was turned away.
			if got.answer.RefreshedRecently {
				t.Fatalf("%s check was told the feed had just been checked, but it shared the fetch", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the %s check never answered", name)
		}
	}
	if feed.fetchCount() != 1 {
		t.Fatalf("two concurrent checks started %d fetches, want 1", feed.fetchCount())
	}
}

func TestASecondCheckInsideTheWindowSaysSoInsteadOfFetching(t *testing.T) {
	h := newFeedHarness(t)
	feed := wireFakeFeed(h)

	if _, err := checkFeed(h, h.cookies, h.csrf); err != nil {
		t.Fatal(err)
	}
	answer, err := checkFeed(h, h.cookies, h.csrf)
	if err != nil {
		t.Fatal(err)
	}
	if answer.status != http.StatusOK {
		t.Fatalf("a check inside the window status=%d, want 200", answer.status)
	}
	if !answer.RefreshedRecently {
		t.Fatal("a check inside the window did not say the feed had just been checked")
	}
	// Turned away, not refused: the current health still comes back, so the
	// console has an answer either way.
	if answer.State != recipefeed.StateOK {
		t.Fatalf("state=%q inside the window, want %q", answer.State, recipefeed.StateOK)
	}
	if feed.fetchCount() != 1 {
		t.Fatalf("a check inside the window started another fetch: %d fetches", feed.fetchCount())
	}

	// Once the window has passed, the next check fetches again.
	h.api.feedRefreshMu.Lock()
	h.api.feedRefreshedAt = time.Now().Add(-forcedFeedRefreshWindow - time.Second)
	h.api.feedRefreshMu.Unlock()
	answer, err = checkFeed(h, h.cookies, h.csrf)
	if err != nil {
		t.Fatal(err)
	}
	if answer.RefreshedRecently || feed.fetchCount() != 2 {
		t.Fatalf("a check after the window did not fetch: recently=%v fetches=%d", answer.RefreshedRecently, feed.fetchCount())
	}
}

func TestAShuttingDownManagerStartsNoFetch(t *testing.T) {
	h := newFeedHarness(t)
	feed := wireFakeFeed(h)

	// Close is what the HTTP server runs at the start of a shutdown. It waits
	// for any fetch already running, and nothing may start a new one after it:
	// the fetch would be abandoned at exit or hold the shutdown open.
	h.api.Close()
	answer, err := checkFeed(h, h.cookies, h.csrf)
	if err != nil {
		t.Fatal(err)
	}
	if answer.status != http.StatusServiceUnavailable {
		t.Fatalf("a check during shutdown status=%d, want 503", answer.status)
	}
	if feed.fetchCount() != 0 {
		t.Fatalf("a check during shutdown started %d fetches, want 0", feed.fetchCount())
	}
}

func TestCheckingTheFeedNeedsAConsoleSession(t *testing.T) {
	h := newFeedHarness(t)
	feed := wireFakeFeed(h)

	answer, err := checkFeed(h, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if answer.status != http.StatusForbidden {
		t.Fatalf("an unauthenticated check status=%d, want 403", answer.status)
	}
	// A session cookie without the CSRF token is not a console either.
	answer, err = checkFeed(h, h.cookies, "")
	if err != nil {
		t.Fatal(err)
	}
	if answer.status != http.StatusForbidden {
		t.Fatalf("a check with no CSRF token status=%d, want 403", answer.status)
	}
	if feed.fetchCount() != 0 {
		t.Fatalf("a refused check still touched the feed: %d fetches", feed.fetchCount())
	}
}

func TestSystemReportsRecipeFeedHealth(t *testing.T) {
	h := newFeedHarness(t)
	type feedHealth struct {
		State               string     `json:"state"`
		AcceptedGeneratedAt *time.Time `json:"accepted_generated_at"`
		FetchedAt           *time.Time `json:"fetched_at"`
		Stale               bool       `json:"stale"`
	}
	var system struct {
		RecipeFeed feedHealth `json:"recipe_feed"`
	}

	// No feed wired is exactly the never_fetched case, reported rather than
	// omitted: a console that shows nothing cannot warn about anything.
	h.get(t, "/api/v1/system", &system)
	if system.RecipeFeed.State != recipefeed.StateNeverFetched {
		t.Fatalf("state=%q, want %q", system.RecipeFeed.State, recipefeed.StateNeverFetched)
	}
	if system.RecipeFeed.AcceptedGeneratedAt != nil || system.RecipeFeed.FetchedAt != nil || system.RecipeFeed.Stale {
		t.Fatalf("a feed that was never fetched reported more than it knows: %#v", system.RecipeFeed)
	}

	accepted := time.Now().UTC().Add(-31 * 24 * time.Hour).Truncate(time.Second)
	fetched := time.Now().UTC().Truncate(time.Second)
	h.api.SetRecipeFeedHealth(func() recipefeed.Health {
		return recipefeed.Health{State: recipefeed.StateUnreachable, AcceptedGeneratedAt: &accepted, FetchedAt: &fetched, Stale: true}
	})
	h.get(t, "/api/v1/system", &system)
	if system.RecipeFeed.State != recipefeed.StateUnreachable || !system.RecipeFeed.Stale {
		t.Fatalf("feed health was not passed through: %#v", system.RecipeFeed)
	}
	if system.RecipeFeed.AcceptedGeneratedAt == nil || !system.RecipeFeed.AcceptedGeneratedAt.Equal(accepted) {
		t.Fatalf("accepted_generated_at was not passed through: %#v", system.RecipeFeed)
	}
	if system.RecipeFeed.FetchedAt == nil || !system.RecipeFeed.FetchedAt.Equal(fetched) {
		t.Fatalf("fetched_at was not passed through: %#v", system.RecipeFeed)
	}
}
