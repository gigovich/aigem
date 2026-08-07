package notes

import (
	"errors"
	"sync"
	"time"
)

// ErrClosed is returned once the store has been closed.
var ErrClosed = errors.New("notes: store is closed")

// Note is one recorded line of text.
type Note struct {
	Body    string
	Created time.Time
}

// Store buffers notes in memory until they are flushed to the sink.
type Store struct {
	mu     sync.Mutex
	notes  []Note
	closed bool
	sink   func([]Note) error
}

// NewStore returns a store that hands flushed notes to sink.
func NewStore(sink func([]Note) error) *Store {
	return &Store{sink: sink}
}

// Add buffers a note.
func (s *Store) Add(body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.notes = append(s.notes, Note{Body: body, Created: time.Now()})
	return nil
}

// Len reports how many notes are buffered.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.notes)
}

// Drain removes and returns the buffered notes.
func (s *Store) Drain() []Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.notes
	s.notes = nil
	return out
}

// Close marks the store closed; later Adds fail.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// retryDelay is the wait before attempt n of a store-side operation.
func retryDelay(n int) time.Duration {
	d := 200 * time.Millisecond
	for i := 1; i < n && d < 5*time.Second; i++ {
		d *= 2
	}
	return d
}

// storeMaxAttempts bounds store-side retries.
const storeMaxAttempts = 4
