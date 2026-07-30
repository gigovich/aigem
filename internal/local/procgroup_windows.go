//go:build windows

package local

import (
	"errors"
	"fmt"
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
// PROCESS_TERMINATE is not granted that way, so terminating one still fails -
// which is reported rather than swallowed.
func openDaemon(pid int, terminate bool) (windows.Handle, error) {
	access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE)
	if terminate {
		access |= windows.PROCESS_TERMINATE
	}
	return windows.OpenProcess(access, false, uint32(pid))
}

// daemonAlive reports whether pid is our live daemon. Opening a handle is not
// sufficient on Windows: a handle can still be opened for a process that has
// already exited but whose kernel object is kept alive by some other open
// handle. The wait state is what actually separates running from terminated.
//
// A false result is also what lets the caller discard the pidfile, so it is
// returned only when this is definitively not the daemon - gone, or a recycled
// pid now held by a different image. When the image cannot be read at all the
// answer is "alive", which keeps the pidfile and leaves the daemon addressable.
func daemonAlive(pid int, binaryPath string) bool {
	h, err := openDaemon(pid, false)
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil || ev != uint32(windows.WAIT_TIMEOUT) {
		return false
	}
	image, err := processImage(h)
	if err != nil {
		return true
	}
	return exeName(image) == exeName(binaryPath)
}

// terminateDaemon stops the daemon. Windows has no process-group signal and no
// catchable termination, so this kills the process outright - which makes
// killing the *wrong* process unacceptable. Windows recycles PIDs from a small
// pool, so a stale pidfile can easily name an unrelated live process; the image
// path is checked against the configured binary first.
//
// An error means nothing was killed, and the caller must keep the pidfile so the
// daemon stays addressable instead of being orphaned.
func terminateDaemon(pid int, binaryPath string) error {
	h, err := openDaemon(pid, true)
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()

	image, err := processImage(h)
	if err != nil {
		return fmt.Errorf("identify process %d: %w", pid, err)
	}
	if exeName(image) != exeName(binaryPath) {
		return fmt.Errorf("process %d is %q, not the configured %q; refusing to terminate it",
			pid, image, binaryPath)
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("terminate process %d: %w", pid, err)
	}
	return nil
}

// processImage returns the full image path of an open process, growing the
// buffer until it fits. MAX_PATH is not a real ceiling: a long-path-aware
// installation can sit well beyond it, and a truncated answer here would mean
// refusing to stop a legitimate daemon.
func processImage(h windows.Handle) (string, error) {
	for n := uint32(windows.MAX_PATH); n <= 32768; n *= 2 {
		buf := make([]uint16, n)
		size := n
		err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size)
		if err == nil {
			return windows.UTF16ToString(buf[:size]), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", err
		}
	}
	return "", errors.New("image path longer than the maximum supported length")
}
