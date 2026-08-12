package uisession

import "github.com/gigovich/aigem/internal/fanout"

// subscriber is the shared fan-out plus the client it belongs to. The fan-out
// knows nothing about who is reading; presence does, so the identity lives here
// rather than in internal/fanout.
type subscriber struct {
	*fanout.Sub[Event]
	client Client
}

// newSubscriber builds a subscriber. queueCap bounds how far behind the live
// stream it may fall; the backlog it starts with does not count against that.
// skipTo is the highest sequence the client already has, for a subscriber
// registered before its history was fetched.
func newSubscriber(c Client, queueCap int, backlog []Event, skipTo uint64) *subscriber {
	return &subscriber{
		Sub: fanout.New(fanout.Config[Event]{
			QueueCap: queueCap,
			Backlog:  backlog,
			SkipTo:   skipTo,
			SeqOf:    func(ev Event) uint64 { return ev.Seq },
			OnDrop:   func(last uint64) Event { return Event{Kind: KindDesync, From: last} },
		}),
		client: c,
	}
}

// stop is nil-safe, because replacing a client id stops whatever was there
// before and there is usually nothing. The promoted Stop cannot do this itself:
// reaching it on a nil *subscriber dereferences the embedded pointer first.
func (s *subscriber) stop() {
	if s == nil {
		return
	}
	s.Stop()
}
