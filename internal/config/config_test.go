package config

import "testing"

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
