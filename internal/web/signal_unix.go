//go:build !windows

package web

import "syscall"

// syscallZero is the "does this process exist" probe. Windows has no such
// signal, so the two platforms answer the question differently.
var syscallZero = syscall.Signal(0)
