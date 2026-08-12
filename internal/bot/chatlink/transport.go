// Package chatlink connects a bot to the fleet's conversation store.
//
// It is the seam the Mattermost transport used to occupy, and it is much
// smaller: there is no websocket to keep alive, no server to reconnect to, and
// no second authority to ask who may see what. A message reaches a bot because
// the store committed it and the bot is a participant.
package chatlink

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/chat"
)

// threadQuietPeriod is how long a thread must be quiet before an unaddressed
// burst wakes the bots in it.
//
// It exists because a multi-bot thread produces replies in bursts, and a bot
// that answered each one would turn one exchange into a cascade. Conversation
// with a person never waits it out: a message that names the bot, or one from
// the operator in a thread the bot is alone in, goes through immediately.
const threadQuietPeriod = 45 * time.Second

// queueLimit bounds the inbound backlog. The store publishes from the path that
// holds the single writer, so this can never block; past the limit the oldest
// pending update is dropped and said out loud, because a bot that silently
// stopped hearing its thread is the worse failure.
const queueLimit = 1024

// transcriptBudget bounds a rendered thread. It is the same order as the tool
// results the agent already clips, and for the same reason: the model has a
// context window, and half a conversation it cannot see is worse than a
// conversation it is told was trimmed.
const transcriptBudget = 20 << 10

// Transport is one bot's view of the store.
type Transport struct {
	store *chat.Store
	// self is this bot's actor id, which is both what it writes as and what it
	// must not react to.
	self string
	name string
	log  *slog.Logger

	events   chan bot.Inbound
	debounce *threadDebouncer

	mu      sync.Mutex
	queue   []bot.Inbound
	closed  bool
	wake    chan struct{}
	stopped chan struct{}
}

// Open connects a bot to the store. The returned transport registers itself as
// a consumer of committed writes, so it starts hearing about threads the moment
// it exists.
func Open(store *chat.Store, name string, log *slog.Logger) *Transport {
	t := &Transport{
		store:   store,
		self:    chat.BotActor(name),
		name:    name,
		log:     log,
		events:  make(chan bot.Inbound),
		wake:    make(chan struct{}, 1),
		stopped: make(chan struct{}),
	}
	t.debounce = newThreadDebouncer(threadQuietPeriod, t.fireThreadUpdate)
	store.AddPublisher(t.onFrames)
	go t.pump()
	return t
}

func (t *Transport) logger() *slog.Logger {
	if t.log == nil {
		return slog.Default()
	}
	return t.log
}

// Events is the stream the runtime serves.
func (t *Transport) Events() <-chan bot.Inbound { return t.events }

// Close stops the transport. The store outlives it: several bots share one.
func (t *Transport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	t.debounce.stop()
	close(t.stopped)
	return nil
}

// onFrames is what the store calls after every committed write. It must not
// block: it runs on the goroutine that just held the writer every bot is queued
// behind.
func (t *Transport) onFrames(frames []chat.Frame) {
	for _, f := range frames {
		if f.Stream != chat.StreamMessage || f.Message == nil {
			continue
		}
		if !visibleTo(f.To, t.self) {
			continue
		}
		switch classify(*f.Message, f.To, t.self) {
		case kindIgnore:
			// Nothing to do - including for this bot's own messages, which is
			// what stops two bots in a thread from answering each other forever.
		case kindMention:
			t.enqueue(bot.Inbound{
				Kind: "mention", Thread: bot.ThreadID(f.Message.Thread),
				Author: f.Message.Author, Text: f.Message.Body,
				AttachmentIDs: f.Message.Attachments, MessageSeq: f.Message.Seq,
			})
		case kindUpdate:
			t.debounce.note(bot.ThreadID(f.Message.Thread))
		}
	}
}

// fireThreadUpdate is what the debouncer calls once a thread has gone quiet.
func (t *Transport) fireThreadUpdate(thread bot.ThreadID) {
	t.enqueue(bot.Inbound{Kind: "thread_update", Thread: thread})
}

// enqueue queues an inbound for the runtime without blocking the writer.
func (t *Transport) enqueue(in bot.Inbound) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	if len(t.queue) >= queueLimit {
		// Drop the oldest thread update rather than a message addressed to this
		// bot: an update is a nudge to re-read a thread that is still there, and
		// a mention is work someone is waiting on.
		if i := firstOfKind(t.queue, "thread_update"); i >= 0 {
			t.queue = append(t.queue[:i], t.queue[i+1:]...)
			t.logger().Warn("inbound queue is full; dropped a thread update")
		} else {
			t.mu.Unlock()
			t.logger().Error("inbound queue is full of addressed messages; dropping one",
				"thread", in.Thread)
			return
		}
	}
	t.queue = append(t.queue, in)
	t.mu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func firstOfKind(q []bot.Inbound, kind string) int {
	for i, in := range q {
		if in.Kind == kind {
			return i
		}
	}
	return -1
}

// pump moves queued inbounds onto the events channel. It is a goroutine rather
// than a buffered channel so the store's publisher never waits on the runtime.
func (t *Transport) pump() {
	for {
		t.mu.Lock()
		if len(t.queue) == 0 {
			closed := t.closed
			t.mu.Unlock()
			if closed {
				close(t.events)
				return
			}
			select {
			case <-t.wake:
			case <-t.stopped:
				t.mu.Lock()
				empty := len(t.queue) == 0
				t.mu.Unlock()
				if empty {
					close(t.events)
					return
				}
			}
			continue
		}
		in := t.queue[0]
		t.queue = t.queue[1:]
		t.mu.Unlock()

		select {
		case t.events <- in:
		case <-t.stopped:
			close(t.events)
			return
		}
	}
}

// ---- writing ----

// Reply answers in the thread the turn was woken in.
func (t *Transport) Reply(thread bot.ThreadID, text string) error {
	return t.Say(context.Background(), thread, text, bot.SayOpts{})
}

// Say posts into a thread this bot is in.
func (t *Transport) Say(ctx context.Context, thread bot.ThreadID, text string, o bot.SayOpts) error {
	_, err := t.store.Say(ctx, string(thread), chat.Draft{
		Author: t.self, Body: text, Mentions: o.Mentions, AwaitReply: o.AwaitReply,
	})
	return err
}

// Open starts a conversation. Participants are the whole authorization
// boundary, so naming them is how the thread becomes visible at all.
func (t *Transport) Open(ctx context.Context, title string, participants []string, text string) (bot.ThreadID, error) {
	th, err := t.store.NewThread(ctx, title, t.self, participants)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) != "" {
		if _, err := t.store.Say(ctx, th.ID, chat.Draft{Author: t.self, Body: text}); err != nil {
			return bot.ThreadID(th.ID), err
		}
	}
	return bot.ThreadID(th.ID), nil
}

// Join pulls an actor into a thread this bot is in.
func (t *Transport) Join(ctx context.Context, thread bot.ThreadID, actor string) error {
	return t.store.AddParticipant(ctx, t.self, string(thread), actor)
}

// ActorFor maps a name to the actor id the store knows it by, or "" when there
// is no such actor. "operator" and "you" both name the human, because a model
// reaches for either.
func (t *Transport) ActorFor(name string) string {
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "@"))
	switch strings.ToLower(name) {
	case "":
		return ""
	case "operator", "you", "human":
		return chat.Operator
	}
	actors, err := t.store.Actors(context.Background())
	if err != nil {
		return ""
	}
	for _, a := range actors {
		if strings.EqualFold(a.Name, name) {
			return a.ID
		}
	}
	return ""
}

// ---- reading ----

// ThreadHistory renders a thread for the model, or "" when it cannot be read -
// a turn must not fail because its context could not be fetched.
func (t *Transport) ThreadHistory(ctx context.Context, thread bot.ThreadID) string {
	text, err := t.store.Transcript(ctx, t.self, string(thread), transcriptBudget)
	if err != nil {
		t.logger().Warn("could not read the thread", "thread", thread, "err", err)
		return ""
	}
	return text
}

// ThreadText is the same block as a tool result, where the error is something
// the model can act on.
func (t *Transport) ThreadText(ctx context.Context, thread bot.ThreadID) (string, error) {
	return t.store.Transcript(ctx, t.self, string(thread), transcriptBudget)
}

// Threads lists this bot's conversations.
func (t *Transport) Threads(ctx context.Context, state string, limit int) (string, error) {
	return t.store.Digest(ctx, t.self, state, limit)
}

// Search runs a full-text query over the messages in them.
func (t *Transport) Search(ctx context.Context, query string, limit int) (string, error) {
	hits, err := t.store.Search(ctx, t.self, query, limit)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "No messages matched.", nil
	}
	var b strings.Builder
	for _, m := range hits {
		who := t.store.AuthorName(ctx, m.Author)
		b.WriteString(m.Thread + "  " + who + ": " + oneLine(m.Body) + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// AuthorName resolves an actor id to a display name.
func (t *Transport) AuthorName(ctx context.Context, actorID string) string {
	return t.store.AuthorName(ctx, actorID)
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 160
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

func visibleTo(audience []string, actor string) bool {
	for _, a := range audience {
		if a == actor {
			return true
		}
	}
	return false
}

// Compile-time assertions that this transport satisfies the seam. They are here
// rather than implied because the optional interfaces are type-asserted at
// their use sites, where a missed method is a silently absent capability rather
// than a build failure.
var (
	_ bot.Transport         = (*Transport)(nil)
	_ bot.ThreadWriter      = (*Transport)(nil)
	_ bot.ThreadReader      = (*Transport)(nil)
	_ bot.AuthorNamer       = (*Transport)(nil)
	_ bot.AttachmentFetcher = (*Transport)(nil)
	_ bot.Journaller        = (*Transport)(nil)
)
