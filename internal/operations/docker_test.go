package operations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDockerCreateUsesConstrainedStructuredRequest(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r := recipes[0]
	var body map[string]any
	client := &DockerClient{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || !strings.Contains(request.URL.Path, "/containers/create") {
			t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL)
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ID":"container-id"}`))}, nil
	})}}
	id, err := client.Create(context.Background(), "managed-name", r.Runtime.Reference(), "/managed/model", r)
	if err != nil {
		t.Fatal(err)
	}
	if id != "container-id" {
		t.Fatalf("id=%s", id)
	}
	if body["Image"] != r.Runtime.Reference() {
		t.Fatalf("image is not digest pinned: %#v", body["Image"])
	}
	if _, ok := body["Entrypoint"].([]any); !ok {
		t.Fatalf("entrypoint is not structured argv: %#v", body["Entrypoint"])
	}
	host := body["HostConfig"].(map[string]any)
	if _, exists := host["Privileged"]; exists {
		t.Fatal("container requested privileged mode")
	}
	binds := host["Binds"].([]any)
	if len(binds) != 1 || binds[0] != "/managed/model:/model:ro" {
		t.Fatalf("unsafe binds: %#v", binds)
	}
}

func TestDockerNotFoundIsDistinguishableFromDaemonFailure(t *testing.T) {
	client := &DockerClient{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"missing"}`))}, nil
	})}}
	_, err := client.Container(context.Background(), "missing")
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Container()=%v", err)
	}
}
