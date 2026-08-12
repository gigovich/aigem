package chatlink

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/chat"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/uisession"
)

// usageFlushEvery bounds how far a turn's recorded spend lags what it has
// actually spent.
//
// The sink fires on the goroutine that made the model call, with the provider's
// response still open, so a write there is paid for by a connection that is not
// yet back in the pool - and it queues behind the single writer the whole fleet
// shares. Per call it bought nothing: AddUsage publishes no frame, so no reader
// is watching the number climb. Batching keeps the write off that path without
// letting a long turn's row sit at zero for the whole run.
//
// The cost is that a process killed mid-turn loses up to this many calls of
// spend rather than at most one. That turn is stamped "interrupted by a
// restart" at the next startup, so its number was never one to bill against.
const usageFlushEvery = 16

// TurnEvents opens a turn and returns the sinks that record its steps and its
// spend.
//
// This is what puts a bot's work in front of the operator. Until now those
// steps reached only the log: the model's answer was the whole of what a human
// could see, which is all Mattermost was ever able to show.
//
// The turn row exists for the whole run, so "amiran is working" is a fact
// anyone can read rather than a ping that keeps having to be re-sent - and one
// that would still be claiming to be true some seconds after the process died.
func (t *Transport) TurnEvents(thread bot.ThreadID, _ string) (agent.Events, bot.UsageSink,
	func(string, error)) {
	ctx := context.Background()
	turn, err := t.store.BeginTurn(ctx, string(thread), t.self)
	if err != nil {
		// A turn whose timeline cannot be recorded still runs. Losing the trace
		// is worse than nothing, but far better than refusing to work.
		t.logger().Warn("could not open a turn", "thread", thread, "err", err)
		return agent.Events{}, nil, func(string, error) {}
	}
	emit := func(ev uisession.Event) {
		if skipInTimeline(ev.Kind) {
			return
		}
		payload, blob := split(ev)
		body, err := json.Marshal(payload)
		if err != nil {
			return
		}
		if _, err := t.store.AppendEvent(ctx, chat.EventRecord{
			Thread: string(thread), Actor: t.self, TurnSeq: turn,
			Kind: string(ev.Kind), Payload: body, Blob: blob,
		}); err != nil {
			t.logger().Warn("could not record a turn step", "kind", ev.Kind, "err", err)
		}
	}
	// The turn's spend, accumulated between flushes. Parallel subagents run on
	// the same client, so several calls finish at once.
	var (
		mu      sync.Mutex
		pending chat.Usage
		model   string
	)
	flush := func() {
		mu.Lock()
		u, m := pending, model
		pending = chat.Usage{}
		mu.Unlock()
		if u.IsZero() {
			return
		}
		if err := t.store.AddUsage(ctx, t.self, turn, u, m); err != nil {
			t.logger().Warn("could not record what the turn cost", "thread", thread, "err", err)
		}
	}
	spend := func(u llm.Usage, m string) {
		call := chat.Usage{
			InputTokens:  u.InputTokens,
			CachedTokens: u.CachedTokens,
			OutputTokens: u.OutputTokens,
			Calls:        1,
		}
		if u.IsZero() {
			call.Uncounted = 1
		}
		mu.Lock()
		pending = pending.Add(call)
		if m != "" {
			model = m
		}
		due := pending.Calls >= usageFlushEvery
		mu.Unlock()
		if due {
			flush()
		}
	}
	done := func(answer string, runErr error) {
		flush()
		emit(uisession.Event{Kind: uisession.KindTurnEnd, Text: answer})
		msg := ""
		if runErr != nil {
			msg = runErr.Error()
		}
		if err := t.store.EndTurn(ctx, t.self, turn, msg); err != nil {
			t.logger().Warn("could not close a turn", "thread", thread, "err", err)
		}
	}
	emit(uisession.Event{Kind: uisession.KindTurnStart})
	return uisession.Bridge(emit), spend, done
}

// skipInTimeline drops the per-delta events.
//
// uisession.Bridge fires OnContent and OnReasoning once per streamed chunk, and
// its journal is a buffered file that can take that. Here every event is a
// transaction on the single writer the whole fleet queues behind, plus a
// thread frame and a fan-out to every subscriber - so one turn's streaming
// would stall every other bot's message.
//
// Nothing is lost that a reader needs: the assistant's finished text arrives as
// assistant_message, and the answer arrives again on turn_end. What the browser
// gives up is the token-by-token caret, which a turn nobody is watching live
// does not need.
func skipInTimeline(kind uisession.Kind) bool {
	return kind == uisession.KindContent || kind == uisession.KindReasoning
}

// split prepares an event for storage: an oversized tool result is replaced by
// its head and the whole body is stored beside it, to be fetched when someone
// expands the call.
//
// The threshold is the store's, and the policy is the same one uisession
// applies to a session's own journal. It is duplicated rather than shared
// because deciding what may be trimmed needs to know what the event means -
// which field is the result and which are the render flags - and that knowledge
// belongs with whoever produces the event, not with the store.
func split(ev uisession.Event) (uisession.Event, []byte) {
	if ev.Kind != uisession.KindToolEnd && ev.Kind != uisession.KindSubToolEnd {
		return ev, nil
	}
	if len(ev.Text) <= chat.BlobThreshold {
		return ev, nil
	}
	blob := []byte(ev.Text)
	ev.Bytes = len(ev.Text)
	ev.Blob = true
	ev.Text = ev.Text[:chat.BlobThreshold]
	return ev, blob
}
