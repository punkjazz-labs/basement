package main

import "testing"

func TestSoleConsoleOwner(t *testing.T) {
	cases := []struct {
		processCount int
		want         bool
	}{
		{processCount: 0, want: false}, // GetConsoleProcessList failed; don't pause on a guess
		{processCount: 1, want: true},  // only this process — the double-click case
		{processCount: 2, want: false}, // shared with an interactive shell
		{processCount: 5, want: false},
	}
	for _, c := range cases {
		if got := soleConsoleOwner(c.processCount); got != c.want {
			t.Errorf("soleConsoleOwner(%d) = %v, want %v", c.processCount, got, c.want)
		}
	}
}
