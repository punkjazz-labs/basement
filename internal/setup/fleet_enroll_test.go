package setup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestEnrollInstalledFleetUsesOneControllerForAllMembers(t *testing.T) {
	previous := fleetHTTPClient
	t.Cleanup(func() { fleetHTTPClient = previous })
	tokens := map[string]string{
		"192.168.99.10:7070": "token-controller",
		"192.168.99.20:7070": "token-member-one",
		"192.168.99.21:7070": "token-member-two",
	}
	joinCalls := 0
	var joinBodies []string
	fleetHTTPClient = &http.Client{Transport: setupRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload []byte
		if request.Body != nil {
			payload, _ = io.ReadAll(request.Body)
		}
		switch request.URL.Path {
		case "/api/v1/auth/pair":
			var body struct {
				Token string `json:"token"`
			}
			_ = json.Unmarshal(payload, &body)
			if body.Token != tokens[request.URL.Host] {
				return nil, errors.New("pairing token went to the wrong installed console")
			}
			return setupResponse(http.StatusOK, `{"authenticated":true,"csrf_token":"csrf-value"}`, map[string]string{"Set-Cookie": "basement_session=session-value; Path=/"}), nil
		case "/api/v1/fleet/join-code":
			if request.Header.Get("Cookie") == "" || request.Header.Get("X-CSRF-Token") != "csrf-value" {
				return nil, errors.New("member join code lacked console authority")
			}
			return setupResponse(http.StatusCreated, `{"code":"v1.pin.secret","expires_at":"2026-08-05T10:10:00Z"}`, nil), nil
		case "/api/v1/fleet/join":
			if request.URL.Host != "192.168.99.10:7070" {
				return nil, errors.New("a member was used as the controller")
			}
			joinCalls++
			joinBodies = append(joinBodies, string(payload))
			return setupResponse(http.StatusCreated, `{"node":{"node_id":"node_test"}}`, nil), nil
		default:
			return nil, errors.New("unexpected installed-console endpoint")
		}
	})}
	installed := []Machine{
		{Target: "spark-head", Result: InstallResult{ConsoleURL: "http://192.168.99.10:7070", Token: tokens["192.168.99.10:7070"]}},
		{Target: "spark-worker", Result: InstallResult{ConsoleURL: "http://192.168.99.20:7070", Token: tokens["192.168.99.20:7070"]}},
		{Target: "edgexpert-alpha", Result: InstallResult{ConsoleURL: "http://192.168.99.21:7070", Token: tokens["192.168.99.21:7070"]}},
	}
	if err := EnrollInstalledFleet(context.Background(), installed); err != nil {
		t.Fatal(err)
	}
	if joinCalls != 2 {
		t.Fatalf("controller join calls=%d, want 2", joinCalls)
	}
	joined := strings.Join(joinBodies, "\n")
	for _, want := range []string{"https://192.168.99.20:7071", "https://192.168.99.21:7071"} {
		if !strings.Contains(joined, want) {
			t.Errorf("join requests did not contain %s: %s", want, joined)
		}
	}
	for _, token := range tokens {
		if strings.Contains(joined, token) {
			t.Fatal("a pairing token entered a fleet join request")
		}
	}
}

func TestInstallMoreEnrollsOwnerApprovedMachinesIntoOneFleet(t *testing.T) {
	fakeMachines(t, map[string]*fakeRunner{
		"spark-worker": installableRunner("192.168.99.20", "spark-worker"),
	}, nil)
	previous := enrolInstalledFleet
	t.Cleanup(func() { enrolInstalledFleet = previous })
	enrolled := 0
	enrolInstalledFleet = func(_ context.Context, installed []Machine) error {
		enrolled = len(installed)
		return nil
	}
	ui := &stubUI{listenMode: ListenLAN, confirmAlways: []bool{true}}
	first := Machine{Target: "spark-head", Result: InstallResult{ConsoleURL: "http://192.168.99.10:7070", Token: "token-controller"}}
	installed := InstallMore(context.Background(), ui, first, []string{"spark-worker"}, LocalFileSource{Path: "/tmp/binary"}, "nvidia")
	if len(installed) != 2 || enrolled != 2 {
		t.Fatalf("installed=%d enrolled=%d, want 2", len(installed), enrolled)
	}
	if len(ui.nextSteps) != 1 || !strings.Contains(strings.Join(ui.nextSteps[0], " "), first.Result.ConsoleURL) {
		t.Fatalf("setup did not return one controller dashboard: %v", ui.nextSteps)
	}
}

type setupRoundTripFunc func(*http.Request) (*http.Response, error)

func (f setupRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func setupResponse(status int, body string, headers map[string]string) *http.Response {
	header := make(http.Header)
	for name, value := range headers {
		header.Set(name, value)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}
