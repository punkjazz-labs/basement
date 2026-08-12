//go:build !linux

package update

import "os"

// Off Linux there is no /proc/self/exe, so the version subcommand resolves
// its own path instead. That is the path, not the running inode: a build on a
// development machine can still report its identity, and it says nothing
// about a binary replaced underneath it. The helper only ever ships for Linux
// ARM64, so the guarantee that degrades here is never the one that matters.

func runningExecutablePath() (string, error) { return os.Executable() }
