package httpapi

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

const responseProxyTestTimeout = 5 * time.Second

// newResponsesProxyServer puts one ready text recipe behind the manager and
// points its host port at upstream. It exercises the public proxy without
// depending on a live model runtime or claiming anything about model output.
func newResponsesProxyServer(t *testing.T, upstream *httptest.Server) (*httptest.Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	database, err := store.Open(dataDir + "/manager.db")
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
	target, ok := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("text recipe missing")
	}
	target.Service.DefaultHostPort = upstream.Listener.Addr().(*net.TCPAddr).Port
	local := &apiExecutor{done: map[string]bool{}}
	runner := engine.New(database, local, []recipe.Recipe{target})
	api := New("test-version", dataDir, authManager, database, readyInventory{}, local, runner, []recipe.Recipe{target})
	t.Cleanup(api.Close)
	if err := database.SetInstalled(context.Background(), store.InstalledModel{
		RecipeID: target.ID, RecipeVersion: target.Version, Status: "ready",
		ArtifactPath: "/managed/" + target.ID, ContainerID: "container-test", Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	_, secret, err := database.CreateAPIKey(context.Background(), "responses-proxy-test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	return server, secret
}

func TestResponsesProxyForwardsStreamedToolAndUsageEventsUnchanged(t *testing.T) {
	const want = "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_test\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_test\",\"delta\":\"{\\\"city\\\":\\\"Paris\\\"}\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":7,\"total_tokens\":19}}}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("upstream request=%s %s, want POST /v1/responses", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not flushable")
			return
		}
		for _, event := range strings.Split(want, "\n\n") {
			if event == "" {
				continue
			}
			_, _ = io.WriteString(w, event+"\n\n")
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)
	manager, secret := newResponsesProxyServer(t, upstream)

	req, err := http.NewRequest(http.MethodPost, manager.URL+"/v1/responses", bytes.NewBufferString(`{"model":"codex-test","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if got := string(body); got != want {
		t.Fatalf("streamed Responses body changed\n got: %q\nwant: %q", got, want)
	}
}

func TestResponsesProxyCancelsUpstreamWhenClientRequestIsCanceled(t *testing.T) {
	started := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		close(started)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-time.After(responseProxyTestTimeout):
		}
	}))
	t.Cleanup(upstream.Close)
	manager, secret := newResponsesProxyServer(t, upstream)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, manager.URL+"/v1/responses", bytes.NewBufferString(`{"model":"codex-test","input":"wait"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	result := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(req)
		if response != nil {
			response.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(responseProxyTestTimeout):
		t.Fatal("upstream did not receive the Responses request")
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-upstreamDone:
		t.Fatal("upstream request ended without observing cancellation")
	case <-time.After(responseProxyTestTimeout):
		t.Fatal("client cancellation did not reach upstream request context")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled client request unexpectedly succeeded")
		}
	case <-time.After(responseProxyTestTimeout):
		t.Fatal("client request did not return after cancellation")
	}
}
