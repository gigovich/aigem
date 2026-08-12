// Package fanout delivers an ordered stream to several readers without letting
// the slowest one set the pace.
//
// It was extracted from the session fan-out so a second ordered stream - a chat
// store's frames - could reuse the same behaviour rather than reimplement its
// subtler parts: history the reader asked for must not count as falling behind,
// and a reader that is dropped must still be told where to resume.
package fanout

import "sync"

// Config describes one subscriber. SeqOf and OnDrop are what keep this package
// ignorant of the stream's element type: SeqOf reads an element's sequence
// number, and OnDrop builds the element sent to a reader that fell too far
// behind, carrying the last sequence it definitely received.
type Config[T any] struct {
	// QueueCap bounds how far behind the live stream a reader may fall.
	QueueCap int
	// Backlog is history the reader asked for. It does not count against
	// QueueCap: a stream longer than the cap would otherwise be dropped for
	// being long rather than for being slow, which is the opposite of what the
	// cap is for.
	Backlog []T
	// SkipTo is the highest sequence the reader is known to already have. It
	// exists for a subscriber registered before its history was fetched: what
	// arrives in that window is kept, and what the history will also carry is
	// dropped when it is spliced in.
	SkipTo uint64
	SeqOf  func(T) uint64
	OnDrop func(last uint64) T
}

// Sub is one attached reader's view of the stream. Elements are queued here
// rather than pushed straight into the reader's channel, because the
// alternative is a fan-out that runs at the speed of its slowest reader: a
// phone on a bad connection would stall the writer for everyone.
//
// The queue is bounded. A subscriber that falls further behind than the cap is
// dropped with OnDrop's element carrying the last sequence number it definitely
// received, which is all a reader needs to reconnect and catch up.
type Sub[T any] struct {
	out    chan T
	seqOf  func(T) uint64
	onDrop func(uint64) T

	mu      sync.Mutex
	q       []T
	dead    bool
	lastSeq uint64
	skipTo  uint64
	// headroom is how much of the queue is backlog rather than live tail.
	headroom int
	cap      int
	wake     chan struct{}
	closing  chan struct{}
	once     sync.Once
}

// New builds a subscriber. Call Run in its own goroutine to pump Out.
func New[T any](c Config[T]) *Sub[T] {
	s := &Sub[T]{
		out:     make(chan T),
		seqOf:   c.SeqOf,
		onDrop:  c.OnDrop,
		q:       c.Backlog,
		skipTo:  c.SkipTo,
		cap:     c.QueueCap,
		wake:    make(chan struct{}, 1),
		closing: make(chan struct{}),
	}
	if n := len(c.Backlog); n > 0 {
		s.lastSeq = s.seqOf(c.Backlog[n-1])
		s.headroom = n
		s.signal()
	}
	return s
}

// Out is the reader's channel. It is closed when the subscriber is dropped or
// detached, so a range loop over it ends.
func (s *Sub[T]) Out() <-chan T { return s.out }

// Push queues an element, or drops the subscriber if it has fallen too far
// behind. It never blocks: it is meant to be called from the same critical
// section that numbers the elements.
func (s *Sub[T]) Push(ev T) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	if seq := s.seqOf(ev); seq != 0 && seq <= s.skipTo {
		// Already covered by the history this subscriber is about to be given.
		s.mu.Unlock()
		return
	}
	if len(s.q)-s.headroom >= s.cap {
		// Stop accepting, but deliver what is already queued and mark where the
		// gap begins. Discarding the queue instead would leave the reader with a
		// hole below the point it is told to resume from.
		s.dead = true
		s.q = append(s.q, s.onDrop(s.lastSeq))
		s.mu.Unlock()
		s.signal()
		return
	}
	s.q = append(s.q, ev)
	if seq := s.seqOf(ev); seq > s.lastSeq {
		s.lastSeq = seq
	}
	s.mu.Unlock()
	s.signal()
}

// Prepend puts an already-fetched history in front of the queue and removes
// anything from the queue that the history covers, so a reader sees each
// element once and in order.
func (s *Sub[T]) Prepend(history []T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(history) == 0 {
		s.signal()
		return
	}
	last := s.seqOf(history[len(history)-1])
	kept := make([]T, 0, len(s.q)+len(history))
	kept = append(kept, history...)
	for _, ev := range s.q {
		if s.seqOf(ev) > last {
			kept = append(kept, ev)
		}
	}
	s.q = kept
	s.headroom = len(history)
	if n := len(kept); n > 0 && s.seqOf(kept[n-1]) > s.lastSeq {
		s.lastSeq = s.seqOf(kept[n-1])
	}
	s.signal()
}

func (s *Sub[T]) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run pumps the queue into the reader's channel until the subscriber is dropped
// or detached, then closes the channel.
func (s *Sub[T]) Run() {
	defer close(s.out)
	for {
		for {
			s.mu.Lock()
			if len(s.q) == 0 {
				dead := s.dead
				s.mu.Unlock()
				if dead {
					return
				}
				break
			}
			ev := s.q[0]
			s.q = s.q[1:]
			if s.headroom > 0 {
				s.headroom--
			}
			s.mu.Unlock()
			select {
			case s.out <- ev:
			case <-s.closing:
				return
			}
		}
		select {
		case <-s.wake:
		case <-s.closing:
			return
		}
	}
}

// Stop detaches the subscriber. It is safe to call more than once, and safe to
// call while the pump is blocked on a reader that stopped reading.
func (s *Sub[T]) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()
	s.once.Do(func() { close(s.closing) })
}
