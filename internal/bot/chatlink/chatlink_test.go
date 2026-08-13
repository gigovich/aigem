package chatlink

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/chat"
	"github.com/gigovich/aigem/internal/llm"
)

const (
	operator = chat.Operator
	amiran   = "bot:amiran"
	demetre  = "bot:demetre"
)

func newStore(t *testing.T) *chat.Store {
	t.Helper()
	s, err := chat.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, a := range []chat.Actor{
		{ID: operator, Name: "operator"},
		{ID: amiran, Name: "amiran", Role: "developer"},
		{ID: demetre, Name: "demetre", Role: "tester"},
	} {
		if err := s.PutActor(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func openTransport(t *testing.T, s *chat.Store, name string) *Transport {
	t.Helper()
	tr := Open(s, name, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func thread(t *testing.T, s *chat.Store, with ...string) string {
	t.Helper()
	th, err := s.NewThread(t.Context(), "retries", operator, with)
	if err != nil {
		t.Fatal(err)
	}
	return th.ID
}

// next reads one inbound, or fails. The debouncer's quiet period is 45s, so a
// test that has to wait it out would be a test nobody runs; everything here
// asserts on the immediate path or on the absence of one.
func next(t *testing.T, tr *Transport) bot.Inbound {
	t.Helper()
	select {
	case in := <-tr.Events():
		return in
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived")
		return bot.Inbound{}
	}
}

func nothing(t *testing.T, tr *Transport) {
	t.Helper()
	select {
	case in := <-tr.Events():
		t.Fatalf("something arrived that should not have: %+v", in)
	case <-time.After(200 * time.Millisecond):
	}
}

// A person alone with one bot is a conversation, and making it wait out a quiet
// period would put 45 seconds into every exchange.
func TestAPersonAloneWithOneBotIsAddressingIt(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	if _, err := s.Say(t.Context(), th, chat.Draft{Author: operator, Body: "the logout is back"}); err != nil {
		t.Fatal(err)
	}
	in := next(t, tr)
	if in.Kind != "mention" || in.Thread != bot.ThreadID(th) || in.Text != "the logout is back" {
		t.Fatalf("got %+v", in)
	}
	if in.MessageSeq == 0 {
		t.Fatal("the inbound carries no message sequence, so it cannot be deduplicated")
	}
}

// The gap this closes: with several bots in a thread, nothing in the product
// produced a mention, so every message from the operator waited out the quiet
// period and arrived with no text.
func TestNamingABotInTheTextAddressesIt(t *testing.T) {
	s := newStore(t)
	amiranT := openTransport(t, s, "amiran")
	demetreT := openTransport(t, s, "demetre")
	th := thread(t, s, amiran, demetre)

	if _, err := s.Say(t.Context(), th,
		chat.Draft{Author: operator, Body: "@amiran please look at the retries"}); err != nil {
		t.Fatal(err)
	}
	in := next(t, amiranT)
	if in.Kind != "mention" {
		t.Fatalf("the named bot got %q, want mention", in.Kind)
	}
	// The other bot is not addressed, so it decides for itself later rather than
	// answering a question that was not put to it.
	nothing(t, demetreT)
}

// Without this rule two bots in a thread answer each other forever.
func TestABotIgnoresItsOwnMessages(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	if err := tr.Reply(bot.ThreadID(th), "reproduced"); err != nil {
		t.Fatal(err)
	}
	nothing(t, tr)
}

// A membership note is the store describing itself. It is in the transcript for
// the record, not to be answered.
func TestASystemMessageWakesNobody(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	if err := s.AddParticipant(t.Context(), operator, th, demetre); err != nil {
		t.Fatal(err)
	}
	nothing(t, tr)
}

// Participation is the boundary, and it has to hold on the live path too.
func TestABotHearsNothingFromAThreadItIsNotIn(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	other := thread(t, s, demetre)

	if _, err := s.Say(t.Context(), other, chat.Draft{Author: operator, Body: "not for you"}); err != nil {
		t.Fatal(err)
	}
	nothing(t, tr)
}

// A restarted bot must replace its own registration. Appending would leave the
// dead transport on the write path for the life of the process, one more copy
// per restart.
func TestARestartedTransportReplacesItsPredecessor(t *testing.T) {
	s := newStore(t)
	first := Open(s, "amiran", slog.New(slog.DiscardHandler))
	th := thread(t, s, amiran)

	second := openTransport(t, s, "amiran")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Say(t.Context(), th, chat.Draft{Author: operator, Body: "still listening?"}); err != nil {
		t.Fatal(err)
	}
	in := next(t, second)
	if in.Text != "still listening?" {
		t.Fatalf("the live transport got %+v", in)
	}
	// The closed one delivers nothing, and its stream has ended.
	select {
	case _, ok := <-first.Events():
		if ok {
			t.Fatal("a closed transport still delivered a message")
		}
	case <-time.After(time.Second):
		t.Fatal("a closed transport's stream never ended")
	}
}

// Closing must not deadlock against the debouncer or double-close the stream.
func TestCloseIsIdempotentAndEndsTheStream(t *testing.T) {
	s := newStore(t)
	tr := Open(s, "amiran", slog.New(slog.DiscardHandler))
	th := thread(t, s, amiran, demetre)

	// An unaddressed message parks a timer in the debouncer; closing has to wait
	// it out rather than race it.
	if _, err := s.Say(t.Context(), th, chat.Draft{Author: demetre, Body: "thinking out loud"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tr.Close()
		_ = tr.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked")
	}
	for range tr.Events() { //nolint:revive // draining until the stream ends is the assertion
	}
}

// The store rejects a body past its cap, and the old transport split into
// chunks. Losing the answer entirely is the one outcome a human cannot see.
func TestAnOversizedReplyIsTruncatedRatherThanLost(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	huge := make([]byte, chat.MaxBodyBytes+4096)
	for i := range huge {
		huge[i] = 'x'
	}
	if err := tr.Reply(bot.ThreadID(th), string(huge)); err != nil {
		t.Fatalf("an oversized reply was lost: %v", err)
	}
	msgs, _, _, err := s.Messages(t.Context(), amiran, th, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the thread holds %d messages, want the truncated reply", len(msgs))
	}
	if len(msgs[0].Body) > chat.MaxBodyBytes {
		t.Fatalf("the stored body is %d bytes, past the cap", len(msgs[0].Body))
	}
	if !contains(msgs[0].Body, "truncated") {
		t.Fatal("the reply was cut without saying so")
	}
}

// The timeline is the point of the migration: a bot's steps have to reach the
// operator, not only the log.
func TestATurnRecordsItsStepsIntoTheThread(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	ev, spend, done := tr.TurnEvents(bot.ThreadID(th), operator)
	// While the turn is open the thread reads as working, which is what replaced
	// the typing indicator.
	v, err := s.ThreadFor(t.Context(), operator, th)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Working {
		t.Fatal("a thread with an open turn does not report working")
	}

	ev.OnToolStart("c1", "bash", []byte(`{"cmd":"go test ./..."}`))
	ev.OnToolEnd("c1", "bash", "FAIL", nil)
	// Per-delta content is deliberately not stored: one transaction per streamed
	// chunk would stall the single writer the whole fleet queues behind.
	ev.OnContent("re")
	ev.OnContent("produced")
	ev.OnAssistantMessage("reproduced")
	// A call the provider reported nothing for is still a call: an attempt that
	// streamed partway and then broke arrives exactly like this.
	spend(llm.Usage{InputTokens: 100, CachedTokens: 40, OutputTokens: 10}, "xai/grok-4.3")
	spend(llm.Usage{InputTokens: 200, OutputTokens: 20}, "xai/grok-4.3")
	spend(llm.Usage{}, "xai/grok-4.3")
	done("reproduced", nil)

	frames, _, _, err := s.Timeline(t.Context(), operator, th, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, f := range frames {
		var ev struct {
			Kind string `json:"kind"`
		}
		if err := unmarshal(f.Event, &ev); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, ev.Kind)
	}
	for _, want := range []string{"turn_start", "tool_start", "tool_end", "assistant_message", "turn_end"} {
		if !containsAll(kinds, want) {
			t.Fatalf("the timeline is %v, missing %q", kinds, want)
		}
	}
	for _, unwanted := range []string{"content", "reasoning"} {
		if containsAll(kinds, unwanted) {
			t.Fatalf("the timeline stored per-delta %q; that is one write per chunk", unwanted)
		}
	}

	v, err = s.ThreadFor(t.Context(), operator, th)
	if err != nil {
		t.Fatal(err)
	}
	if v.Working {
		t.Fatal("the thread still reports working after the turn ended")
	}

	turns, _, _, err := s.Turns(t.Context(), operator, th, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	want := chat.Usage{InputTokens: 300, CachedTokens: 40, OutputTokens: 30, Calls: 3, Uncounted: 1}
	if turns[0].Usage != want {
		t.Fatalf("turn usage = %+v, want %+v", turns[0].Usage, want)
	}
	if turns[0].Model != "xai/grok-4.3" {
		t.Fatalf("turn model = %q", turns[0].Model)
	}
}

// The write is batched off the goroutine holding the provider's response, so a
// long turn's row would otherwise sit at zero for the whole run - which for a
// developer bot is up to 120 model rounds.
func TestALongTurnsSpendIsRecordedBeforeItEnds(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	_, spend, done := tr.TurnEvents(bot.ThreadID(th), operator)
	defer done("", nil)
	for range usageFlushEvery {
		spend(llm.Usage{InputTokens: 10, OutputTokens: 1}, "xai/grok-4.3")
	}

	turns, _, _, err := s.Turns(t.Context(), operator, th, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := chat.Usage{InputTokens: 10 * usageFlushEvery, OutputTokens: usageFlushEvery,
		Calls: usageFlushEvery}
	if len(turns) != 1 || turns[0].Usage != want {
		t.Fatalf("a turn still running reports %+v, want %+v", turns, want)
	}
}

// A call that names no model must not blank the one the batch already had: the
// store keeps the last non-empty value, but only among what reaches it, and a
// batch flushed with an empty model lands with none at all.
func TestACallWithNoModelDoesNotEraseTheTurnsModel(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	_, spend, done := tr.TurnEvents(bot.ThreadID(th), operator)
	spend(llm.Usage{InputTokens: 10, OutputTokens: 1}, "xai/grok-4.3")
	spend(llm.Usage{InputTokens: 10, OutputTokens: 1}, "")
	done("", nil)

	turns, _, _, err := s.Turns(t.Context(), operator, th, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Model != "xai/grok-4.3" {
		t.Fatalf("turn model = %+v, want xai/grok-4.3", turns)
	}
}

// Parallel subagents run on the one client, so several calls finish at once and
// the accumulator is written from all of them.
func TestConcurrentCallsDoNotLoseSpend(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	_, spend, done := tr.TurnEvents(bot.ThreadID(th), operator)
	const workers, each = 8, 20
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				spend(llm.Usage{InputTokens: 1, OutputTokens: 1}, "xai/grok-4.3")
			}
		}()
	}
	wg.Wait()
	done("", nil)

	turns, _, _, err := s.Turns(t.Context(), operator, th, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := chat.Usage{InputTokens: workers * each, OutputTokens: workers * each,
		Calls: workers * each}
	if len(turns) != 1 || turns[0].Usage != want {
		t.Fatalf("usage = %+v, want %+v", turns, want)
	}
}

// An oversized tool result is stored beside the timeline, so a reconnect does
// not ship it and expanding the call still can.
func TestAnOversizedToolResultGoesToABlob(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	ev, _, done := tr.TurnEvents(bot.ThreadID(th), operator)
	body := make([]byte, chat.BlobThreshold*3)
	for i := range body {
		body[i] = 'y'
	}
	ev.OnToolEnd("c1", "bash", string(body), nil)
	done("", nil)

	frames, _, _, err := s.Timeline(t.Context(), operator, th, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var seq uint64
	for _, f := range frames {
		var e struct {
			Kind  string `json:"kind"`
			Blob  bool   `json:"blob"`
			Bytes int    `json:"bytes"`
			Text  string `json:"text"`
		}
		if err := unmarshal(f.Event, &e); err != nil {
			t.Fatal(err)
		}
		if e.Kind != "tool_end" {
			continue
		}
		if !e.Blob || e.Bytes != len(body) || len(e.Text) != chat.BlobThreshold {
			t.Fatalf("the stored event does not point at a blob: %+v", e)
		}
		seq = f.Seq
	}
	if seq == 0 {
		t.Fatal("no tool_end was stored")
	}
	got, err := s.Blob(t.Context(), operator, th, seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(body) {
		t.Fatalf("the blob holds %d bytes, want %d", len(got), len(body))
	}
}

func TestActorForNamesAndTheOperator(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")

	for _, tc := range []struct{ in, want string }{
		{"demetre", demetre},
		{"@demetre", demetre},
		{"DEMETRE", demetre},
		{"operator", operator},
		{"you", operator},
		{"ghost", ""},
		{"", ""},
	} {
		if got := tr.ActorFor(tc.in); got != tc.want {
			t.Errorf("ActorFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func containsAll(list []string, want string) bool { return slices.Contains(list, want) }

func unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// The trace and the answer it produced have to be one fact, not two adjacent
// ones. Without the stamp the browser would have to guess which run a message
// came out of by bracketing it between a turn's start and end - which a bot
// that posts twice in a turn, or a turn killed with the process, both defeat.
func TestATurnsMessagesAreFiledUnderIt(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	_, _, done := tr.TurnEvents(bot.ThreadID(th), operator)
	if err := tr.Reply(bot.ThreadID(th), "reproduced on staging"); err != nil {
		t.Fatal(err)
	}
	done("reproduced on staging", nil)

	msgs, _, _, err := s.Messages(t.Context(), operator, th, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want the answer", len(msgs))
	}
	if msgs[0].Turn == 0 {
		t.Fatal("the answer is not filed under the turn that produced it")
	}

	// A tool's own post carries no turn, even inside a live run and even in the
	// run's own thread. A bot works up to four threads at once, so post_message
	// from one run can land in another where this bot also has a turn open, and
	// stamping by thread would hang that run's trace under a note it did not
	// produce.
	_, _, done2 := tr.TurnEvents(bot.ThreadID(th), operator)
	defer done2("", nil)
	if _, err := tr.Say(t.Context(), bot.ThreadID(th), "an aside", bot.SayOpts{}); err != nil {
		t.Fatal(err)
	}
	msgs, _, _, err = s.Messages(t.Context(), operator, th, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].Turn != 0 {
		t.Fatalf("a tool's post was filed under a run: turn %d", msgs[0].Turn)
	}
}

func TestAFileChangedInATurnBecomesADiffAndAStep(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	_, _, done := tr.TurnEvents(bot.ThreadID(th), operator)
	tr.FileChanged(bot.ThreadID(th), chat.Artifact{
		Path: "internal/auth/flow.go", Old: "before\n", New: "middle\n",
	})
	// A second edit to the same file keeps the content from before the turn
	// touched it, so the diff is the turn's whole effect rather than its last
	// keystroke - and counts once.
	tr.FileChanged(bot.ThreadID(th), chat.Artifact{
		Path: "internal/auth/flow.go", Old: "middle\n", New: "after\n",
	})
	done("", nil)

	got, err := s.Artifacts(t.Context(), operator, th, 0, "internal/auth/flow.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d artifacts, want the one file", len(got))
	}
	if got[0].Old != "before\n" || got[0].New != "after\n" {
		t.Fatalf("diff = %q -> %q, want the turn's whole effect", got[0].Old, got[0].New)
	}

	turns, _, _, err := s.Turns(t.Context(), operator, th, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].Files != 1 {
		t.Fatalf("turn files = %d, want 1", turns[0].Files)
	}
	// The panel's figure and the trace's have to come from the same events, so
	// the write also lands in the timeline.
	frames, _, _, err := s.Timeline(t.Context(), operator, th, 0, turns[0].Seq, 100)
	if err != nil {
		t.Fatal(err)
	}
	var changes int
	for _, f := range frames {
		if strings.Contains(string(f.Event), `"file_changed"`) {
			changes++
		}
	}
	if changes != 2 {
		t.Fatalf("the timeline has %d file_changed steps, want both edits", changes)
	}
}

// A step announcing a diff the panel has no row for is how the summary line and
// the file list come to disagree: the line counts these events, and it would
// climb past the cap the list stops at while the panel stopped listing.
func TestAFileTheStoreDidNotKeepIsNotAStep(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	_, _, done := tr.TurnEvents(bot.ThreadID(th), operator)
	defer done("", nil)
	for i := range chat.MaxTurnArtifacts + 5 {
		tr.FileChanged(bot.ThreadID(th), chat.Artifact{
			Path: fmt.Sprintf("generated/file%03d.go", i), New: "x",
		})
	}

	turns, _, _, err := s.Turns(t.Context(), operator, th, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].Files != chat.MaxTurnArtifacts {
		t.Fatalf("turn files = %d, want the cap", turns[0].Files)
	}
	frames, _, _, err := s.Timeline(t.Context(), operator, th, 0, turns[0].Seq, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var changes int
	for _, f := range frames {
		if strings.Contains(string(f.Event), `"file_changed"`) {
			changes++
		}
	}
	// One per file the store kept, and not one per file the bot wrote: the
	// panel can only ever list the former.
	if changes != chat.MaxTurnArtifacts {
		t.Fatalf("the timeline has %d file_changed steps, want %d - the ones with a row",
			changes, chat.MaxTurnArtifacts)
	}
}

// A cron job runs against no conversation, so there is no turn for its writes
// to hang off. Dropping them is right; attributing them to whatever ran last in
// some thread is not.
func TestAFileChangedOutsideATurnIsDropped(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	tr.FileChanged(bot.ThreadID(th), chat.Artifact{Path: "internal/auth/flow.go", New: "x"})

	got, err := s.Artifacts(t.Context(), operator, th, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d artifacts with no turn open, want none", len(got))
	}
	// And nothing reached the timeline either. Asserting only on the artifacts
	// could not tell "dropped before the write" from "one rejected write and a
	// warning per file, for every cron run this bot ever makes".
	frames, _, _, err := s.Timeline(t.Context(), operator, th, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("a change outside a run reached the timeline: %d events", len(frames))
	}
}

// closeTurn forgets a run only if it is still the current one. A superseded
// turn clearing a later run's entry would leave that run's answer unstamped,
// and its trace attached to nothing.
func TestClosingASupersededTurnLeavesTheCurrentOneStamped(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	_, _, doneFirst := tr.TurnEvents(bot.ThreadID(th), operator)
	_, _, doneSecond := tr.TurnEvents(bot.ThreadID(th), operator)
	// Out of order on purpose: the first run finishing after the second began.
	doneFirst("", nil)

	if err := tr.Reply(bot.ThreadID(th), "still mine"); err != nil {
		t.Fatal(err)
	}
	doneSecond("", nil)

	msgs, _, _, err := s.Messages(t.Context(), operator, th, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].Turn == 0 {
		t.Fatal("the live run's answer lost its turn when an older one closed")
	}
}

// The summary line is the point of the whole migration, so what it counts is
// asserted rather than assumed. The turn's own brackets are not steps.
func TestATurnCountsItsStepsAndTools(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	ev, _, done := tr.TurnEvents(bot.ThreadID(th), operator)
	ev.OnToolStart("c1", "grep", json.RawMessage(`{"pattern":"Refresh("}`))
	ev.OnToolEnd("c1", "grep", "internal/auth/flow.go:88", nil)
	// The plan write is a step but not a tool: the browser draws those calls as
	// a plan rather than as a tool card, so counting one promises a card the
	// trace does not hold.
	ev.OnToolStart("c2", planTool, json.RawMessage(`{"todos":[]}`))
	ev.OnToolEnd("c2", planTool, "ok", nil)
	ev.OnAssistantMessage("looking")
	done("done", nil)

	turns, _, _, err := s.Turns(t.Context(), operator, th, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].Tools != 1 {
		t.Fatalf("turn tools = %d, want only the grep", turns[0].Tools)
	}
	// Three: the two tool calls and the assistant message that said something.
	// A step is a row the expanded trace draws, so the ends that complete a row
	// rather than add one do not count, the run's own brackets do not, and the
	// answer turn_end carries is drawn as the message the trace hangs under.
	if turns[0].Steps != 3 {
		t.Fatalf("turn steps = %d, want 3", turns[0].Steps)
	}
}

// The agent announces an assistant message before every tool batch, and on a
// round that produced tool calls and no prose it carries no text and the
// renderer draws nothing for it. Counting those announced two steps per round
// for a turn that took one.
func TestAnEmptyAssistantMessageIsNotAStep(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	ev, _, done := tr.TurnEvents(bot.ThreadID(th), operator)
	for i := range 3 {
		ev.OnAssistantMessage("")
		id := fmt.Sprintf("c%d", i)
		ev.OnToolStart(id, "grep", json.RawMessage(`{}`))
		ev.OnToolEnd(id, "grep", "ok", nil)
	}
	done("Reproduced.", nil)

	turns, _, _, err := s.Turns(t.Context(), operator, th, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].Steps != 3 {
		t.Fatalf("turn steps = %d, want the three tool calls alone", turns[0].Steps)
	}
}

// The plan panel reads the turn rather than replaying the timeline, so the
// write has to reach the row.
func TestAPlanWrittenInATurnIsStoredOnIt(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	ev, _, done := tr.TurnEvents(bot.ThreadID(th), operator)
	ev.OnTodoUpdate([]agent.TodoItem{
		{Text: "reproduce on staging", Status: agent.TodoCompleted},
		{Text: "patch internal/auth", Status: agent.TodoInProgress},
	})
	done("", nil)

	turns, _, _, err := s.Turns(t.Context(), operator, th, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var plan []agent.TodoItem
	if err := json.Unmarshal(turns[0].Plan, &plan); err != nil {
		t.Fatalf("plan = %q: %v", turns[0].Plan, err)
	}
	if len(plan) != 2 || plan[1].Status != "in_progress" {
		t.Fatalf("plan = %+v, want the two steps as written", plan)
	}
}
