//go:build !windows

package bot

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockBotConfig(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockBotConfig(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
