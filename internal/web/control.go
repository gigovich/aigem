package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
)

// The control stream: one socket per tab, read-only, carrying the deltas that
// tell a page what changed somewhere else in the daemon.
//
// It never replays. A client that reconnects gets a fresh base in hello and
// asks for whatever collection it cares about over HTTP, which is also what it
// does when it notices it has missed a message. That is the whole point of rev:
// every published mutation gets the next number, so a client that sees the
// number jump knows it has a hole without the daemon having to remember what
// was in it.
//
// The rule a client applies is "rev greater than the last one plus one means a
// gap". Not "not equal to": a frame that is not a mutation carries the
// revision the client is already at, and reading that as a gap would make
// every mistake cost a refetch.
//
// Mutations never come up this socket. They go over HTTP, where a status code
// and an error body mean something, where httptest can drive every one of them,
// and where this file stays a single switch on an op name.

const (
	// controlBuffer is how far behind a client may fall before the hub gives up
	// on it. The unit is messages rather than bytes because what goes down this
	// stream is deltas - an id and a status, a set of counts - while anything
	// large is a run event on its own socket or a collection the client fetches.
	controlBuffer = 32

	controlHello       = "hello"
	controlClientError = "client_error"
)

// controlMessage is every frame this stream sends. The rev is on the envelope
// rather than in the payload so that a client can check for a gap without
// knowing what kind of message it is looking at.
type controlMessage struct {
	Type string `json:"type"`
	Rev  uint64 `json:"rev"`
	Data any    `json:"data,omitempty"`
}

// clientError is the payload of the one frame that is not a state delta: an op
// this stream will not carry out.
type clientError struct {
	Op    string `json:"op,omitempty"`
	Error string `json:"error"`
}

// hub fans published mutations out to the connected control sockets and owns
// the daemon's revision counter.
//
// One counter for the whole daemon, not one per collection: a client tracks a
// single number, and the cost of a coarse counter is that an unrelated mutation
// can make a page refetch something that did not change - which is a spare
// request, against a per-collection scheme where a client has to know the full
// set of collections before it can tell a gap from a message it does not care
// about.
type hub struct {
	mu   sync.Mutex
	rev  uint64
	subs map[*controlSub]struct{}
}

// controlDelta is one published mutation on its way to one client: the frame,
// and the revision in it. The revision travels alongside the bytes because what
// a client has been *sent* is not what the daemon is *at* - the queue below
// holds the difference - and a frame that is not a mutation has to carry the
// former. See controlError.
type controlDelta struct {
	rev   uint64
	frame []byte
}

// controlSub is one connected client. The channel is buffered and the hub never
// blocks on it: a client that fills it is dropped instead, see publish.
type controlSub struct {
	msgs chan controlDelta
	// behind is closed once, by the hub, when it has had to skip a message for
	// this client. Its socket ends and the client re-bases on the hello of its
	// next connection.
	behind chan struct{}
	// skipped guards that close, and is read and written under the hub's lock.
	skipped bool
}

func newHub() *hub { return &hub{subs: make(map[*controlSub]struct{})} }

// subscribe attaches a client and reports the revision its state must be at
// least as new as. The two happen under one lock, so a mutation published in
// between is delivered on the subscription rather than lost in the gap between
// the number the client was told and the moment it started listening.
func (h *hub) subscribe() (*controlSub, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sub := &controlSub{msgs: make(chan controlDelta, controlBuffer), behind: make(chan struct{})}
	h.subs[sub] = struct{}{}
	return sub, h.rev
}

func (h *hub) unsubscribe(sub *controlSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, sub)
}

// current reports the revision the daemon is at without publishing anything.
//
// A snapshot served over HTTP is stamped with what this returns, read before
// the snapshot is taken: a base labelled older than it is costs a refetch, and
// a base labelled newer than it is is a page that is silently stale with no gap
// to notice.
func (h *hub) current() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rev
}

// publish stamps a mutation with the next revision and hands it to every
// connected client. It returns the revision it assigned.
//
// The payload is encoded before the lock is taken - it is the expensive part,
// and holding a daemon-wide lock across it would make every publisher and every
// page that connects or disconnects wait on the encoding of a message none of
// them care about. The revision and the fan-out are under the lock together, so
// the order each client sees is the order the revisions were assigned in.
//
// Nothing here blocks. A client whose queue is full is dropped rather than
// waited for: it has stopped reading a stream that carries no history, so
// ending its socket and letting it re-base on its next hello tells it more than
// any number of messages it will never read. Skipping messages and leaving the
// socket open would not: the skip is only ever noticed by a message that comes
// after it, and a burst that fills a queue is exactly the burst that ends.
func (h *hub) publish(kind string, data any) uint64 {
	// The name is encoded rather than quoted: strconv writes Go string syntax,
	// which is not JSON for a control byte or a lone surrogate, and a caller
	// deriving a name from anything but a literal would find that out on the
	// wire. Encoding a string cannot fail - invalid UTF-8 is replaced rather
	// than refused - so there is no case here, only an obligation.
	name, _ := json.Marshal(kind)
	payload, err := json.Marshal(data)
	if err != nil {
		// The mutation happened, so the revision is spent either way. What goes
		// out is the envelope alone: a client is told the revision moved and
		// refetches, which is the same recovery as a gap and leaves nothing
		// stale.
		slog.Error("a control payload could not be encoded; clients will refetch on it",
			"type", kind, "err", err)
		payload = nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.rev++
	rev := h.rev
	d := controlDelta{rev: rev, frame: appendControlMessage(name, rev, payload)}
	for sub := range h.subs {
		select {
		case sub.msgs <- d:
		default:
			if !sub.skipped {
				sub.skipped = true
				close(sub.behind)
			}
		}
	}
	return rev
}

// appendControlMessage builds the frame by hand. The name and the payload are
// already encoded, and handing them back to encoding/json would copy and
// re-scan every byte while the hub's lock is held.
func appendControlMessage(name []byte, rev uint64, payload []byte) []byte {
	b := make([]byte, 0, len(payload)+len(name)+48)
	b = append(b, `{"type":`...)
	b = append(b, name...)
	b = append(b, `,"rev":`...)
	b = strconv.AppendUint(b, rev, 10)
	if len(payload) > 0 && string(payload) != "null" {
		b = append(b, `,"data":`...)
		b = append(b, payload...)
	}
	return append(b, '}')
}

// handleControlSocket attaches one page to the daemon for as long as it stays
// connected.
//
// It is mounted through Guard like every other API route, so it authenticates
// by cookie as well as by token - which is what lets a browser open one without
// the token in the URL - and so a client that reconnects forever cannot hold
// more sockets than the daemon has agreed to.
func (s *Server) handleControlSocket(w http.ResponseWriter, r *http.Request) {
	// A request that is not a handshake is refused here rather than by the
	// upgrade below. gobwas hijacks before it validates, and writes its own
	// refusal straight onto the connection - past the wrapper that puts the
	// security headers on every other response, and past the socket cap, which
	// Guard charges only to a request that looks like a handshake. A signed-in
	// browser following this URL is exactly that request.
	if !isUpgrade(r) {
		http.Error(w, "this route is a websocket", http.StatusBadRequest)
		return
	}

	// Subscribed first and the base read after, so that hello's state is never
	// older than the revision it claims: see hub.current.
	//
	// Subscribing is not the hijack, so a backend that cannot answer is still a
	// status code rather than a socket that opens and says nothing.
	sub, rev := s.hub.subscribe()
	// Armed before anything that can unwind past it. The backend is an
	// interface implemented outside this package, so a panic in it is a live
	// possibility, and net/http recovering one would leave a subscription the
	// hub writes to for the life of the process.
	defer s.hub.unsubscribe(sub)
	meta, err := s.backend.Meta(r.Context())
	if err != nil {
		http.Error(w, "the daemon could not describe itself", http.StatusInternalServerError)
		return
	}

	c, ok := s.upgrade(w, r)
	if !ok {
		return
	}
	// Closed as well as unregistered: the registry is the only other thing that
	// could ever close it, so unwinding past here without both would leave the
	// connection and the goroutine behind it for the life of the process.
	defer func() {
		c.close()
		s.conns.remove(c)
	}()

	if err := c.send(controlMessage{Type: controlHello, Rev: rev, Data: s.metaBody(rev, meta)}); err != nil {
		return
	}

	// delivered is the last revision written on this connection, which is not
	// the one the daemon is at: publish only queues, so the hub can be a whole
	// buffer ahead of what this socket has actually sent. A frame that is not a
	// mutation carries this rather than that, or the answer to a client's own
	// mistake would arrive stamped past the deltas still queued behind it - and
	// the client would refetch, then apply those older deltas over the newer
	// snapshot it had just been given.
	var delivered atomic.Uint64
	delivered.Store(rev)
	send := func(d controlDelta) error {
		if err := c.sendBytes(d.frame); err != nil {
			return err
		}
		delivered.Store(d.rev)
		return nil
	}

	// Either end can finish first, and each has to unblock the other. A client
	// that disconnects ends the reader, and a daemon shutting down closes the
	// connection under both.
	done := make(chan struct{})
	go func() {
		defer close(done)
		pump(c, sub.msgs, send, sub.behind, wsPingInterval)
		c.close()
	}()
	c.readClientOps(func(data []byte) any { return controlOp(data, delivered.Load()) })
	c.close()
	<-done
}

// controlOp is the whole client vocabulary of this socket. Ping is here so that
// a client behind a proxy with an idle timeout has something to send;
// everything else a page wants to do is an HTTP request.
func controlOp(data []byte, rev uint64) any {
	var in struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return controlError("", "bad message: "+err.Error(), rev)
	}
	if in.Op == "ping" {
		return nil
	}
	return controlError(in.Op, fmt.Sprintf(
		"the control stream is read-only and does not take %q; send mutations over HTTP",
		in.Op), rev)
}

// controlError carries the revision this connection is already at rather than a
// new one: nothing happened, and the gap rule then reads it as exactly that.
func controlError(op, msg string, rev uint64) controlMessage {
	return controlMessage{Type: controlClientError, Rev: rev, Data: clientError{Op: op, Error: msg}}
}
