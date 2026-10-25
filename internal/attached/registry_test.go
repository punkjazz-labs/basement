package attached

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func declaration(id, healthURL string, nodes ...string) string {
	var list strings.Builder
	for _, node := range nodes {
		list.WriteString("    - node_name: " + node + "\n")
	}
	return "schema_version: 1\nid: " + id + "\ndisplay_name: Service " + id + "\nkind: chat\nmanaged_by: systemd\nhealth_url: " + healthURL +
		"\ntopology:\n  spark_count: " + string(rune('0'+len(nodes))) + "\n  nodes:\n" + list.String()
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newTestRegistry gives a registry over a fresh data directory whose
// attached-services.d exists, with a frozen clock the test moves by hand.
func newTestRegistry(t *testing.T) (*Registry, string, *time.Time) {
	t.Helper()
	dataDir := t.TempDir()
	registry := NewRegistry(dataDir, nil)
	if err := os.Mkdir(registry.Directory(), 0o750); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	registry.now = func() time.Time { return clock }
	registry.timeout = 200 * time.Millisecond
	return registry, registry.Directory(), &clock
}

func TestMissingDirectoryLeavesTheFeatureInert(t *testing.T) {
	registry := NewRegistry(t.TempDir(), nil)
	registry.Reload()
	snapshot := registry.Snapshot()
	if len(snapshot.Services) != 0 || len(snapshot.Problems) != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if _, held := registry.Holder("node", "head-box"); held {
		t.Fatal("no declaration can hold a Spark")
	}
	registry.CheckNow(context.Background())
}

func TestReloadReadsValidFilesAndReportsTheRest(t *testing.T) {
	registry, dir, _ := newTestRegistry(t)
	writeFile(t, dir, "b-good.yaml", declaration("good-service", "http://127.0.0.1:1/health", "head-box", "worker-box"))
	writeFile(t, dir, "a-broken.yaml", "schema_version: 1\nid: [\n")
	writeFile(t, dir, "c-duplicate.yaml", declaration("good-service", "http://127.0.0.1:2/health", "other-box"))
	writeFile(t, dir, "notes.txt", "not a declaration")
	writeFile(t, dir, ".hidden.yaml", "also ignored")
	writeFile(t, dir, "d-typo.yaml", declaration("typo-service", "http://127.0.0.1:3/health", "third-box")+"health_url_typo: x\n")
	registry.Reload()

	snapshot := registry.Snapshot()
	if len(snapshot.Services) != 1 || snapshot.Services[0].File != "b-good.yaml" || snapshot.Services[0].Health.Status != StatusUnknown {
		t.Fatalf("services = %+v", snapshot.Services)
	}
	if len(snapshot.Problems) != 3 {
		t.Fatalf("problems = %+v", snapshot.Problems)
	}
	wants := map[string]string{
		"a-broken.yaml":    "not a valid declaration",
		"c-duplicate.yaml": "id good-service is already declared in b-good.yaml",
		"d-typo.yaml":      "field health_url_typo not found",
	}
	for _, problem := range snapshot.Problems {
		if !strings.Contains(problem.Error, wants[problem.File]) {
			t.Fatalf("problem for %s = %q, want %q", problem.File, problem.Error, wants[problem.File])
		}
	}
	if holder, held := registry.Holder("", "WORKER-BOX"); !held || holder != "Service good-service" {
		t.Fatalf("holder = %q, %v", holder, held)
	}
	if _, held := registry.Holder("", "other-box"); held {
		t.Fatal("a refused duplicate must not hold the Spark it names")
	}
	if _, held := registry.Holder("", "third-box"); held {
		t.Fatal("a refused file must not hold the Spark it names")
	}

	if err := os.Remove(filepath.Join(dir, "b-good.yaml")); err != nil {
		t.Fatal(err)
	}
	registry.Reload()
	if _, held := registry.Holder("", "worker-box"); held {
		t.Fatal("removing the file releases the Spark")
	}
	if snapshot := registry.Snapshot(); len(snapshot.Services) != 1 || snapshot.Services[0].Declaration.ID != "good-service" ||
		snapshot.Services[0].File != "c-duplicate.yaml" {
		t.Fatalf("once the first file is gone the other one owns its id: %+v", snapshot.Services)
	}
}

func TestUnreadableDirectoryIsAProblemNotAPanic(t *testing.T) {
	dataDir := t.TempDir()
	registry := NewRegistry(dataDir, nil)
	writeFile(t, dataDir, DirectoryName, "a file where the directory should be")
	registry.Reload()
	snapshot := registry.Snapshot()
	if len(snapshot.Problems) != 1 || !strings.Contains(snapshot.Problems[0].Error, "not a directory") {
		t.Fatalf("problems = %+v", snapshot.Problems)
	}
}

func TestCheckReportsHealthFromOneGet(t *testing.T) {
	var requests []string
	var mu sync.Mutex
	status := atomic.Int32{}
	status.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	registry, dir, clock := newTestRegistry(t)
	writeFile(t, dir, "service.yaml", declaration("probe-service", server.URL+"/health", "head-box"))
	registry.Reload()
	registry.CheckNow(context.Background())
	health := registry.Snapshot().Services[0].Health
	if health.Status != StatusHealthy || health.LastError != "" || health.LastCheckedAt != clock.Format(time.RFC3339Nano) || health.LatencyMS < 0 {
		t.Fatalf("health = %+v", health)
	}

	status.Store(http.StatusServiceUnavailable)
	registry.CheckNow(context.Background())
	health = registry.Snapshot().Services[0].Health
	if health.Status != StatusUnhealthy || health.LastError != "the health check answered HTTP 503" {
		t.Fatalf("health = %+v", health)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range requests {
		if request != "GET /health" {
			t.Fatalf("only GET of the health URL is allowed, saw %q", request)
		}
	}
}

func TestCheckFollowsNoRedirect(t *testing.T) {
	var elsewhere atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			elsewhere.Add(1)
			return
		}
		http.Redirect(w, r, "/v1/videos/sync", http.StatusFound)
	}))
	defer server.Close()
	registry, dir, _ := newTestRegistry(t)
	writeFile(t, dir, "service.yaml", declaration("redirect-service", server.URL+"/health", "head-box"))
	registry.Reload()
	registry.CheckNow(context.Background())
	health := registry.Snapshot().Services[0].Health
	if health.Status != StatusUnhealthy || health.LastError != "the health check answered HTTP 302" {
		t.Fatalf("health = %+v", health)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("a redirect must never take the check to another path")
	}
}

func TestCheckWithoutAnAnswerIsUnhealthy(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	registry, dir, _ := newTestRegistry(t)
	writeFile(t, dir, "a.yaml", declaration("slow-service", slow.URL+"/health", "box-a"))
	writeFile(t, dir, "b.yaml", declaration("gone-service", closedURL+"/health", "box-b"))
	registry.Reload()
	registry.CheckNow(context.Background())
	services := registry.Snapshot().Services
	if services[0].Health.Status != StatusUnhealthy || services[0].Health.LastError != "no answer within 200ms" {
		t.Fatalf("slow health = %+v", services[0].Health)
	}
	if services[1].Health.Status != StatusUnhealthy || !strings.Contains(services[1].Health.LastError, "connection refused") {
		t.Fatalf("closed health = %+v", services[1].Health)
	}
}

func TestChecksRunOnTheirOwnInterval(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	registry, dir, clock := newTestRegistry(t)
	writeFile(t, dir, "service.yaml", declaration("interval-service", server.URL+"/health", "head-box")+"health_interval_seconds: 10\n")
	registry.Reload()

	var checks sync.WaitGroup
	registry.checkDue(context.Background(), &checks)
	checks.Wait()
	if calls.Load() != 1 || registry.Snapshot().Services[0].Health.Status != StatusHealthy {
		t.Fatalf("a new declaration is checked at once; calls = %d", calls.Load())
	}
	*clock = clock.Add(9 * time.Second)
	registry.checkDue(context.Background(), &checks)
	checks.Wait()
	if calls.Load() != 1 {
		t.Fatalf("checked again before its interval; calls = %d", calls.Load())
	}
	*clock = clock.Add(time.Second)
	registry.checkDue(context.Background(), &checks)
	checks.Wait()
	if calls.Load() != 2 {
		t.Fatalf("not checked again after its interval; calls = %d", calls.Load())
	}

	// The same file, read again unchanged, keeps its answer; a new health URL
	// starts again from unknown, because the old answer was about elsewhere.
	registry.Reload()
	if registry.Snapshot().Services[0].Health.Status != StatusHealthy {
		t.Fatal("an unchanged declaration must keep its health across a reload")
	}
	writeFile(t, dir, "service.yaml", declaration("interval-service", server.URL+"/other", "head-box"))
	registry.Reload()
	if registry.Snapshot().Services[0].Health.Status != StatusUnknown {
		t.Fatal("a changed health URL must start again from unknown")
	}
}

func TestReloadIsDueOnChangeOrInterval(t *testing.T) {
	dataDir := t.TempDir()
	registry := NewRegistry(dataDir, nil)
	clock := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	registry.now = func() time.Time { return clock }
	if !registry.reloadDue() {
		t.Fatal("a registry that has never read is due")
	}
	registry.Reload()
	if registry.reloadDue() {
		t.Fatal("nothing changed")
	}
	if err := os.Mkdir(registry.Directory(), 0o750); err != nil {
		t.Fatal(err)
	}
	if !registry.reloadDue() {
		t.Fatal("a directory that appeared is a change")
	}
	registry.Reload()
	if registry.reloadDue() {
		t.Fatal("nothing changed since the directory was read")
	}
	later := time.Date(2026, 10, 25, 12, 0, 30, 0, time.UTC)
	if err := os.Chtimes(registry.Directory(), later, later); err != nil {
		t.Fatal(err)
	}
	if !registry.reloadDue() {
		t.Fatal("a directory whose entries changed is due")
	}
	registry.Reload()
	clock = clock.Add(ReloadInterval)
	if !registry.reloadDue() {
		t.Fatal("an edit inside a file is picked up by the interval")
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	registry, dir, _ := newTestRegistry(t)
	writeFile(t, dir, "service.yaml", declaration("run-service", server.URL+"/health", "head-box"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		registry.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if calls.Load() == 0 {
		t.Fatal("Run reads the directory and checks a new declaration without waiting for a tick")
	}
}
