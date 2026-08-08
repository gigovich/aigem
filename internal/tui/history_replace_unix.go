//go:build !windows

package tui

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openNoFollow refuses to open a symlink, so a planted one cannot redirect the
// chmod that follows. Windows has no equivalent flag.
const openNoFollow = unix.O_NOFOLLOW

func replaceInputHistoryFile(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// secureInputHistoryDir tightens the directory through a file descriptor rather
// than by path, so it cannot be aimed at a symlink's target.
func secureInputHistoryDir(path string) error {
	dir, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	return errors.Join(dir.Chmod(0o700), dir.Close())
}

func tryLockInputHistoryFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func unlockInputHistoryFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
