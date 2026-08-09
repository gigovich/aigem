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

	cap     int
	wake    chan struct{}
	closing chan struct{}
	once    sync.Once
}

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
	if len(s.q) >= s.cap {
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
	s.lastSeq = ev.Seq
	s.mu.Unlock()
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
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()
	s.once.Do(func() { close(s.closing) })
}
