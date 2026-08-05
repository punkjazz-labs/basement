//go:build !unix

package update

import (
	"errors"
	"os"
)

// The manager only ever updates itself on the Spark it manages, which is
// Linux: the slot layout, the systemd units and the root updater are all
// Linux. The Windows build exists so one binary can drive an install over
// SSH, and it never reaches this path. Refusing is honest; a portable
// approximation would be a copy without the symlink and same-inode
// guarantees, and a lock a second updater could ignore.
var errUpdateNotSupported = errors.New("a manager update transaction does not run on this operating system")

func copyRegularFile(source, destination string, mode os.FileMode) error {
	return errUpdateNotSupported
}

func acquireUpdateLock(path string) (*os.File, error) {
	return nil, errUpdateNotSupported
}
