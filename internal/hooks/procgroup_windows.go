//go:build windows

package hooks

import (
	"os/exec"
	"syscall"
)

// createNewProcessGroup is CREATE_NEW_PROCESS_GROUP: the hook gets its own group
// so console signals aimed at aigem do not reach it.
const createNewProcessGroup = 0x00000200

// configureProcessGroup is the Windows counterpart of the Unix process-group
// setup. Windows has no group-wide kill, so cancellation terminates the hook
// itself; the caller's WaitDelay still bounds how long a surviving child can
// hold the output pipes open.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	cmd.Cancel = func() error { return cmd.Process.Kill() }
}
