package chatlink

import (
	"context"
	"encoding/json"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/chat"
	"github.com/gigovich/aigem/internal/uisession"
)

// TurnEvents opens a turn and returns the sink that records its steps.
//
// This is what puts a bot's work in front of the operator. Until now those
// steps reached only the log: the model's answer was the whole of what a human
// could see, which is all Mattermost was ever able to show.
//
// The turn row exists for the whole run, so "amiran is working" is a fact
// anyone can read rather than a ping that keeps having to be re-sent - and one
// that would still be claiming to be true some seconds after the process died.
func (t *Transport) TurnEvents(thread bot.ThreadID, _ string) (agent.Events, func(string, error)) {
	ctx := context.Background()
	turn, err := t.store.BeginTurn(ctx, string(thread), t.self)
	if err != nil {
		// A turn whose timeline cannot be recorded still runs. Losing the trace
		// is worse than nothing, but far better than refusing to work.
		t.logger().Warn("could not open a turn", "thread", thread, "err", err)
		return agent.Events{}, func(string, error) {}
	}
	emit := func(ev uisession.Event) {
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
	done := func(answer string, runErr error) {
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
	return uisession.Bridge(emit), done
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
