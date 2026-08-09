//go:build windows

package web

import "os"

// Windows has no signal 0. FindProcess there already fails for a pid that is
// not running, so the probe is a no-op that always succeeds once we have a
// handle.
var syscallZero os.Signal = nil
