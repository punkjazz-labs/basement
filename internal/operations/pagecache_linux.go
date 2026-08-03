//go:build linux

package operations

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncAndEvict writes back a just-written window and then drops it from
// the page cache. A Spark holds most of its unified memory in a serving
// model, so a multi-hundred-gigabyte download that leaves its dirty
// pages to the kernel can stall the whole machine on writeback. Both
// calls are advisory: a filesystem that refuses them still produces the
// same bytes on disk, so failures are ignored rather than surfaced.
func syncAndEvict(f *os.File, offset, length int64) {
	if f == nil || length <= 0 {
		return
	}
	fd := int(f.Fd())
	_ = unix.SyncFileRange(fd, offset, length, unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER)
	_ = unix.Fadvise(fd, offset, length, unix.FADV_DONTNEED)
}

// evict drops an already-clean region, for read paths that would
// otherwise cache every byte they hash. Advisory, as above.
func evict(f *os.File, offset, length int64) {
	if f == nil || length <= 0 {
		return
	}
	_ = unix.Fadvise(int(f.Fd()), offset, length, unix.FADV_DONTNEED)
}
