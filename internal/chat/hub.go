package chat

import (
	"sync"

	"github.com/gigovich/aigem/internal/fanout"
)

// queueCap bounds how far behind the live stream one client may fall before it
// is dropped with a resume point. It is generous because the alternative to
// falling behind is being told to reload: a phone on a bad connection should
// have to be very slow, not merely slow, before it loses its place.
const queueCap = 512

// Hub fans committed frames out to whoever is attached.
//
// It decides nothing about who may see what. Entitlement travels on the frame,
// filled in by the transaction that wrote it, because the alternative is asking
// the database once per frame per subscriber on a path that holds the writer.
type Hub struct {
	mu   sync.Mutex
	subs map[*Client]struct{}
}

// Client is one attached reader. It is a handle rather than an id because the
// only thing an id bought was a map lookup that silently did nothing when the
// client had already detached.
type Client struct {
	hub   *Hub
	actor string
	out   *fanout.Sub[Frame]
	once  sync.Once

	// watch is the one thread whose timeline this client wants. A fleet
	// mid-turn produces hundreds of events a minute across every thread, and
	// shipping all of them to a client showing a list is how the fan-out budget
	// goes. It is guarded by the hub's lock, which Publish already holds.
	watch string
}

func NewHub() *Hub { return &Hub{subs: map[*Client]struct{}{}} }

// Attach adds a client and gives it everything after since.
//
// The hub fetches the backlog rather than taking one, because only doing it in
// this order closes the window: the client is registered first, with everything
// at or below since suppressed, and the history is then spliced in front of
// whatever arrived while it was being read. Fetching first and registering
// after would lose any write that committed in between - which for a SQLite
// read is a real interval, not a theoretical one.
func (h *Hub) Attach(actor string, since uint64, backlog func(uint64) ([]Frame, error)) (*Client, error) {
	c := &Client{
		hub:   h,
		actor: actor,
		out: fanout.New(fanout.Config[Frame]{
			QueueCap: queueCap,
			SkipTo:   since,
			SeqOf:    func(f Frame) uint64 { return f.Seq },
			OnDrop: func(last uint64) Frame {
				return Frame{Seq: last, Stream: StreamDesync, From: last}
			},
		}),
	}
	h.mu.Lock()
	h.subs[c] = struct{}{}
	h.mu.Unlock()

	if backlog != nil {
		history, err := backlog(since)
		if err != nil {
			c.Detach()
			return nil, err
		}
		c.out.Prepend(history)
	}
	go c.out.Run()
	return c, nil
}

// Frames is what the client reads. It closes when the client detaches or is
// dropped.
func (c *Client) Frames() <-chan Frame { return c.out.Out() }

// Watch points the client's timeline at one thread, or at none when thread is
// empty. Events for anything else are not sent to it.
func (c *Client) Watch(thread string) {
	c.hub.mu.Lock()
	defer c.hub.mu.Unlock()
	c.watch = thread
}

// Detach is safe to call more than once, and from either end of the connection.
func (c *Client) Detach() {
	c.once.Do(func() {
		c.hub.mu.Lock()
		delete(c.hub.subs, c)
		c.hub.mu.Unlock()
		c.out.Stop()
	})
}

// Publish delivers a committed write's frames. It never blocks: a subscriber
// that cannot keep up is dropped by its own queue rather than allowed to stall
// the writer every bot is queued behind.
func (h *Hub) Publish(frames []Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range frames {
		for c := range h.subs {
			if !f.visibleTo(c.actor) {
				continue
			}
			if f.Stream == StreamEvent && c.watch != f.ThreadID {
				continue
			}
			c.out.Push(f)
		}
	}
}

// Close detaches every client, which closes their streams and ends the
// goroutines pumping them.
//
// http.Server.Close does not touch a hijacked connection, so without this every
// attached socket outlives the daemon - and once the store is closed behind it,
// answers every op with "database is closed" while staying connected.
func (h *Hub) Close() {
	h.mu.Lock()
	subs := make([]*Client, 0, len(h.subs))
	for c := range h.subs {
		subs = append(subs, c)
	}
	h.mu.Unlock()
	for _, c := range subs {
		c.Detach()
	}
}

// Attached is how many clients are following.
func (h *Hub) Attached() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
