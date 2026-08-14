// Package chatpush turns one conversation event into a notification on a
// phone: the moment a thread starts asking the operator for something.
//
// It is a package of its own because it is the only thing that knows both
// halves. The store knows what happened and nothing about delivery; the push
// package knows how to deliver and nothing about threads. Joining them here
// keeps the rule that decides when a person is worth interrupting in one
// readable place.
package chatpush

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/chat"
	"github.com/gigovich/aigem/internal/push"
)

// Notifier watches committed writes and pushes the transitions into needs_you.
//
// Only the transition. A bot finishing a turn, posting an update or starting
// work is not a reason to light up a phone, and a thread that is already asking
// does not ask twice.
type Notifier struct {
	store  *chat.Store
	client *push.Client
	log    *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	// wg tracks deliveries in flight, so Close returns once nothing is still
	// talking to a push service. Add is called under mu, and Close sets closed
	// under the same lock before it waits - the store calls its publishers
	// outside its own lock, so a write already in flight can reach onFrames
	// after the publisher has been removed, and Add racing Wait from zero is a
	// panic rather than a late delivery.
	wg sync.WaitGroup
	// sending bounds how many push services are being talked to at once. Each
	// delivery can take the request timeout, and a fleet that turns several
	// threads over to the operator at once should not open a socket per thread
	// per subscription.
	sending chan struct{}

	mu sync.Mutex
	// asking is whether each thread is currently waiting on the operator. A
	// transition needs a before as well as an after, and a frame carries only
	// the after.
	//
	// It is kept rather than read off the frame because the state a frame
	// carries is the effective one: while any turn is open in a thread it reads
	// "working" and hides whatever the thread is actually parked in. What moves
	// the resting state is a message, so that is what this follows.
	asking map[string]bool
	closed bool
	remove func()
}

// maxSending is the ceiling on concurrent deliveries.
const maxSending = 4

// seedLimit is how many already-asking threads a start reads. It is the most
// the store will return for one query, and a fleet with more than five hundred
// unanswered questions has a problem that a notification cannot help with; the
// overflow is announced again the next time it changes.
const seedLimit = 500

func New(store *chat.Store, client *push.Client, log *slog.Logger) *Notifier {
	return &Notifier{
		store: store, client: client, log: log,
		asking:  map[string]bool{},
		sending: make(chan struct{}, maxSending),
	}
}

// Start seeds what is already asking and then follows every write.
//
// Seeding comes first on purpose. Without it, the first frame after a restart
// for a thread that was already in needs_you reads as a transition, and the
// operator is interrupted again for something they already know about.
//
// The two steps are not atomic, and nothing here makes them so: a transition
// committing between the read and the registration is missed by both, and is
// announced only when that thread next changes. The daemon starts this before
// it serves a request or starts a bot, so nothing can write in the gap.
//
// The seed reads the effective state, so a thread with a turn still open reads
// "working" and is not seeded. The daemon closes the turns a previous run left
// open before it gets here, which is what makes that empty rather than merely
// unlikely; the cost of being wrong is one repeated notification.
func (n *Notifier) Start(ctx context.Context) error {
	views, err := n.store.Inbox(ctx, chat.Operator, chat.StateNeedsYou, false, seedLimit)
	if err != nil {
		return err
	}
	n.ctx, n.cancel = context.WithCancel(context.WithoutCancel(ctx))
	n.mu.Lock()
	for _, v := range views {
		n.asking[v.ID] = true
	}
	n.mu.Unlock()
	n.remove = n.store.AddPublisher("push", n.onFrames)
	return nil
}

// closeGrace is how long shutdown waits for deliveries already in flight.
//
// It is not zero because a dropped push is dropped for good: the thread stays
// in needs_you, and the next run seeds that state as already known and so never
// announces it. It is not unbounded because a push service that accepts a
// connection and then says nothing must not hold the fleet's shutdown open for
// the request timeout.
const closeGrace = 2 * time.Second

// Close stops following and gives whatever is in flight a moment to land. Past
// that it cancels them, and then waits for them to unwind - so a delivery never
// outlives this call, and a push service that says nothing cannot hold up the
// fleet's shutdown for longer than the grace.
func (n *Notifier) Close() {
	// A daemon whose keys would not load has no notifier, and its shutdown path
	// is the same one as everybody else's.
	if n == nil {
		return
	}
	if n.remove != nil {
		n.remove()
	}
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()
	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeGrace):
	}
	if n.cancel != nil {
		n.cancel()
	}
	<-done
}

// alert is one thread's worth of notification.
type alert struct {
	Thread string `json:"thread"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	// URL is the screen to open, so the service worker does not have to know
	// how this application spells a thread.
	URL string `json:"url"`
}

// onFrames runs on the store's write path and must not block it: everything
// here is a map lookup, and the delivery it decides on happens elsewhere.
func (n *Notifier) onFrames(frames []chat.Frame) {
	var alerts []alert
	n.mu.Lock()
	if n.closed {
		// Shutting down. The store calls its publishers outside its own lock, so
		// this can be entered after Close removed the registration.
		n.mu.Unlock()
		return
	}
	// The thread each message left behind, which is what carries the audience,
	// the title and whether the operator has put the conversation away. Every
	// write that says something publishes both frames, message first.
	views := map[string]*chat.ThreadView{}
	for _, f := range frames {
		if f.Stream == chat.StreamThread {
			if f.Thread == nil {
				// A tombstone: the thread is gone, and so is any reason to
				// remember what it was doing.
				delete(n.asking, f.ThreadID)
				continue
			}
			views[f.ThreadID] = f.Thread
		}
	}
	for _, f := range frames {
		if f.Stream != chat.StreamMessage || f.Message == nil {
			continue
		}
		m := f.Message
		// The store's own rule, not a second copy of it: what a message leaves a
		// thread in is exactly what decides whether the operator is now owed an
		// answer, and two implementations of that would drift.
		//
		// The operator is the only human, so their being in the thread is what
		// "someone could answer" means. Without a thread frame there is nothing
		// to say who is in it - which cannot happen for a message, since the
		// write that says something publishes both.
		v, ok := views[f.ThreadID]
		switch chat.MessageState(*m, ok && slices.Contains(v.Participants, chat.Operator)) {
		case chat.StateWaiting:
			// A person spoke. Whatever was owed is owed no longer, and this is
			// recorded even for a thread whose frame did not arrive: believing a
			// thread is still asking is what silently swallows its next question.
			n.asking[f.ThreadID] = false
		case chat.StateNeedsYou:
			// An archived thread is one the operator has put away: it does not
			// interrupt them, and it is deliberately absent from the seed, so
			// tracking it here would make the two halves disagree about what a
			// restart already knows.
			if v.Archived || n.asking[f.ThreadID] {
				continue
			}
			n.asking[f.ThreadID] = true
			alerts = append(alerts, alertFor(*v, m.Author))
		}
	}
	// Under the lock, so Close cannot begin waiting between the check above and
	// the accounting for what this call is about to start.
	n.wg.Add(len(alerts))
	n.mu.Unlock()

	for _, a := range alerts {
		go func() {
			defer n.wg.Done()
			n.sending <- struct{}{}
			defer func() { <-n.sending }()
			n.deliver(a)
		}()
	}
}

func alertFor(v chat.ThreadView, author string) alert {
	title := strings.TrimSpace(v.Title)
	if title == "" {
		title = "aigem"
	}
	body := "needs you"
	// The bot that asked, taken from the message rather than from the thread's
	// last author: by the time several bots are working in one thread, the last
	// word and the question are not always the same one.
	if kind, name := chat.ActorName(author); kind == chat.KindBot && name != "" {
		body = name + " needs you"
	}
	// What was said is deliberately not in here. A notification is read off a
	// locked screen, and the thread is one tap away for whoever can unlock it.
	return alert{Thread: v.ID, Title: title, Body: body, URL: "/chat/" + v.ID}
}

// deliver sends one alert to every subscription, and forgets the ones a push
// service says no longer exist.
func (n *Notifier) deliver(a alert) {
	subs, err := n.store.PushSubs(n.ctx)
	if err != nil {
		n.log.Warn("reading push subscriptions failed", "err", err)
		return
	}
	if len(subs) == 0 {
		return
	}
	payload, err := json.Marshal(a)
	if err != nil {
		n.log.Warn("encoding a notification failed", "err", err)
		return
	}
	for _, sub := range subs {
		err := n.client.Send(n.ctx, sub, push.Message{Payload: payload, Topic: topicFor(a.Thread)})
		switch {
		case err == nil:
		case errors.Is(err, push.ErrGone):
			// The browser is gone, not merely unreachable. Keeping the row would
			// mean pushing to it on every alert forever.
			if err := n.store.DeletePushSub(n.ctx, sub.Endpoint); err != nil {
				n.log.Warn("forgetting a dead push subscription failed", "err", err)
			}
		case n.ctx.Err() != nil:
			// Shutting down. The alert is still in the store, which is where the
			// inbox reads it from.
			return
		default:
			n.log.Warn("push failed", "thread", a.Thread, "err", err)
		}
	}
}

// topicFor lets a second alert about one thread replace a first that has not
// been delivered yet, so a phone that was off does not wake to the same thread
// twice.
//
// A thread id is "t_" and hex, which is already what RFC 8030 allows a topic to
// be. This checks anyway rather than trusting that: a topic the service rejects
// would fail the whole delivery, and losing the deduplication is the cheaper
// failure by far.
func topicFor(thread string) string {
	if !push.ValidTopic(thread) {
		return ""
	}
	return thread
}
