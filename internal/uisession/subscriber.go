package uisession

import "sync"

// subscriber is one attached front-end's view of the stream. Events are queued
// here rather than pushed straight into the client's channel, because the
// alternative is a fan-out that runs at the speed of its slowest reader: a
// phone on a bad connection would stall the agent for everyone.
//
// The queue is bounded. A subscriber that falls further behind than the cap is
// dropped with a desync event carrying the last sequence number it definitely
// received, which is all a client needs to reconnect and catch up.
type subscriber struct {
	client Client
	out    chan Event

	mu      sync.Mutex
	q       []Event
	dead    bool
	lastSeq uint64

	// skipTo is the highest sequence number the client is known to have. It
	// exists for a subscriber registered before its history was fetched: events
	// that arrive in that window are kept, and the ones the history will also
	// carry are dropped when it is spliced in.
	skipTo uint64
	// headroom is how much of the queue is backlog rather than live tail. See
	// newSubscriber: history the client asked for must not count as falling
	// behind.
	headroom int
	cap      int
	wake     chan struct{}
	closing  chan struct{}
	once     sync.Once
}

// newSubscriber builds a subscriber. queueCap bounds how far behind the live
// stream it may fall; the backlog it starts with does not count against that.
// The backlog is history the client asked for and is already draining - a
// conversation longer than the cap would otherwise be dropped for being long
// rather than for being slow, which is the opposite of what the cap is for.
func newSubscriber(c Client, queueCap int, backlog []Event) *subscriber {
	s := &subscriber{
		client:  c,
		out:     make(chan Event),
		q:       backlog,
		cap:     queueCap,
		wake:    make(chan struct{}, 1),
		closing: make(chan struct{}),
	}
	if n := len(backlog); n > 0 {
		s.lastSeq = backlog[n-1].Seq
		s.headroom = n
		s.signal()
	}
	return s
}

// push queues an event, or drops the subscriber if it has fallen too far
// behind. It never blocks: it is called with the session lock held, from the
// same critical section that numbers the events.
func (s *subscriber) push(ev Event) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	if ev.Seq != 0 && ev.Seq <= s.skipTo {
		// Already covered by the history this subscriber is about to be given.
		s.mu.Unlock()
		return
	}
	if len(s.q)-s.headroom >= s.cap {
		// Stop accepting, but deliver what is already queued and mark where the
		// gap begins. Discarding the queue instead would leave the client with a
		// hole below the point it is told to resume from.
		s.dead = true
		s.q = append(s.q, Event{Kind: KindDesync, From: s.lastSeq})
		s.mu.Unlock()
		s.signal()
		return
	}
	s.q = append(s.q, ev)
	if ev.Seq > s.lastSeq {
		s.lastSeq = ev.Seq
	}
	s.mu.Unlock()
	s.signal()
}

// prepend puts an already-fetched history in front of the queue and removes
// anything from the queue that the history covers, so a client sees each event
// once and in order.
func (s *subscriber) prepend(history []Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(history) == 0 {
		s.signal()
		return
	}
	last := history[len(history)-1].Seq
	kept := make([]Event, 0, len(s.q)+len(history))
	kept = append(kept, history...)
	for _, ev := range s.q {
		if ev.Seq > last {
			kept = append(kept, ev)
		}
	}
	s.q = kept
	s.headroom = len(history)
	if n := len(kept); n > 0 && kept[n-1].Seq > s.lastSeq {
		s.lastSeq = kept[n-1].Seq
	}
	s.signal()
}

func (s *subscriber) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run pumps the queue into the client's channel until the subscriber is dropped
// or detached, then closes the channel so the client's range loop ends.
func (s *subscriber) run() {
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

// stop detaches the subscriber. It is safe to call more than once, and safe to
// call while the pump is blocked on a client that stopped reading.
func (s *subscriber) stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()
	s.once.Do(func() { close(s.closing) })
}
