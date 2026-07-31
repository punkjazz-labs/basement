//go:build windows

package inventory

// The manager service only runs on Linux GB10 machines; Windows builds exist
// solely for the `setup` CLI, which never inspects local capacity. These
// stubs keep the package compiling there.

func diskSpace(string) (int64, int64) { return 0, 0 }

func filesystemDevice(string) (uint64, bool) { return 0, false }
