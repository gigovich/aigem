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
	subs map[uint64]*subscriber
	next uint64
}

type subscriber struct {
	actor string
	// watch is the one thread whose timeline this client wants. A fleet
	// mid-turn produces hundreds of events a minute across every thread, and
	// shipping all of them to a client showing a list is how the fan-out budget
	// goes.
	watch string
	out   *fanout.Sub[Frame]
}

func NewHub() *Hub { return &Hub{subs: map[uint64]*subscriber{}} }

// Attach adds a client and returns its stream, a handle for Watch, and a
// function to detach. backlog is history the client already asked for; it does
// not count against the queue bound.
func (h *Hub) Attach(actor string, backlog []Frame) (id uint64, out <-chan Frame, detach func()) {
	sub := &subscriber{
		actor: actor,
		out: fanout.New(fanout.Config[Frame]{
			QueueCap: queueCap,
			Backlog:  backlog,
			SeqOf:    func(f Frame) uint64 { return f.Seq },
			OnDrop: func(last uint64) Frame {
				return Frame{Stream: StreamDesync, From: last}
			},
		}),
	}
	h.mu.Lock()
	h.next++
	id = h.next
	h.subs[id] = sub
	h.mu.Unlock()

	go sub.out.Run()
	var once sync.Once
	return id, sub.out.Out(), func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, id)
			h.mu.Unlock()
			sub.out.Stop()
		})
	}
}

// Watch points a client's timeline at one thread, or at none when thread is
// empty. Events for anything else are not sent to it.
func (h *Hub) Watch(id uint64, thread string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sub, ok := h.subs[id]; ok {
		sub.watch = thread
	}
}

// Publish delivers a committed write's frames. It never blocks: a subscriber
// that cannot keep up is dropped by its own queue rather than allowed to stall
// the writer every bot is queued behind.
func (h *Hub) Publish(frames []Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range frames {
		for _, sub := range h.subs {
			if !f.visibleTo(sub.actor) {
				continue
			}
			if f.Stream == StreamEvent && sub.watch != f.ThreadID {
				continue
			}
			sub.out.Push(f)
		}
	}
}

// Attached is how many clients are following, for the fleet screen and for
// tests.
func (h *Hub) Attached() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
