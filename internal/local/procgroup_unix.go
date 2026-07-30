//go:build !windows

package local

import (
	"os"
	"syscall"
)

// detachAttrs starts the daemon in its own process group, so it outlives aigem
// and Stop can signal the whole tree.
func detachAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// terminateTree asks the daemon's process group, then the process itself, to stop.
func terminateTree(pid int) {
	// Negative pid targets the whole process group (Setpgid at spawn).
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
}

// processAlive reports whether pid is a live process. Signal 0 performs the
// permission and existence checks without delivering anything.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
