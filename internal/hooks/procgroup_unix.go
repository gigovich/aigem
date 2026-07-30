//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup runs the hook in its own process group and kills the
// whole group on timeout, so a command that spawns children does not leak them.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
