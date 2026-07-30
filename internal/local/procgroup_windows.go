//go:build windows

package local

import (
	"os"
	"syscall"
)

// Creation flags that let the daemon survive aigem exiting: DETACHED_PROCESS
// gives it no console, CREATE_NEW_PROCESS_GROUP keeps console signals aimed at
// aigem from reaching it.
const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

func detachAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup}
}

// terminateTree stops the daemon. Windows has no process-group signal, so the
// process is terminated directly; llama-server spawns no children of its own.
func terminateTree(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// processAlive reports whether pid is a live process. On Windows FindProcess
// opens a handle to the process and fails when there is none, which is the
// existence check itself - unlike Unix, where it always succeeds.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
