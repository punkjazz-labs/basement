package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The console binds every configured address and one server answers on all
// of them. Shutting that server down closes every listener: no address is
// left holding a port after the manager stops.
func TestListenConsoleServesEveryAddressFromOneServer(t *testing.T) {
	listeners, err := listenConsole([]string{"127.0.0.1:0", "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listeners) != 2 {
		t.Fatalf("listenConsole bound %d addresses, want 2", len(listeners))
	}
	first, second := listeners[0].Addr().String(), listeners[1].Addr().String()
	if first == second {
		t.Fatalf("both listeners bound the same address %q", first)
	}

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "basement")
	}), ReadHeaderTimeout: time.Second}
	for _, listener := range listeners {
		go server.Serve(listener)
	}

	for _, address := range []string{first, second} {
		body, err := get(t, address)
		if err != nil {
			t.Fatalf("GET %s failed: %v", address, err)
		}
		if body != "basement" {
			t.Errorf("GET %s = %q, want the same console on every address", address, body)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{first, second} {
		if _, err := get(t, address); err == nil {
			t.Errorf("%s still answered after shutdown", address)
		}
	}
}

// An address that cannot be bound stops the whole start. It is never skipped
// quietly: the owner would then trust an address nobody can reach. The
// addresses that did bind are released again, so the failed start holds
// nothing.
func TestListenConsoleFailsLoudlyAndReleasesWhatItBound(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freeAddress := free.Addr().String()
	if err := free.Close(); err != nil {
		t.Fatal(err)
	}

	listeners, err := listenConsole([]string{freeAddress, taken.Addr().String()})
	if err == nil {
		for _, listener := range listeners {
			listener.Close()
		}
		t.Fatal("listenConsole succeeded with an address that was already taken")
	}
	if !strings.Contains(err.Error(), taken.Addr().String()) {
		t.Errorf("error does not name the address that failed: %v", err)
	}

	// The first address must be free again, not held by the failed start.
	reclaimed, err := net.Listen("tcp", freeAddress)
	if err != nil {
		t.Fatalf("a failed start kept %s bound: %v", freeAddress, err)
	}
	reclaimed.Close()
}

func TestListenConsoleBindsOneAddressAsBefore(t *testing.T) {
	listeners, err := listenConsole([]string{"127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer listeners[0].Close()
	if len(listeners) != 1 {
		t.Errorf("listenConsole bound %d addresses, want 1", len(listeners))
	}
}

func get(t *testing.T, address string) (string, error) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + address + "/")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<10))
	return string(payload), err
}
