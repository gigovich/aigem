//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup runs the command in its own process group and kills the
// whole group when its context is cancelled, so a command that spawns children
// does not leak them. Mirrors internal/hooks, for the same reason.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
