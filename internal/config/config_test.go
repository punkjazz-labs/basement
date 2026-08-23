package config

import (
	"strings"
	"testing"
)

func TestListenAcceptsOneAddressOrAList(t *testing.T) {
	for _, test := range []struct {
		value string
		want  []string
	}{
		{"127.0.0.1:7070", []string{"127.0.0.1:7070"}},
		{"192.168.99.10:7070,100.64.30.7:7070", []string{"192.168.99.10:7070", "100.64.30.7:7070"}},
		{" 192.168.99.10:7070 , 100.64.30.7:7070 ", []string{"192.168.99.10:7070", "100.64.30.7:7070"}},
		{"[::1]:7070,127.0.0.1:7070", []string{"[::1]:7070", "127.0.0.1:7070"}},
		// The same socket named twice binds once, not twice.
		{"127.0.0.1:7070,127.0.0.1:7070", []string{"127.0.0.1:7070"}},
	} {
		got, err := parseListenAddresses(test.value)
		if err != nil {
			t.Errorf("parseListenAddresses(%q) failed: %v", test.value, err)
			continue
		}
		if strings.Join(got, "|") != strings.Join(test.want, "|") {
			t.Errorf("parseListenAddresses(%q) = %v, want %v", test.value, got, test.want)
		}
	}
}

// An address the owner asked for and the manager does not bind is an address
// nobody can reach. Every one of these is refused at startup instead.
func TestListenRefusesAnIncompleteOrMalformedList(t *testing.T) {
	for _, value := range []string{
		"",
		"   ",
		",",
		"192.168.99.10:7070,",
		",192.168.99.10:7070",
		"192.168.99.10:7070,,100.64.30.7:7070",
		"192.168.99.10:7070,  ,100.64.30.7:7070",
		"192.168.99.10",
		"192.168.99.10:7070,100.64.30.7",
		"192.168.99.10:seventy",
		"192.168.99.10:0",
		"192.168.99.10:70700",
		"192.168.99.10:7070,100.64.30.7:-1",
	} {
		if got, err := parseListenAddresses(value); err == nil {
			t.Errorf("parseListenAddresses(%q) = %v, want an error", value, got)
		}
	}
}

// The fleet listener follows the primary address only. The other addresses
// serve the console; the machine still has one fleet identity.
func TestFleetListenerFollowsThePrimaryAddressOfAList(t *testing.T) {
	addresses, err := parseListenAddresses("192.168.99.10:7070,100.64.30.7:7070")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Listen: addresses}
	if got := cfg.PrimaryListen(); got != "192.168.99.10:7070" {
		t.Fatalf("PrimaryListen() = %q, want the first address", got)
	}
	fleet, err := adjacentListenAddress(cfg.PrimaryListen())
	if err != nil || fleet != "192.168.99.10:7071" {
		t.Errorf("fleet listener = %q, %v; want 192.168.99.10:7071", fleet, err)
	}
	if got := (Config{}).PrimaryListen(); got != "" {
		t.Errorf("PrimaryListen() with no address = %q, want an empty string", got)
	}
}

func TestAdjacentFleetListenerFollowsConsoleAddress(t *testing.T) {
	for _, test := range []struct {
		console string
		want    string
	}{
		{"127.0.0.1:7070", "127.0.0.1:7071"},
		{"192.168.99.10:9000", "192.168.99.10:9001"},
		{"[::1]:7070", "[::1]:7071"},
	} {
		got, err := adjacentListenAddress(test.console)
		if err != nil || got != test.want {
			t.Errorf("adjacentListenAddress(%q)=%q, %v; want %q", test.console, got, err, test.want)
		}
	}
}

func TestAdjacentFleetListenerRejectsAConsoleWithoutAnotherPort(t *testing.T) {
	for _, console := range []string{"127.0.0.1", "127.0.0.1:65535", ""} {
		if _, err := adjacentListenAddress(console); err == nil {
			t.Errorf("adjacentListenAddress(%q) succeeded", console)
		}
	}
}
