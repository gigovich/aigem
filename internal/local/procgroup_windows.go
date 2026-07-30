//go:build windows

package local

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// DETACHED_PROCESS gives the daemon no console, so it is not torn down when the
// terminal that launched aigem goes away. That alone also puts it out of reach
// of console control events, so no process-group flag is needed here.
const detachedProcess = 0x00000008

func detachAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedProcess}
}

// openDaemon opens pid with the rights needed to check liveness and, when
// terminate is set, to kill it. PROCESS_QUERY_LIMITED_INFORMATION is used rather
// than the full query right because it is granted across integrity levels, so a
// daemon started from an elevated shell is still visible to an unelevated aigem.
func openDaemon(pid int, terminate bool) (windows.Handle, error) {
	access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE)
	if terminate {
		access |= windows.PROCESS_TERMINATE
	}
	return windows.OpenProcess(access, false, uint32(pid))
}

// processAlive reports whether pid is a live process. Opening a handle is not
// sufficient on Windows: a handle can still be opened for a process that has
// already exited but whose kernel object is kept alive by some other open
// handle. The wait state is what actually separates running from terminated.
func processAlive(pid int) bool {
	h, err := openDaemon(pid, false)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// terminateDaemon stops the daemon. Windows has no process-group signal and no
// catchable termination, so this kills the process outright - which makes
// killing the *wrong* process unacceptable. Windows recycles PIDs from a small
// pool, so a stale pidfile can easily name an unrelated live process; the image
// path is checked against the configured binary first, and anything that cannot
// be positively identified is left alone.
func terminateDaemon(pid int, binaryPath string) {
	h, err := openDaemon(pid, true)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	if !imageMatches(h, binaryPath) {
		return
	}
	_ = windows.TerminateProcess(h, 1)
}

func imageMatches(h windows.Handle, binaryPath string) bool {
	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return false
	}
	return exeName(windows.UTF16ToString(buf[:size])) == exeName(binaryPath)
}
