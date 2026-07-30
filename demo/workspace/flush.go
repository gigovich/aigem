package notes

// Flush writes pending notes, retrying with backoff until maxAttempts is
// reached, after which the note is dropped rather than retried forever.
func (s *Store) Flush() error {
	for n := 1; n <= maxAttempts; n++ {
		if err := s.write(); err == nil {
			return nil
		}
		s.attempts++
		sleep(backoff(n))
	}
	return errDropped
}
