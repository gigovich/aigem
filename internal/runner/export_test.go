package runner

import "time"

// SessionStartTimeout reports the shipped bound, so a test can pin the value
// and not only the mechanism.
func SessionStartTimeout() time.Duration { return sessionStartTimeout }

// SetSessionStartTimeout shrinks the SessionStart bound for a test and restores
// it afterwards. Tests in this package do not run in parallel, which is what
// makes writing a package-level variable safe here.
func SetSessionStartTimeout(d time.Duration) func() {
	old := sessionStartTimeout
	sessionStartTimeout = d
	return func() { sessionStartTimeout = old }
}
