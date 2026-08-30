package uisession

import (
	"testing"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/tools"
)

// A fresh conversation must not open with the previous one's transcript. The
// retained history is what beginLocked writes into the new journal, so leaving
// it behind put conversation A above conversation B for anyone attaching.
func TestResetDoesNotCarryHistoryIntoTheNextConversation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := New(Config{
		Tools: reg,
		NewAgent: func(c agent.ConfirmFunc) *agent.Agent {
			return agent.New(&scriptedClient{}, reg, 0.3, c, "")
		},
		Ring: 128,
	})
	t.Cleanup(l.Close)

	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Submit("first conversation", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, KindTurnEnd)
	stop()
	if err := l.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := l.Submit("second conversation", nil); err != nil {
		t.Fatal(err)
	}

	// Served from the journal: Reset cleared the ring (without resetting the
	// sequence counter), so replayLocked falls back to reading the journal
	// file for "second conversation"'s own session id.
	events, err := l.Replay(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind == KindUserMessage && ev.Text == "first conversation" {
			t.Fatalf("the new conversation's journal begins with the old one: %+v", ev)
		}
	}
}

// Attaching to a long conversation must not be mistaken for falling behind.
// The backlog is history the client asked for and is already draining; counting
// it against the cap dropped a client for the length of the conversation rather
// than for its own slowness, and took the first live event with it.
func TestBacklogLongerThanTheCapIsNotADesync(t *testing.T) {
	// Exactly a full ring: the backlog then equals the queue cap, which is the
	// point at which the next live event used to trip the "falling behind" rule.
	const ring = 32
	l := New(Config{Ring: ring})
	t.Cleanup(l.Close)
	for range ring {
		l.emit(Event{Kind: KindNotice, Text: "history"})
	}
	ch, stop, err := l.Subscribe(Client{ID: "late"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	l.emit(Event{Kind: KindNotice, Text: "live"})

	for {
		ev := recv(t, ch)
		if ev.Kind == KindDesync {
			t.Fatalf("attaching dropped the client at seq %d", ev.From)
		}
		if ev.Text == "live" {
			return
		}
	}
}
