//go:build !windows

package inventory

import (
	"os"
	"path/filepath"
	"syscall"
)

func diskSpace(path string) (int64, int64) {
	var stat syscall.Statfs_t
	probe := path
	for {
		if err := syscall.Statfs(probe, &stat); err == nil {
			return int64(stat.Bavail) * int64(stat.Bsize), int64(stat.Blocks) * int64(stat.Bsize)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return 0, 0
		}
		probe = parent
	}
}

func filesystemDevice(path string) (uint64, bool) {
	probe := path
	for {
		if stat, err := os.Stat(probe); err == nil {
			if system, ok := stat.Sys().(*syscall.Stat_t); ok {
				return uint64(system.Dev), true
			}
			return 0, false
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return 0, false
		}
		probe = parent
	}
}
