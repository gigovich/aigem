package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	amiran  = "bot:amiran"
	demetre = "bot:demetre"
	jane    = "bot:jane"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seedActors(t, s)
	return s
}

// seedActors registers the fleet. A thread's creator is a foreign key into
// actors, so nothing can be written until the identities exist - which is the
// same order the daemon does it in at startup.
func seedActors(t *testing.T, s *Store) {
	t.Helper()
	for _, a := range []Actor{
		{ID: Operator, Kind: KindHuman, Name: "operator"},
		{ID: amiran, Kind: KindBot, Name: "amiran", Role: "developer"},
		{ID: demetre, Kind: KindBot, Name: "demetre", Role: "researcher"},
		{ID: jane, Kind: KindBot, Name: "jane", Role: "tester"},
	} {
		if err := s.PutActor(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
}

func mustThread(t *testing.T, s *Store, title string, with ...string) *Thread {
	t.Helper()
	th, err := s.NewThread(t.Context(), title, Operator, with)
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func mustSay(t *testing.T, s *Store, thread, author, body string) Message {
	t.Helper()
	m, err := s.Say(t.Context(), thread, Draft{Author: author, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMigrationsAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	seedActors(t, s)
	th := mustThread(t, s, "keep me", amiran)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening runs Migrate again over a populated database.
	s2, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("reopening a migrated store failed: %v", err)
	}
	defer func() { _ = s2.Close() }()

	v, err := s2.ThreadFor(ctx, Operator, th.ID)
	if err != nil {
		t.Fatalf("the thread did not survive a reopen: %v", err)
	}
	if v.Title != "keep me" {
		t.Fatalf("title = %q, want %q", v.Title, "keep me")
	}
}

// The sequence is what lets messages and timeline events share one ordering, so
// two writers must never be handed the same number.
func TestConcurrentWritersGetDistinctIncreasingSeqs(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "busy", amiran, demetre, jane)

	const writers, each = 8, 12
	var (
		mu   sync.Mutex
		seen = map[uint64]bool{}
		wg   sync.WaitGroup
	)
	authors := []string{Operator, amiran, demetre, jane}
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				m, err := s.Say(ctx, th.ID, Draft{
					Author: authors[w%len(authors)],
					Body:   strings.Repeat("x", 1+i%5),
				})
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if seen[m.Seq] {
					t.Errorf("sequence %d handed out twice", m.Seq)
				}
				seen[m.Seq] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != writers*each {
		t.Fatalf("got %d distinct sequences, want %d", len(seen), writers*each)
	}

	// And the cursor is above every number it handed out.
	top, err := s.Seq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for seq := range seen {
		if seq > top {
			t.Fatalf("cursor is %d but %d was handed out", top, seq)
		}
	}
}

func TestInboxCountsUnreadWithoutTheReadersOwnMessages(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	mustSay(t, s, th.ID, Operator, "the logout at 03:00 is back")
	mustSay(t, s, th.ID, amiran, "taking it")
	last := mustSay(t, s, th.ID, amiran, "reproduced")

	got, err := s.Inbox(ctx, Operator, "", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("inbox has %d threads, want 1", len(got))
	}
	// Two from amiran; the operator's own message is not unread to them.
	if got[0].Unread != 2 {
		t.Fatalf("unread = %d, want 2", got[0].Unread)
	}
	if got[0].LastText != "reproduced" {
		t.Fatalf("preview = %q, want %q", got[0].LastText, "reproduced")
	}
	want := []string{Operator, amiran}
	if len(got[0].Participants) != len(want) {
		t.Fatalf("participants = %v, want %v", got[0].Participants, want)
	}

	if err := s.MarkRead(ctx, Operator, th.ID, last.Seq); err != nil {
		t.Fatal(err)
	}
	got, err = s.Inbox(ctx, Operator, "", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Unread != 0 {
		t.Fatalf("unread after marking read = %d, want 0", got[0].Unread)
	}
}

// Reading backwards must not undo what the reader has already seen: a client
// scrolling up would otherwise resurrect its own unread badge.
func TestMarkReadNeverMovesBackwards(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	mustSay(t, s, th.ID, amiran, "one")
	last := mustSay(t, s, th.ID, amiran, "two")

	if err := s.MarkRead(ctx, Operator, th.ID, last.Seq); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead(ctx, Operator, th.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.Inbox(ctx, Operator, "", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Unread != 0 {
		t.Fatalf("unread = %d after reading backwards, want 0", got[0].Unread)
	}
}

func TestStateFollowsWhoSpokeAndWhetherTheyAsked(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	stateOf := func() string {
		t.Helper()
		v, err := s.ThreadFor(ctx, Operator, th.ID)
		if err != nil {
			t.Fatal(err)
		}
		return v.State
	}

	if got := stateOf(); got != StateIdle {
		t.Fatalf("a new thread is %q, want %q", got, StateIdle)
	}
	mustSay(t, s, th.ID, Operator, "please look")
	if got := stateOf(); got != StateWaiting {
		t.Fatalf("after the operator spoke: %q, want %q", got, StateWaiting)
	}
	mustSay(t, s, th.ID, amiran, "on it")
	if got := stateOf(); got != StateIdle {
		t.Fatalf("after a plain bot reply: %q, want %q", got, StateIdle)
	}
	if _, err := s.Say(ctx, th.ID, Draft{
		Author: amiran, Body: "should I revoke the old token first?", AwaitReply: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(); got != StateNeedsYou {
		t.Fatalf("after a bot asked: %q, want %q", got, StateNeedsYou)
	}
}

// "working" is a fact about the turns table. A stored column duplicating it
// would be wrong for exactly as long as a crash left a turn open.
func TestWorkingIsDerivedFromTheTurnsTable(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	mustSay(t, s, th.ID, Operator, "please look")

	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.ThreadFor(ctx, Operator, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Working || v.State != StateWorking {
		t.Fatalf("mid-turn the thread is working=%v state=%q, want true/%q",
			v.Working, v.State, StateWorking)
	}

	if err := s.EndTurn(ctx, amiran, turn, ""); err != nil {
		t.Fatal(err)
	}
	v, err = s.ThreadFor(ctx, Operator, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Working {
		t.Fatal("the thread still reports working after the turn ended")
	}
	if v.State != StateWaiting {
		t.Fatalf("state after the turn = %q, want the resting %q", v.State, StateWaiting)
	}
}

// A turn with no process behind it is not running, and an inbox that says
// otherwise is worse than one that says nothing.
func TestCloseStaleTurnsEndsWhatARestartOrphaned(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	if _, err := s.BeginTurn(ctx, th.ID, amiran); err != nil {
		t.Fatal(err)
	}

	n, err := s.CloseStaleTurns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("closed %d turns, want 1", n)
	}
	v, err := s.ThreadFor(ctx, Operator, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Working {
		t.Fatal("a thread still reports working after its orphaned turn was closed")
	}
}

func TestTurnAccumulatesUsageAcrossCalls(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []Usage{
		{InputTokens: 1000, CachedTokens: 400, OutputTokens: 120, Calls: 1},
		{InputTokens: 1500, OutputTokens: 60, Calls: 1},
		{Calls: 1, Uncounted: 1}, // the provider reported nothing for this one
	} {
		if err := s.AddUsage(ctx, amiran, turn, u, "grok-4.3"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EndTurn(ctx, amiran, turn, ""); err != nil {
		t.Fatal(err)
	}

	turns, _, _, err := s.Turns(ctx, Operator, th.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	got := turns[0].Usage
	want := Usage{InputTokens: 2500, CachedTokens: 400, OutputTokens: 180, Calls: 3, Uncounted: 1}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
	if turns[0].Model != "grok-4.3" {
		t.Fatalf("model = %q, want %q", turns[0].Model, "grok-4.3")
	}
	if turns[0].Ended.IsZero() {
		t.Fatal("the turn was not marked ended")
	}

	// A late call from a goroutine the turn did not wait for must not reopen the
	// accounting: a number that keeps moving after the work stopped is not a
	// record of it. The write is refused quietly, as EndTurn's second close is.
	if err := s.AddUsage(ctx, amiran, turn,
		Usage{InputTokens: 9999, Calls: 1}, "someone/else"); err != nil {
		t.Fatal(err)
	}
	after, _, _, err := s.Turns(ctx, Operator, th.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].Usage != want || after[0].Model != "grok-4.3" {
		t.Fatalf("an ended turn took more spend: %+v", after[0])
	}
}

// A thread's total is summed in the store, because the caller that wants it
// wants one line and nothing prunes the turns table.
// Spend carries both counts because the panel needs both, and printing one
// where the other belonged is how "94 turns" appeared over a list of 100.
func TestSpendCountsSpendingTurnsAndAllRunsSeparately(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	spent := mustTurn(t, s, th.ID, amiran)
	if err := s.AddUsage(ctx, amiran, spent, Usage{InputTokens: 10, Calls: 1}, "grok"); err != nil {
		t.Fatal(err)
	}
	if err := s.EndTurn(ctx, amiran, spent, ""); err != nil {
		t.Fatal(err)
	}
	// A turn killed before its first usage flush, which is every turn that died
	// inside its first sixteen model calls.
	quiet := mustTurn(t, s, th.ID, amiran)
	if err := s.EndTurn(ctx, amiran, quiet, "interrupted by a restart"); err != nil {
		t.Fatal(err)
	}

	sp, err := s.Spend(ctx, Operator, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Turns != 1 {
		t.Fatalf("spending turns = %d, want the one that reached the provider", sp.Turns)
	}
	if sp.Runs != 2 {
		t.Fatalf("runs = %d, want both", sp.Runs)
	}
}

func TestThreadSpendSumsOnlyTheTurnsThatReachedTheProvider(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran, demetre)

	spend := func(actor string, u Usage, model string) {
		t.Helper()
		turn, err := s.BeginTurn(ctx, th.ID, actor)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddUsage(ctx, actor, turn, u, model); err != nil {
			t.Fatal(err)
		}
		if err := s.EndTurn(ctx, actor, turn, ""); err != nil {
			t.Fatal(err)
		}
	}
	spend(amiran, Usage{InputTokens: 1000, CachedTokens: 400, OutputTokens: 120, Calls: 2,
		Uncounted: 1}, "xai/grok-4.3")
	spend(demetre, Usage{InputTokens: 500, OutputTokens: 60, Calls: 1}, "xai/grok-4.2")
	spend(amiran, Usage{InputTokens: 40, OutputTokens: 10, Calls: 1}, "xai/grok-4.3")
	// Reached nothing, so it is neither counted nor allowed to name a model.
	spend(amiran, Usage{}, "xai/grok-4.9")

	got, err := s.Spend(ctx, Operator, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := Spend{
		Usage:  Usage{InputTokens: 1540, CachedTokens: 400, OutputTokens: 190, Calls: 4, Uncounted: 1},
		Turns:  3,
		Models: []string{"xai/grok-4.2", "xai/grok-4.3"},
	}
	if got.Usage != want.Usage || got.Turns != want.Turns ||
		!slices.Equal(got.Models, want.Models) {
		t.Fatalf("spend = %+v, want %+v", got, want)
	}

	// A thread nobody has worked in reports nothing, rather than a row of zeroes
	// its reader has to recognise as meaning the same.
	quiet := mustThread(t, s, "quiet", amiran)
	if got, err := s.Spend(ctx, Operator, quiet.ID); err != nil || !got.Usage.IsZero() ||
		got.Turns != 0 {
		t.Fatalf("an unworked thread reports %+v (err %v)", got, err)
	}

	// Participation is the boundary here as everywhere else.
	if _, err := s.Spend(ctx, jane, th.ID); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("a non-participant read the thread's spend: %v", err)
	}
}

// Participation is the whole authorization boundary, and a bot that could tell
// "not yours" from "does not exist" could probe for conversations it cannot see.
func TestAnOutsiderCannotReachAThread(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "private", amiran)
	mustSay(t, s, th.ID, amiran, "between us")

	if _, err := s.Say(ctx, th.ID, Draft{Author: jane, Body: "hello"}); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("Say by an outsider: %v, want ErrNoSuchThread", err)
	}
	if _, _, _, err := s.Messages(ctx, jane, th.ID, 0, 10); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("Messages for an outsider: %v, want ErrNoSuchThread", err)
	}
	if _, err := s.ThreadFor(ctx, jane, th.ID); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("ThreadFor an outsider: %v, want ErrNoSuchThread", err)
	}
	if _, err := s.BeginTurn(ctx, th.ID, jane); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("BeginTurn by an outsider: %v, want ErrNoSuchThread", err)
	}
	inbox, err := s.Inbox(ctx, jane, "", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 0 {
		t.Fatalf("an outsider's inbox has %d threads, want 0", len(inbox))
	}
	// The error for a thread that does not exist at all is the same one.
	if _, err := s.ThreadFor(ctx, Operator, "t_0000000000000000"); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("ThreadFor a missing thread: %v, want ErrNoSuchThread", err)
	}
}

// Adding someone is a capability. The alternative is any bot pulling any other
// bot into any conversation.
func TestOnlyAParticipantMayAddOne(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	if err := s.AddParticipant(ctx, jane, th.ID, demetre); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("an outsider added a participant: %v, want ErrNoSuchThread", err)
	}
	if err := s.AddParticipant(ctx, amiran, th.ID, demetre); err != nil {
		t.Fatal(err)
	}
	ok, err := s.IsParticipant(ctx, th.ID, demetre)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("demetre was not added")
	}
}

// A conversation whose membership changed silently cannot be audited.
func TestMembershipChangesAreInTheTranscript(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	if err := s.AddParticipant(ctx, amiran, th.ID, demetre); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveParticipant(ctx, Operator, th.ID, demetre); err != nil {
		t.Fatal(err)
	}
	msgs, _, _, err := s.Messages(ctx, Operator, th.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var joined, left bool
	for _, m := range msgs {
		if m.Kind != MsgSystem {
			continue
		}
		if strings.Contains(m.Body, "demetre joined") {
			joined = true
		}
		if strings.Contains(m.Body, "demetre left") {
			left = true
		}
	}
	if !joined || !left {
		t.Fatalf("transcript records joined=%v left=%v; both must be there", joined, left)
	}
}

// A membership note answers nothing and asks nothing, so it must not clear a
// thread that is still waiting on the operator.
func TestASystemNoteDoesNotChangeTheState(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	if _, err := s.Say(ctx, th.ID, Draft{Author: amiran, Body: "which token?", AwaitReply: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddParticipant(ctx, amiran, th.ID, demetre); err != nil {
		t.Fatal(err)
	}
	v, err := s.ThreadFor(ctx, Operator, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != StateNeedsYou {
		t.Fatalf("state after a join note = %q, want %q", v.State, StateNeedsYou)
	}
}

func TestDeletingAThreadTakesItsRowsWithIt(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	m := mustSay(t, s, th.ID, amiran, "reproduced")
	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, EventRecord{
		Thread: th.ID, Actor: amiran, TurnSeq: turn,
		Kind: "tool_end", Payload: []byte(`{"kind":"tool_end"}`), Blob: []byte("the whole thing"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread(ctx, Operator, th.ID); err != nil {
		t.Fatal(err)
	}

	for _, q := range []struct {
		name  string
		query string
		arg   any
	}{
		{"messages", `SELECT count(*) FROM messages WHERE thread_id = ?`, th.ID},
		{"participants", `SELECT count(*) FROM participants WHERE thread_id = ?`, th.ID},
		{"events", `SELECT count(*) FROM events WHERE thread_id = ?`, th.ID},
		{"turns", `SELECT count(*) FROM turns WHERE thread_id = ?`, th.ID},
		{"blobs", `SELECT count(*) FROM blobs`, nil},
		{"fts", `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'reproduced'`, nil},
	} {
		var n int
		var err error
		if q.arg != nil {
			err = s.r.QueryRowContext(ctx, q.query, q.arg).Scan(&n)
		} else {
			err = s.r.QueryRowContext(ctx, q.query).Scan(&n)
		}
		if err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		if n != 0 {
			t.Fatalf("%d %s rows survived the thread", n, q.name)
		}
	}
	_ = m
}

func TestSearchFindsAMessageAndStaysInsideTheCallersThreads(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	mine := mustThread(t, s, "mine", amiran)
	mustSay(t, s, mine.ID, amiran, "the rotation drops sessions")

	theirs, err := s.NewThread(ctx, "theirs", demetre, []string{jane})
	if err != nil {
		t.Fatal(err)
	}
	mustSay(t, s, theirs.ID, demetre, "the rotation is fine here")

	hits, err := s.Search(ctx, amiran, "rotation", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1 (only the caller's own thread)", len(hits))
	}
	if hits[0].Thread != mine.ID {
		t.Fatalf("hit is in %s, want %s", hits[0].Thread, mine.ID)
	}

	// A query with FTS5 syntax in it must be words, not an operator.
	if _, err := s.Search(ctx, amiran, `auth.Refresh( NOT`, 20); err != nil {
		t.Fatalf("a query with punctuation and a keyword in it errored: %v", err)
	}
}

// A deleted message must leave the index, or search returns rows that no longer
// join to anything.
func TestSearchIndexFollowsDeletes(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "mine", amiran)
	mustSay(t, s, th.ID, amiran, "a distinctive phrase")

	if hits, err := s.Search(ctx, amiran, "distinctive", 20); err != nil || len(hits) != 1 {
		t.Fatalf("before delete: %d hits, err %v", len(hits), err)
	}
	if err := s.DeleteThread(ctx, Operator, th.ID); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, amiran, "distinctive", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("after delete: %d hits, want 0", len(hits))
	}
}

func TestMessagesPageBackwards(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "long", amiran)
	var seqs []uint64
	for i := range 10 {
		seqs = append(seqs, mustSay(t, s, th.ID, amiran, string(rune('a'+i))).Seq)
	}

	page, cursor, more, err := s.Messages(ctx, Operator, th.ID, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 4 || page[0].Seq != seqs[9] {
		t.Fatalf("first page starts at %d with %d rows; want %d and 4", page[0].Seq, len(page), seqs[9])
	}
	// The cursor is the whole point of the envelope: a caller that took the
	// lowest seq it saw would be right here and wrong on the last page, which is
	// the one where the difference loses history.
	if !more || cursor != page[len(page)-1].Seq {
		t.Fatalf("first page reported more=%v cursor=%d; want true and %d",
			more, cursor, page[len(page)-1].Seq)
	}
	older, _, _, err := s.Messages(ctx, Operator, th.ID, cursor, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 4 || older[0].Seq >= cursor {
		t.Fatalf("the second page overlaps the first: %d then %d", cursor, older[0].Seq)
	}

	// The last page is the one a client must be able to recognise. Ten messages
	// in pages of four means the third holds two and ends the thread.
	_, _, more2, err := s.Messages(ctx, Operator, th.ID, older[len(older)-1].Seq, 4)
	if err != nil {
		t.Fatal(err)
	}
	if more2 {
		t.Fatal("the page holding the oldest messages still reported more")
	}

	// A page that exactly fills its limit with nothing behind it must not claim
	// otherwise: this is the off-by-one the limit+1 read exists to get right.
	whole, cursor3, more3, err := s.Messages(ctx, Operator, th.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) != 10 || more3 || cursor3 != 0 {
		t.Fatalf("an exact-fit page reported %d rows, more=%v, cursor=%d; want 10, false, 0",
			len(whole), more3, cursor3)
	}
}

func TestTimelinePagesForwards(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "long turn", amiran)
	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		if _, err := s.AppendEvent(ctx, EventRecord{
			Thread: th.ID, Actor: amiran, TurnSeq: turn, Kind: "tool_start",
			Payload: []byte(fmt.Sprintf(`{"kind":"tool_start","name":"step%d"}`, i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	first, cursor, more, err := s.Timeline(ctx, Operator, th.ID, 0, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 4 || !more || cursor != first[len(first)-1].Seq {
		t.Fatalf("first page: %d events, more=%v, cursor=%d; want 4, true and %d",
			len(first), more, cursor, first[len(first)-1].Seq)
	}
	// Forwards, so the cursor is where the next since starts - the newest
	// delivered event, not the oldest.
	rest, _, more2, err := s.Timeline(ctx, Operator, th.ID, cursor, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	// Checked before the message below indexes it: reporting an empty second
	// page as an index-out-of-range panic hides the very thing that failed.
	if len(rest) == 0 {
		t.Fatal("the second page is empty; the cursor did not advance past the first")
	}
	if more2 || rest[0].Seq <= cursor {
		t.Fatalf("second page: %d events starting at %d, more=%v", len(rest), rest[0].Seq, more2)
	}
	if len(first)+len(rest) != 6 {
		t.Fatalf("paging saw %d events, want all six steps", len(first)+len(rest))
	}
}

func TestTailReplaysMessagesAndOneCurrentThreadRow(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	mustSay(t, s, th.ID, Operator, "look please")

	mark, err := s.Seq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustSay(t, s, th.ID, amiran, "on it")
	mustSay(t, s, th.ID, amiran, "done")

	frames, cursor, more, err := s.Tail(ctx, Operator, mark, 100)
	if err != nil {
		t.Fatal(err)
	}
	if more {
		t.Fatal("a two-message tail with a limit of 100 reported more to come")
	}
	var msgs, threads int
	for _, f := range frames {
		switch f.Stream {
		case StreamMessage:
			msgs++
		case StreamThread:
			threads++
		}
	}
	if msgs != 2 {
		t.Fatalf("replayed %d messages, want 2", msgs)
	}
	// One row as it is now, not a replay of every shape it passed through.
	if threads != 1 {
		t.Fatalf("replayed %d thread frames, want 1", threads)
	}
	// Resuming from the reported cursor delivers nothing twice.
	again, _, _, err := s.Tail(ctx, Operator, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("resuming from the reported cursor replayed %d frames, want 0", len(again))
	}

	// An outsider replays nothing.
	frames, _, _, err = s.Tail(ctx, jane, mark, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("an outsider replayed %d frames, want 0", len(frames))
	}
}

func TestBlobRoundTripsAndIsScopedToParticipants(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("go test output\n", 500)
	seq, err := s.AppendEvent(ctx, EventRecord{
		Thread: th.ID, Actor: amiran, TurnSeq: turn, Kind: "tool_end",
		Payload: []byte(`{"kind":"tool_end","text":"head"}`), Blob: []byte(body),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Blob(ctx, Operator, th.ID, seq)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("blob round trip changed %d bytes into %d", len(body), len(got))
	}
	if _, err := s.Blob(ctx, jane, th.ID, seq); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("an outsider read a blob: %v, want ErrNoSuchThread", err)
	}
}

func TestTimelineReturnsEventsAfterSince(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for _, kind := range []string{"tool_start", "tool_end", "assistant_message"} {
		seq, err := s.AppendEvent(ctx, EventRecord{
			Thread: th.ID, Actor: amiran, TurnSeq: turn, Kind: kind,
			Payload: []byte(`{"kind":"` + kind + `"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}

	frames, _, _, err := s.Timeline(ctx, Operator, th.ID, seqs[0], 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d events after the first, want 2", len(frames))
	}
	if frames[0].Seq != seqs[1] || frames[1].Seq != seqs[2] {
		t.Fatalf("events came back out of order: %d then %d", frames[0].Seq, frames[1].Seq)
	}
	if string(frames[0].Event) != `{"kind":"tool_end"}` {
		t.Fatalf("payload = %s, want the stored JSON verbatim", frames[0].Event)
	}
}

// The timeline is large and stops being worth its disk within days. What was
// said is the record, and is never pruned.
func TestPruneDropsOldEventsAndKeepsMessages(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	old := time.Now().Add(-72 * time.Hour)
	s.now = func() time.Time { return old }
	mustSay(t, s, th.ID, amiran, "an old message")
	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, EventRecord{
		Thread: th.ID, Actor: amiran, TurnSeq: turn, Kind: "tool_end",
		Payload: []byte(`{"kind":"tool_end"}`), Blob: []byte("old output"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.EndTurn(ctx, amiran, turn, ""); err != nil {
		t.Fatal(err)
	}

	s.now = time.Now
	fresh, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	freshSeq, err := s.AppendEvent(ctx, EventRecord{
		Thread: th.ID, Actor: amiran, TurnSeq: fresh, Kind: "tool_end",
		Payload: []byte(`{"kind":"tool_end"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	events, blobs, _, err := s.Prune(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if events != 1 || blobs != 1 {
		t.Fatalf("pruned %d events and %d blobs, want 1 and 1", events, blobs)
	}
	frames, _, _, err := s.Timeline(ctx, Operator, th.ID, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].Seq != freshSeq {
		t.Fatalf("after pruning the timeline has %d events, want only the fresh one", len(frames))
	}
	msgs, _, _, err := s.Messages(ctx, Operator, th.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("pruning removed messages: %d left, want 1", len(msgs))
	}
}

// A run whose history was amputated under it would render as a live turn with
// no work in it.
func TestPruneLeavesARunningTurnsEventsAlone(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	s.now = func() time.Time { return time.Now().Add(-72 * time.Hour) }
	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, EventRecord{
		Thread: th.ID, Actor: amiran, TurnSeq: turn, Kind: "tool_start",
		Payload: []byte(`{"kind":"tool_start"}`),
	}); err != nil {
		t.Fatal(err)
	}
	s.now = time.Now

	events, _, _, err := s.Prune(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("pruned %d events belonging to a turn that is still running", events)
	}

	// Once it ends, the same events are collectable.
	if err := s.EndTurn(ctx, amiran, turn, ""); err != nil {
		t.Fatal(err)
	}
	events, _, _, err = s.Prune(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("pruned %d events after the turn ended, want 1", events)
	}
}

func TestInboxFiltersByEffectiveState(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	asking := mustThread(t, s, "asking", amiran)
	running := mustThread(t, s, "running", demetre)

	if _, err := s.Say(ctx, asking.ID, Draft{Author: amiran, Body: "which one?", AwaitReply: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTurn(ctx, running.ID, demetre); err != nil {
		t.Fatal(err)
	}

	needs, err := s.Inbox(ctx, Operator, StateNeedsYou, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(needs) != 1 || needs[0].ID != asking.ID {
		t.Fatalf("needs_you returned %d threads, want only %s", len(needs), asking.ID)
	}
	working, err := s.Inbox(ctx, Operator, StateWorking, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(working) != 1 || working[0].ID != running.ID {
		t.Fatalf("working returned %d threads, want only %s", len(working), running.ID)
	}
}

// Filtering in Go after the query would let LIMIT drop matching threads to make
// room for ones that were then filtered out, which reads as an inbox that lost
// a thread.
func TestStateFilterIsAppliedBeforeTheLimit(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()

	// The one thread that needs the operator is the oldest, so anything that
	// filters after LIMIT will miss it.
	asking := mustThread(t, s, "asking", amiran)
	if _, err := s.Say(ctx, asking.ID, Draft{Author: amiran, Body: "which token?", AwaitReply: true}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		quiet := mustThread(t, s, "quiet", demetre)
		mustSay(t, s, quiet.ID, demetre, "note "+string(rune('a'+i)))
	}

	got, err := s.Inbox(ctx, Operator, StateNeedsYou, false, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != asking.ID {
		t.Fatalf("needs_you with a limit of 3 returned %d threads, want the one that asks", len(got))
	}
}

func TestArchivedThreadsLeaveTheInbox(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "done with this", amiran)
	mustSay(t, s, th.ID, amiran, "shipped")

	if err := s.SetArchived(ctx, Operator, th.ID, true); err != nil {
		t.Fatal(err)
	}
	live, err := s.Inbox(ctx, Operator, "", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("an archived thread is still in the inbox: %d rows", len(live))
	}
	archived, err := s.Inbox(ctx, Operator, "", true, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 {
		t.Fatalf("the archive has %d threads, want 1", len(archived))
	}
}

func TestPublisherSeesFramesOnlyAfterTheCommit(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()

	var mu sync.Mutex
	var got []Frame
	_ = s.AddPublisher("test", func(f []Frame) {
		mu.Lock()
		defer mu.Unlock()
		// If this ran inside the transaction, the row would not be readable yet
		// from the reader pool.
		for _, fr := range f {
			if fr.Stream == StreamMessage && fr.Message != nil {
				var n int
				if err := s.r.QueryRowContext(ctx,
					`SELECT count(*) FROM messages WHERE seq = ?`, fr.Message.Seq).Scan(&n); err != nil {
					t.Error(err)
				}
				if n != 1 {
					t.Errorf("frame for seq %d published before its row was committed", fr.Message.Seq)
				}
			}
		}
		got = append(got, f...)
	})

	th := mustThread(t, s, "retries", amiran)
	mustSay(t, s, th.ID, amiran, "reproduced")

	mu.Lock()
	defer mu.Unlock()
	var msg, thread int
	for _, f := range got {
		switch f.Stream {
		case StreamMessage:
			msg++
		case StreamThread:
			thread++
		}
	}
	if msg != 1 {
		t.Fatalf("published %d message frames, want 1", msg)
	}
	// One for opening the thread, one for the message landing in it.
	if thread != 2 {
		t.Fatalf("published %d thread frames, want 2", thread)
	}
}

// The payload has to stay JSON on the wire. As a []byte it would be base64'd
// into a string the browser would have to decode to find the event it was sent.
func TestFrameJSONKeepsTheEventPayloadAsJSON(t *testing.T) {
	f := Frame{Seq: 7, Stream: StreamEvent, ThreadID: "t_0102030405060708",
		Event: []byte(`{"kind":"tool_end","text":"ok"}`)}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"event":{"kind":"tool_end"`) {
		t.Fatalf("event was not embedded as JSON: %s", b)
	}
	var back Frame
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if string(back.Event) != string(f.Event) || back.Seq != f.Seq || back.Stream != f.Stream {
		t.Fatalf("round trip changed the frame: %+v", back)
	}
}

func TestValidThreadID(t *testing.T) {
	s := newStore(t)
	th := mustThread(t, s, "retries", amiran)
	if !ValidThreadID(th.ID) {
		t.Fatalf("a freshly minted id %q is rejected", th.ID)
	}
	for _, bad := range []string{"", "t_", "t_zzzz", "t_0102030405060708extra", "0102030405060708"} {
		if ValidThreadID(bad) {
			t.Fatalf("%q is accepted as a thread id", bad)
		}
	}
}

// The WAL and SHM sidecars hold the same plaintext as the database, and SQLite
// copies the database file's mode onto them when it creates them - so chmodding
// only chat.db protected a third of the data.
func TestTheDatabaseAndItsSidecarsArePrivate(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	seedActors(t, s)
	th := mustThread(t, s, "retries", amiran)
	mustSay(t, s, th.ID, amiran, "something worth not leaking")

	for _, name := range []string{"chat.db", "chat.db-wal", "chat.db-shm"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue // the sidecars only exist while a connection is open
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s is mode %04o, want 0600", name, mode)
		}
	}
}

// ---- stage 7: the trace, its counters and its diffs ----

func TestArtifactsDefaultToTheNewestTurnThatChangedAnything(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	first := mustTurn(t, s, th.ID, amiran)
	if _, err := s.PutArtifact(ctx, amiran, th.ID, first,
		Artifact{Path: "internal/auth/flow.go", Old: "a\n", New: "b\n"}); err != nil {
		t.Fatal(err)
	}
	if err := s.EndTurn(ctx, amiran, first, ""); err != nil {
		t.Fatal(err)
	}
	second := mustTurn(t, s, th.ID, amiran)
	if _, err := s.PutArtifact(ctx, amiran, th.ID, second,
		Artifact{Path: "internal/auth/store.go", New: "c\n", Created: true}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Artifacts(ctx, Operator, th.ID, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "internal/auth/store.go" {
		t.Fatalf("artifacts = %+v, want only the newest turn's", got)
	}
	// The list draws filenames; content is shipped only for a path asked for by
	// name, so the panel does not pay for every diff it is not showing.
	if got[0].New != "" {
		t.Fatalf("the list carried %d bytes of content", len(got[0].New))
	}
	one, err := s.Artifacts(ctx, Operator, th.ID, first, "internal/auth/flow.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Old != "a\n" || one[0].New != "b\n" {
		t.Fatalf("named artifact = %+v, want its content", one)
	}
}

// Participation is the whole authorization boundary here as everywhere else: a
// diff is the content of the operator's files, and a bot outside the thread
// must not be able to read one by guessing a turn number.
func TestAnOutsiderCannotReadOrWriteArtifacts(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	turn := mustTurn(t, s, th.ID, amiran)

	if _, err := s.Artifacts(ctx, jane, th.ID, turn, ""); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("an outsider read artifacts: %v", err)
	}
	if _, err := s.PutArtifact(ctx, jane, th.ID, turn,
		Artifact{Path: "internal/auth/flow.go", New: "x"}); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("an outsider wrote an artifact: %v", err)
	}
	// And a participant cannot file one under another bot's run: turn numbers
	// are small consecutive integers, so guessing one is not a barrier.
	if err := s.AddParticipant(ctx, Operator, th.ID, demetre); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutArtifact(ctx, demetre, th.ID, turn,
		Artifact{Path: "internal/auth/flow.go", New: "x"}); !errors.Is(err, ErrNoSuchTurn) {
		t.Fatalf("a participant wrote into another bot's turn: %v", err)
	}
}

// A path is an identity, not a label: it is half an artifact's primary key.
// Shortening it the way a model name is shortened made two files that shared a
// long prefix into one row holding the first file's "before" against the
// second's "after", under a name that was neither.
func TestALongPathIsCleanedButNotShortened(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	turn := mustTurn(t, s, th.ID, amiran)

	deep := "internal/" + strings.Repeat("nested/", 20)
	for _, path := range []string{deep + "flow.go", deep + "store.go"} {
		if _, err := s.PutArtifact(ctx, amiran, th.ID, turn,
			Artifact{Path: path, Old: "before", New: "after"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Artifacts(ctx, Operator, th.ID, turn, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("two deep paths collapsed into %d row(s): %+v", len(got), got)
	}

	// And it is idempotent at its bound, because it runs once where the timeline
	// event is built and again in the store: an order that trimmed before
	// truncating left a trailing space on a path whose cut fell on one, and two
	// spellings of a path are two rows.
	long := strings.Repeat("a", MaxPathChars-1) + " b.go"
	if once, twice := ReadablePath(long), ReadablePath(ReadablePath(long)); once != twice {
		t.Fatalf("ReadablePath is not idempotent: %q then %q", once, twice)
	}

	// The cleaning still happens: a bidi override in a filename shows the
	// operator a reversed extension.
	if _, err := s.PutArtifact(ctx, amiran, th.ID, turn,
		Artifact{Path: "report\u202egnp.exe", New: "x"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Artifacts(ctx, Operator, th.ID, turn, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if strings.ContainsRune(a.Path, 0x202e) {
			t.Fatalf("a bidi override survived into %q", a.Path)
		}
	}
}

// A file too large to keep says so. Serving it as an empty diff would render as
// a file that had been emptied, which is a different and alarming fact.
func TestAnOversizedDiffIsMarkedTruncatedAndStaysThatWay(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	turn := mustTurn(t, s, th.ID, amiran)

	huge := strings.Repeat("x", MaxArtifactBytes+1)
	if _, err := s.PutArtifact(ctx, amiran, th.ID, turn,
		Artifact{Path: "internal/web/ui/dist/bundle.js", Old: "", New: huge}); err != nil {
		t.Fatal(err)
	}
	// The next edit is small, but there is nothing left to diff it against: the
	// original content was never kept, so showing it alone would draw the whole
	// file as created.
	if _, err := s.PutArtifact(ctx, amiran, th.ID, turn,
		Artifact{Path: "internal/web/ui/dist/bundle.js", Old: huge, New: "small\n"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Artifacts(ctx, Operator, th.ID, turn, "internal/web/ui/dist/bundle.js")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Truncated {
		t.Fatalf("artifact = %+v, want it marked truncated", got)
	}
	if got[0].Old != "" || got[0].New != "" {
		t.Fatalf("a truncated artifact carried content: %+v", got[0])
	}
	turns, _, _, err := s.Turns(ctx, Operator, th.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].Files != 1 {
		t.Fatalf("turn files = %d, want the one file counted once", turns[0].Files)
	}
}

func TestTurnsPageNewestFirst(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	var seqs []uint64
	for range 5 {
		turn := mustTurn(t, s, th.ID, amiran)
		if err := s.EndTurn(ctx, amiran, turn, ""); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, turn)
	}

	page, cursor, more, err := s.Turns(ctx, Operator, th.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || !more {
		t.Fatalf("page = %d turns, more = %v; want 2 and more", len(page), more)
	}
	if page[0].Seq != seqs[4] || page[1].Seq != seqs[3] {
		t.Fatalf("page = %d,%d; want the two newest", page[0].Seq, page[1].Seq)
	}
	rest, _, more2, err := s.Turns(ctx, Operator, th.ID, cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 3 || more2 {
		t.Fatalf("rest = %d turns, more = %v; want the remaining 3 and no more", len(rest), more2)
	}
}

// Expanding one collapsed trace must not pull the thread's whole history: a
// working thread is thousands of events, and the reader asked about one run.
func TestTimelineNarrowsToOneTurn(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	first := mustTurn(t, s, th.ID, amiran)
	mustEvent(t, s, th.ID, amiran, first, "tool_start")
	if err := s.EndTurn(ctx, amiran, first, ""); err != nil {
		t.Fatal(err)
	}
	second := mustTurn(t, s, th.ID, amiran)
	mustEvent(t, s, th.ID, amiran, second, "tool_start")
	mustEvent(t, s, th.ID, amiran, second, "tool_end")

	all, _, _, err := s.Timeline(ctx, Operator, th.ID, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("the whole timeline has %d events, want 3", len(all))
	}
	one, _, _, err := s.Timeline(ctx, Operator, th.ID, 0, second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 2 {
		t.Fatalf("the second turn has %d events, want 2", len(one))
	}
}

// The counters are what the summary line reads, so they are asserted against
// the store rather than against the producer that fills them in.
func TestATurnsCountersAndPlanAreKeptOnTheRow(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	turn := mustTurn(t, s, th.ID, amiran)

	for _, rec := range []EventRecord{
		{Kind: "tool_start", Step: true, Tool: true},
		{Kind: "tool_end", Step: true},
		{Kind: "todo", Step: true, Plan: json.RawMessage(`[{"text":"patch","status":"pending"}]`)},
		{Kind: "turn_start"},
	} {
		rec.Thread, rec.Actor, rec.TurnSeq = th.ID, amiran, turn
		rec.Payload = []byte(`{"kind":"` + rec.Kind + `"}`)
		if _, err := s.AppendEvent(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}

	turns, _, _, err := s.Turns(ctx, Operator, th.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].Steps != 3 || turns[0].Tools != 1 {
		t.Fatalf("turn = %d steps / %d tools, want 3 and 1", turns[0].Steps, turns[0].Tools)
	}
	if string(turns[0].Plan) != `[{"text":"patch","status":"pending"}]` {
		t.Fatalf("plan = %q", turns[0].Plan)
	}
}

// Artifacts hang off turns, and nothing prunes turns. Without this they are the
// one thing in the store that only ever grows - and they are the largest thing
// in it, because each one is two copies of a file.
func TestPruneDropsOldArtifacts(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	s.now = func() time.Time { return time.Now().Add(-72 * time.Hour) }
	old := mustTurn(t, s, th.ID, amiran)
	if _, err := s.PutArtifact(ctx, amiran, th.ID, old,
		Artifact{Path: "internal/auth/flow.go", Old: "a\n", New: "b\n"}); err != nil {
		t.Fatal(err)
	}
	if err := s.EndTurn(ctx, amiran, old, ""); err != nil {
		t.Fatal(err)
	}

	s.now = time.Now
	fresh := mustTurn(t, s, th.ID, amiran)
	if _, err := s.PutArtifact(ctx, amiran, th.ID, fresh,
		Artifact{Path: "internal/auth/store.go", Old: "c\n", New: "d\n"}); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := s.Prune(ctx, time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	gone, err := s.Artifacts(ctx, Operator, th.ID, old, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 0 {
		t.Fatalf("the old turn still has %d artifacts", len(gone))
	}
	kept, err := s.Artifacts(ctx, Operator, th.ID, fresh, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 {
		t.Fatalf("the fresh turn has %d artifacts, want the one just written", len(kept))
	}
}

// The same reasoning as an event's turn check, and it has to be made in Say
// too: without it any participant could file a message under another bot's run
// and it would render inside that bot's trace.
func TestSayRefusesATurnTheAuthorDoesNotOwn(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran, demetre)
	other := mustThread(t, s, "docs", amiran)
	turn := mustTurn(t, s, th.ID, amiran)

	if _, err := s.Say(ctx, th.ID, Draft{Author: demetre, Body: "mine now", TurnSeq: turn}); !errors.Is(err, ErrNoSuchTurn) {
		t.Fatalf("a bot filed a message under another's turn: %v", err)
	}
	if _, err := s.Say(ctx, other.ID, Draft{Author: amiran, Body: "wrong thread", TurnSeq: turn}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a message was filed under a turn in another thread: %v", err)
	}
}

// The comment on the counters update says a late event from a goroutine that
// outlived its run cannot inflate a closed turn. Nothing asserted it, and
// deleting the guard left every suite green.
func TestAClosedTurnTakesNoMoreStepsOrFiles(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	turn := mustTurn(t, s, th.ID, amiran)
	mustEvent(t, s, th.ID, amiran, turn, "tool_start")
	if _, err := s.PutArtifact(ctx, amiran, th.ID, turn,
		Artifact{Path: "internal/auth/flow.go", New: "b\n"}); err != nil {
		t.Fatal(err)
	}
	if err := s.EndTurn(ctx, amiran, turn, ""); err != nil {
		t.Fatal(err)
	}

	// Both writes still land - the timeline is a record of what happened, and a
	// late step did happen - but neither moves a number on a finished run.
	mustEvent(t, s, th.ID, amiran, turn, "tool_start")
	if _, err := s.PutArtifact(ctx, amiran, th.ID, turn,
		Artifact{Path: "internal/auth/store.go", New: "c\n"}); err != nil {
		t.Fatal(err)
	}

	turns, _, _, err := s.Turns(ctx, Operator, th.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].Steps != 1 {
		t.Fatalf("a closed turn took another step: %d", turns[0].Steps)
	}
	if turns[0].Files != 1 {
		t.Fatalf("a closed turn took another file: %d", turns[0].Files)
	}
	got, err := s.Artifacts(ctx, Operator, th.ID, turn, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a closed turn recorded %d files, want the one from before it ended", len(got))
	}
}

// Past the cap a path is dropped rather than refused, and the count stops with
// it - so the figure on the summary line is what the panel can actually list.
func TestATurnStopsRecordingFilesPastTheCap(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	turn := mustTurn(t, s, th.ID, amiran)

	for i := range MaxTurnArtifacts + 5 {
		stored, err := s.PutArtifact(ctx, amiran, th.ID, turn,
			Artifact{Path: fmt.Sprintf("generated/file%03d.go", i), New: "x"})
		if err != nil {
			t.Fatal(err)
		}
		// The caller writes a timeline step only for a file that was kept, so a
		// dropped one must say so - otherwise the summary line counts past the
		// number of rows the panel can list.
		if want := i < MaxTurnArtifacts; stored != want {
			t.Fatalf("file %d: stored = %v, want %v", i, stored, want)
		}
	}
	// And the ones past it, written again: a dropped path is not known, so a
	// naive count would tick up once per edit rather than once per file.
	for i := MaxTurnArtifacts; i < MaxTurnArtifacts+5; i++ {
		if _, err := s.PutArtifact(ctx, amiran, th.ID, turn,
			Artifact{Path: fmt.Sprintf("generated/file%03d.go", i), New: "y"}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Artifacts(ctx, Operator, th.ID, turn, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxTurnArtifacts {
		t.Fatalf("recorded %d files, want the cap of %d", len(got), MaxTurnArtifacts)
	}
	turns, _, _, err := s.Turns(ctx, Operator, th.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].Files != MaxTurnArtifacts {
		t.Fatalf("turn files = %d, want the cap; the badge and the list must agree",
			turns[0].Files)
	}
}

// Parallel subagent tool calls write from several goroutines at once. The store
// has one writer, but the dedupe is a read followed by a write and the counter
// is what a summary line reads.
func TestConcurrentFileChangesCountEachPathOnce(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	turn := mustTurn(t, s, th.ID, amiran)

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 10 {
				_, _ = s.PutArtifact(ctx, amiran, th.ID, turn, Artifact{
					Path: fmt.Sprintf("internal/auth/f%d.go", i),
					New:  fmt.Sprintf("by %d\n", w),
				})
			}
		}(w)
	}
	wg.Wait()

	turns, _, _, err := s.Turns(ctx, Operator, th.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].Files != 10 {
		t.Fatalf("turn files = %d, want the 10 distinct paths", turns[0].Files)
	}
}

// Reclaiming space after a prune must finish, and it must not depend on
// `incremental_vacuum` doing anything.
//
// The pragma in 0001_init.sql was a no-op for the whole life of the store: the
// driver applies the DSN's journal_mode first, that initialises the file, and
// auto_vacuum cannot be changed afterwards. A reclaim loop written to stop when
// the free list reached zero therefore spun forever on the single writer, on
// every prune, for the life of the daemon.
func TestPruneReclaimsSpaceAndTerminates(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	var mode int
	if err := s.w.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (incremental); the DSN sets it before "+
			"journal_mode initialises the file, and a migration cannot", mode)
	}

	if err := prunes(ctx, s); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.w.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("free list still holds %d pages; nothing was returned to the filesystem", left)
	}
}

// The case the loop actually had to survive, and the one every deployed store
// is in: a file created before auto_vacuum was in the DSN. The pragma cannot be
// set afterwards, so `incremental_vacuum` frees nothing and the free list never
// reaches zero - which is why the loop stops on it failing to shrink instead.
func TestPruneTerminatesOnAStoreWithoutAutoVacuum(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.db")

	// Built the way a store was before the fix: journal_mode initialises the
	// file, and auto_vacuum is silently stuck at 0 from then on.
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `CREATE TABLE seed (x)`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seedActors(t, s)

	var mode int
	if err := s.w.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 0 {
		t.Skipf("this store came up with auto_vacuum %d; the case under test needs 0", mode)
	}
	if err := prunes(ctx, s); err != nil {
		t.Fatal(err)
	}
}

// prunes fills a store with prunable history, prunes it, and fails if that does
// not return - a loop waiting for a free list that cannot shrink spins on the
// single writer forever, on every prune, for the life of the daemon.
func prunes(ctx context.Context, s *Store) error {
	th, err := s.NewThread(ctx, "retries", Operator, []string{amiran})
	if err != nil {
		return err
	}
	s.now = func() time.Time { return time.Now().Add(-72 * time.Hour) }
	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		return err
	}
	for range 200 {
		if _, err := s.AppendEvent(ctx, EventRecord{
			Thread: th.ID, Actor: amiran, TurnSeq: turn, Kind: "tool_end",
			Payload: []byte(`{"kind":"tool_end"}`), Blob: make([]byte, 60<<10),
		}); err != nil {
			return err
		}
	}
	if err := s.EndTurn(ctx, amiran, turn, ""); err != nil {
		return err
	}
	s.now = time.Now

	done := make(chan error, 1)
	go func() {
		_, _, _, err := s.Prune(ctx, time.Now().Add(-24*time.Hour))
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		return errors.New("Prune did not return; the reclaim loop is spinning on the writer")
	}
}

func mustTurn(t *testing.T, s *Store, threadID, actor string) uint64 {
	t.Helper()
	turn, err := s.BeginTurn(t.Context(), threadID, actor)
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func mustEvent(t *testing.T, s *Store, threadID, actor string, turn uint64, kind string) uint64 {
	t.Helper()
	seq, err := s.AppendEvent(t.Context(), EventRecord{
		Thread: threadID, Actor: actor, TurnSeq: turn, Kind: kind,
		Payload: []byte(`{"kind":"` + kind + `"}`), Step: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestRosterCountsThreadsAndOpenTurns(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()

	th, err := s.NewThread(ctx, "refresh-token rotation", Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.NewThread(ctx, "the settings pane", Operator, []string{amiran, demetre})
	if err != nil {
		t.Fatal(err)
	}
	// Archived work is not work anyone is carrying, so it is not counted.
	archived, err := s.NewThread(ctx, "last month", Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetArchived(ctx, Operator, archived.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTurn(ctx, th.ID, amiran); err != nil {
		t.Fatal(err)
	}

	byID := map[string]FleetMember{}
	roster, err := s.Roster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range roster {
		byID[m.ID] = m
	}
	if got := byID[amiran]; got.Threads != 2 || !got.Working {
		t.Errorf("amiran: threads=%d working=%v, want 2 and working", got.Threads, got.Working)
	}
	if got := byID[demetre]; got.Threads != 1 || got.Working {
		t.Errorf("demetre: threads=%d working=%v, want 1 and idle", got.Threads, got.Working)
	}
	// The operator opened all three, and one of them is archived.
	if got := byID[Operator]; got.Threads != 2 || got.Working {
		t.Errorf("operator: threads=%d working=%v, want 2 and idle", got.Threads, got.Working)
	}
	if got := byID[amiran]; got.Name != "amiran" || got.Role != "developer" {
		t.Errorf("the roster lost the actor's own fields: %+v", got)
	}
	_ = other
}

// The roster and the inbox read the same table, so a turn that ended must stop
// showing as working in both at once.
func TestRosterStopsWorkingWhenTheTurnEnds(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th, err := s.NewThread(ctx, "t", Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EndTurn(ctx, amiran, turn, ""); err != nil {
		t.Fatal(err)
	}
	roster, err := s.Roster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range roster {
		if m.Working {
			t.Errorf("%s still reads as working after its turn ended", m.ID)
		}
	}
}
