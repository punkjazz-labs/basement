//go:build !linux

package operations

import "os"

// Page-cache control is a Linux target concern; the macOS dev machine
// has no equivalent of SYNC_FILE_RANGE plus FADV_DONTNEED, and these
// hints never change the bytes written, so elsewhere they do nothing.
func syncAndEvict(f *os.File, offset, length int64) {}

func evict(f *os.File, offset, length int64) {}
