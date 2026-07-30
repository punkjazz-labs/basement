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
	r, ok := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("Qwen 35 recipe missing")
	}
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
	id, err := client.Create(context.Background(), "managed-name", r.Runtime.Reference(), []string{"/managed/model"}, "/managed/cache", r)
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
	if len(binds) != 2 || binds[0] != "/managed/model:/model:ro" || binds[1] != "/managed/cache:/root/.cache:rw" {
		t.Fatalf("unsafe binds: %#v", binds)
	}
	if host["ReadonlyRootfs"] != true || host["ShmSize"] != float64(34359738368) {
		t.Fatalf("runtime constraints missing: %#v", host)
	}
}

func TestLagunaUsesSeparateReadOnlyDrafterMount(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "laguna-s-2-1-nvfp4-dflash-1s")
	if !ok {
		t.Fatal("Laguna recipe missing")
	}
	var body map[string]any
	client := &DockerClient{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ID":"container-id"}`))}, nil
	})}}
	_, err = client.Create(context.Background(), "laguna", r.Runtime.Reference(), []string{"/owned/target", "/owned/draft"}, "/owned/cache", r)
	if err != nil {
		t.Fatal(err)
	}
	binds := body["HostConfig"].(map[string]any)["Binds"].([]any)
	if len(binds) != 3 || binds[0] != "/owned/target:/model:ro" || binds[1] != "/owned/draft:/drafter:ro" {
		t.Fatalf("Laguna mounts=%#v", binds)
	}
	joined := strings.Join(toStrings(body["Cmd"].([]any)), " ")
	if !strings.Contains(joined, `"method":"dflash"`) || !strings.Contains(joined, `"model":"/drafter"`) {
		t.Fatalf("DFlash arguments missing: %s", joined)
	}
}

func toStrings(values []any) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index], _ = value.(string)
	}
	return result
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
