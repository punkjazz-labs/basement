//go:build unix

package update

// The manager update transaction runs on the Spark, which is Unix. These two
// operations need guarantees the standard library does not express: opening a
// file without following a symlink and proving it is still the same inode
// afterwards, and taking a lock no second updater can ignore. Both are here
// rather than in updater.go so the manager still cross-compiles for Windows,
// which is published so one binary can drive an install over SSH and never
// reaches the update path. See updater_other.go for what happens if it does.

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func copyRegularFile(source, destination string, mode os.FileMode) error {
	descriptor, err := unix.Open(source, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(descriptor), source)
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(descriptor, &before); err != nil {
		return err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 {
		return errors.New("staged update input is not one regular file")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".copy-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, file); err != nil {
		temporary.Close()
		return err
	}
	var after unix.Stat_t
	if err := unix.Fstat(descriptor, &after); err != nil {
		temporary.Close()
		return err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim {
		temporary.Close()
		return errors.New("staged update input changed while it was copied")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func acquireUpdateLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another manager update transaction is running")
	}
	return file, nil
}
