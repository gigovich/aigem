// Package notes is the sample project the demo recording explores. It is
// deliberately tiny, and it compiles so the repository's own build and lint
// stay clean.
package notes

import (
	"errors"
	"time"
)

// Store keeps notes in memory until they are flushed.
type Store struct {
	notes    map[string]Note
	attempts int
}

// Note is a single stored note.
type Note struct {
	ID   string
	Body string
}

// errDropped is returned when a note could not be flushed within maxAttempts.
var errDropped = errors.New("note dropped after too many failed flushes")

// write pushes the pending notes to the backing store.
func (s *Store) write() error {
	if len(s.notes) == 0 {
		return nil
	}
	return errors.New("backing store unavailable")
}

func sleep(d time.Duration) { time.Sleep(d) }
