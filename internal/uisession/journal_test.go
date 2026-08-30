package uisession

import (
	"errors"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/tools"
)

func journalSession(t *testing.T, ring int) *Local {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := New(Config{
		Tools: reg,
		NewAgent: func(confirm agent.ConfirmFunc) *agent.Agent {
			return agent.New(&scriptedClient{}, reg, 0.3, confirm, "")
		},
		Ring: ring,
	})
	t.Cleanup(l.Close)
	return l
}

// A client away longer than the retained history is served from the journal
// rather than told to reload, which is the whole point of writing one.
func TestReplayFallsBackToJournal(t *testing.T) {
	// The ring has to outlast the turn - a subscriber that falls behind its own
	// queue is dropped, and this test is about the journal, not backpressure -
	// so it is evicted deliberately below instead of by being too small.
	const ring = 64
	l := journalSession(t, ring)
	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Submit("hello", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, KindTurnEnd)
	stop()

	// Push the early events out of the ring.
	for range ring * 2 {
		l.emit(Event{Kind: KindNotice, Text: "filler"})
	}
	evs, err := l.Replay(1)
	if err != nil {
		t.Fatalf("Replay(1) = %v, want the journal to cover it", err)
	}
	if len(evs) == 0 || evs[0].Seq != 2 {
		t.Fatalf("replayed %d events starting at %d, want a contiguous tail from 2",
			len(evs), evs[0].Seq)
	}
	var sawTurn bool
	for _, ev := range evs {
		if ev.Kind == KindTurnEnd {
			sawTurn = true
		}
	}
	if !sawTurn {
		t.Error("the journal did not preserve the turn that scrolled out of memory")
	}
}

// A session with no turns has no id and therefore no journal, so a gap in its
// history is a genuine one.
func TestReplayTruncatedWithoutAJournal(t *testing.T) {
	l := journalSession(t, 2)
	for range 10 {
		l.emit(Event{Kind: KindNotice})
	}
	if _, err := l.Replay(1); !errors.Is(err, ErrTruncated) {
		t.Fatalf("Replay(1) = %v, want ErrTruncated", err)
	}
}

// An oversized tool result is truncated to its head before it is stored, so
// reconnecting does not ship megabytes of grep output. Bytes still records the
// full original length.
func TestOversizedToolResultIsTruncatedInTheJournal(t *testing.T) {
	l := journalSession(t, 64)
	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := l.Submit("go", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, KindTurnEnd)

	big := strings.Repeat("x", blobThreshold*3)
	l.emit(Event{Kind: KindToolEnd, ID: "call-1", Name: "grep", Text: big})

	live := waitFor(t, ch, KindToolEnd)
	if live.Text != big {
		t.Fatalf("the live event was trimmed: %d bytes, want %d", len(live.Text), len(big))
	}

	stored, err := l.Replay(live.Seq - 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 || stored[0].Kind != KindToolEnd {
		t.Fatalf("expected the tool result back, got %+v", stored)
	}
	got := stored[0]
	if len(got.Text) != blobThreshold || got.Bytes != len(big) {
		t.Fatalf("stored form = %d bytes bytes=%d, want a %d-byte head recording %d",
			len(got.Text), got.Bytes, blobThreshold, len(big))
	}
}

// Sequence numbers restart in every process, so a resumed conversation has to
// pick up after the journal's last entry - otherwise a second event is written
// under a number the file already uses and "everything after 5" returns halves
// of two conversations.
func TestResumeContinuesTheSequence(t *testing.T) {
	l := journalSession(t, 64)
	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Submit("first", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, KindTurnEnd)
	id := l.Meta().ID
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := l.Replay(0)
	if err != nil {
		t.Fatal(err)
	}
	stop()

	if err := l.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Load(id); err != nil {
		t.Fatal(err)
	}
	after, err := l.Replay(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before) {
		t.Fatalf("timeline shrank on resume: %d then %d", len(before), len(after))
	}
	seen := map[uint64]bool{}
	for _, ev := range after {
		if seen[ev.Seq] {
			t.Fatalf("sequence number %d written twice", ev.Seq)
		}
		seen[ev.Seq] = true
	}
}
