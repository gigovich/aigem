package notes

import "time"

// flushMaxAttempts bounds how often a flush is retried before the batch is
// dropped.
const flushMaxAttempts = 4

// flushDelay is the wait before attempt n of a flush.
func flushDelay(n int) time.Duration {
	d := 200 * time.Millisecond
	for i := 1; i < n && d < 5*time.Second; i++ {
		d *= 2
	}
	return d
}

// Flush drains the store and hands the batch to the sink, retrying with a
// backoff. A batch that never lands is dropped and reported.
func (s *Store) Flush() error {
	batch := s.Drain()
	if len(batch) == 0 {
		return nil
	}
	var err error
	for attempt := 1; attempt <= flushMaxAttempts; attempt++ {
		if err = s.sink(batch); err == nil {
			return nil
		}
		time.Sleep(flushDelay(attempt))
	}
	return err
}
