//go:build linux

package update

// The root updater helper reports the bytes actually executing, not the bytes
// at its own path: those two differ for the whole window between a swap and
// the next run, which is exactly the window someone asking the question cares
// about. On Linux /proc/self/exe is that guarantee, and Linux is where the
// helper runs.

func runningExecutablePath() (string, error) { return "/proc/self/exe", nil }
