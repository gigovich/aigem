package uisession

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/tools"
)

func journalSession(t *testing.T, ring int) *Local {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return journalSessionHere(t, ring)
}

// journalSessionHere builds a second session over the state directory that is
// already set, which is how a test stands in for another process resuming the
// same conversation.
func journalSessionHere(t *testing.T, ring int) *Local {
	t.Helper()
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

// An oversized tool result is kept whole beside the journal and only its head
// is stored inline, so reconnecting does not ship megabytes of grep output.
func TestOversizedToolResultGoesToABlob(t *testing.T) {
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

	big := strings.Repeat("x", journalTextCap*3)
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
	if len(got.Text) != journalTextCap || !got.Blob || got.Bytes != len(big) {
		t.Fatalf("stored form = %d bytes blob=%v bytes=%d, want a %d-byte head recording %d",
			len(got.Text), got.Blob, got.Bytes, journalTextCap, len(big))
	}
	body, err := ReadBlob(l.Meta().ID, live.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if body != big {
		t.Fatalf("the blob is %d bytes, want %d", len(body), len(big))
	}
}

// The blob is the only part of a conversation this package writes outside the
// journal file, and it holds whatever a tool printed. It is the owner's to
// read, in a directory that is the owner's to list.
func TestBlobIsWrittenForTheOwnerOnly(t *testing.T) {
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
	l.emit(Event{Kind: KindToolEnd, ID: "call-1", Name: "grep",
		Text: strings.Repeat("x", journalTextCap*3)})
	live := waitFor(t, ch, KindToolEnd)

	dir, err := journalDir(l.Meta().ID)
	if err != nil {
		t.Fatal(err)
	}
	blobs := filepath.Join(dir, "blobs")
	if fi, err := os.Stat(blobs); err != nil {
		t.Fatal(err)
	} else if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("the blobs directory is %04o, want 0700", perm)
	}
	fi, err := os.Stat(filepath.Join(blobs, strconv.FormatUint(live.Seq, 10)))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the blob is %04o, want 0600", perm)
	}
	// The rename is what publishes the body; a temp left behind would be a file
	// growing in the state directory for every oversized result ever produced.
	entries, err := os.ReadDir(blobs)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("a half-written blob was left behind: %s", e.Name())
		}
	}
}

// A session with nowhere to write cannot keep the body, and the event it stores
// must not claim otherwise: a client that saw blob=true and got a 404 would
// have no way to tell a lost body from a bug in its own fetching. Events
// emitted before the first turn are the reachable case - the journal is not
// opened until a conversation has one.
func TestATrimmedEventPromisesNoBlobWithoutAJournal(t *testing.T) {
	l := journalSession(t, 64)
	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	big := strings.Repeat("x", journalTextCap*3)
	l.emit(Event{Kind: KindToolEnd, ID: "call-1", Name: "grep", Text: big})
	live := waitFor(t, ch, KindToolEnd)

	stored, err := l.Replay(live.Seq - 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 {
		t.Fatal("the event was not retained")
	}
	if got := stored[0]; got.Blob || got.Bytes != len(big) || len(got.Text) != journalTextCap {
		t.Fatalf("stored form = %d bytes blob=%v bytes=%d, want a trimmed head promising nothing",
			len(got.Text), got.Blob, got.Bytes)
	}
}

// The promise invariant, driven from the other side: the journal is open, the
// result is oversized, and the write fails anyway. The stored event has to say
// so, because a client that saw blob=true and got a 404 has no way to tell a
// lost body from a bug in its own fetching.
func TestATrimmedEventPromisesNoBlobWhenTheWriteFails(t *testing.T) {
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

	// A state directory that lost the blobs directory under a running session
	// is the reachable shape of every write failure: no space, no permission,
	// a directory somebody removed.
	dir, err := journalDir(l.Meta().ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "blobs")); err != nil {
		t.Fatal(err)
	}

	big := strings.Repeat("x", journalTextCap*3)
	l.emit(Event{Kind: KindToolEnd, ID: "call-1", Name: "grep", Text: big})
	live := waitFor(t, ch, KindToolEnd)

	stored, err := l.Replay(live.Seq - 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 {
		t.Fatal("the event was not retained")
	}
	if got := stored[0]; got.Blob || got.Bytes != len(big) || len(got.Text) != journalTextCap {
		t.Fatalf("stored form = %d bytes blob=%v bytes=%d, want a trimmed head promising nothing",
			len(got.Text), got.Blob, got.Bytes)
	}
	if _, err := ReadBlob(l.Meta().ID, live.Seq); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadBlob after a failed write = %v, want a not-exist error", err)
	}
}

// A delegated run's tool results are trimmed and stored the same way, and the
// seq is what keeps two of them apart. It is the case the key was chosen for:
// when a provider supplies no call id the agent numbers them per agent, so two
// concurrent subagents both produce "call-1".
func TestTwoNestedCallsWithOneIdGetTheirOwnBlobs(t *testing.T) {
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

	first := strings.Repeat("a", journalTextCap*3)
	second := strings.Repeat("b", journalTextCap*3)
	l.emit(Event{Kind: KindSubToolEnd, ID: "call-1", RunID: "task-1", Name: "grep", Text: first})
	one := waitFor(t, ch, KindSubToolEnd)
	l.emit(Event{Kind: KindSubToolEnd, ID: "call-1", RunID: "task-2", Name: "grep", Text: second})
	two := waitFor(t, ch, KindSubToolEnd)

	if one.Seq == two.Seq {
		t.Fatalf("both nested results were emitted under seq %d", one.Seq)
	}
	id := l.Meta().ID
	got, err := ReadBlob(id, one.Seq)
	if err != nil || got != first {
		t.Errorf("the first nested blob is %d bytes (%v), want %d", len(got), err, len(first))
	}
	got, err = ReadBlob(id, two.Seq)
	if err != nil || got != second {
		t.Errorf("the second nested blob is %d bytes (%v), want %d", len(got), err, len(second))
	}
}

// A rename that cannot land leaves nothing behind and promises nothing. Sync
// and Close failing reach the same cleanup, but the rename is the one a test
// can drive without reaching inside putBlob.
func TestABlobThatCannotBePublishedLeavesNoTemp(t *testing.T) {
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

	// A directory standing where the blob goes is how the rename is made to
	// fail without reaching inside putBlob: the write, the sync and the close
	// all succeed, and only the last step cannot complete.
	dir, err := journalDir(l.Meta().ID)
	if err != nil {
		t.Fatal(err)
	}
	blobs := filepath.Join(dir, "blobs")
	if err := os.MkdirAll(filepath.Join(blobs, strconv.FormatUint(l.Seq()+1, 10)), 0o700); err != nil {
		t.Fatal(err)
	}

	big := strings.Repeat("x", journalTextCap*3)
	l.emit(Event{Kind: KindToolEnd, ID: "call-1", Name: "grep", Text: big})
	live := waitFor(t, ch, KindToolEnd)

	stored, err := l.Replay(live.Seq - 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 || stored[0].Blob {
		t.Fatalf("stored form = %+v, want an event promising no body", stored)
	}
	entries, err := os.ReadDir(blobs)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("a blob that could not be published left %s behind", e.Name())
		}
	}
}

// A journal that cannot be read through must not be appended to. Numbering
// would restart over sequences the file already holds, and the blob written
// under each of them would replace a body a stored event still points at.
func TestAnUnreadableJournalIsNotRenumberedOver(t *testing.T) {
	l := journalSession(t, 64)
	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Submit("go", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, KindTurnEnd)
	first := strings.Repeat("a", journalTextCap*3)
	l.emit(Event{Kind: KindToolEnd, ID: "call-1", Name: "grep", Text: first})
	kept := waitFor(t, ch, KindToolEnd)
	stop()
	id := l.Meta().ID
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}

	// One line past what ReadJournal will scan. Any read failure does this; a
	// line too long is the one a test can produce.
	dir, err := journalDir(id)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("z", 5<<20) + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// A second process resuming the same conversation, over the same state
	// directory: a fresh session would get its own and see no journal at all.
	next := journalSessionHere(t, 64)
	if _, err := next.Load(id); err != nil {
		t.Fatal(err)
	}
	ch2, stop2, err := next.Subscribe(Client{ID: "d"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	if err := next.Submit("again", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch2, KindTurnEnd)
	next.emit(Event{Kind: KindToolEnd, ID: "call-1", Name: "grep",
		Text: strings.Repeat("b", journalTextCap*3)})
	waitFor(t, ch2, KindToolEnd)

	body, err := ReadBlob(id, kept.Seq)
	if err != nil {
		t.Fatalf("the first conversation's blob is gone: %v", err)
	}
	if body != first {
		t.Fatalf("the blob at seq %d was overwritten: it now starts %q",
			kept.Seq, body[:1])
	}
}

// A seq nothing was stored under surfaces as the underlying not-exist error, so
// a caller can tell "no body was kept" from "the body is empty".
func TestReadBlobReportsAMissingOne(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := ReadBlob("20260905-120000-abcd", 7); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadBlob for an unwritten seq = %v, want a not-exist error", err)
	}
}

// The session id reaches journalDir from a record in the state directory, which
// is a JSON file somebody can edit. Anything but one path element is a way to
// point it at a directory that is not a journal.
func TestJournalDirRefusesAnIdThatIsAPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, id := range []string{"", ".", "..", "../..", "a/b", "/etc"} {
		if dir, err := journalDir(id); err == nil {
			t.Errorf("journalDir(%q) = %q, want a refusal", id, dir)
		}
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
	// Served from the still-intact ring: the events are still retained in memory.
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
	// Served from the journal: Reset cleared the ring (without resetting the
	// sequence counter), so this only sees Load's own meta event unless
	// replayLocked falls back to reading the journal file.
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
