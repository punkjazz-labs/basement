//go:build !windows

package main

// pauseBeforeExit is a no-op outside Windows: only Windows detaches a
// double-clicked process from any inherited shell into its own console, so
// only Windows needs to hold that window open for its user to read.
func pauseBeforeExit() {}
