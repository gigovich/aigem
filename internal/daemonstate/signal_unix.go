//go:build !windows

package daemonstate

import "syscall"

// signalZero is the "does this process exist" probe. Windows has no such
// signal, so the two platforms answer the question differently.
var signalZero = syscall.Signal(0)
