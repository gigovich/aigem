package notes

import "time"

// maxAttempts bounds how often a flush is retried before the note is dropped.
const maxAttempts = 5

// backoff returns the delay before attempt n, doubling each time up to a cap.
func backoff(n int) time.Duration {
	d := time.Second
	for i := 1; i < n && d < 30*time.Second; i++ {
		d *= 2
	}
	return d
}
