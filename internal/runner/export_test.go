package runner

import "time"

// SetSessionStartTimeout shrinks the SessionStart bound for a test and restores
// it afterwards.
func SetSessionStartTimeout(d time.Duration) func() {
	old := sessionStartTimeout
	sessionStartTimeout = d
	return func() { sessionStartTimeout = old }
}
