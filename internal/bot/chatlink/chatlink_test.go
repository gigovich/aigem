package chatlink

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/chat"
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
	msgs, err := s.Messages(t.Context(), amiran, th, 0, 10)
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

	ev, done := tr.TurnEvents(bot.ThreadID(th), operator)
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
	done("reproduced", nil)

	frames, err := s.Timeline(t.Context(), operator, th, 0, 100)
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
}

// An oversized tool result is stored beside the timeline, so a reconnect does
// not ship it and expanding the call still can.
func TestAnOversizedToolResultGoesToABlob(t *testing.T) {
	s := newStore(t)
	tr := openTransport(t, s, "amiran")
	th := thread(t, s, amiran)

	ev, done := tr.TurnEvents(bot.ThreadID(th), operator)
	body := make([]byte, chat.BlobThreshold*3)
	for i := range body {
		body[i] = 'y'
	}
	ev.OnToolEnd("c1", "bash", string(body), nil)
	done("", nil)

	frames, err := s.Timeline(t.Context(), operator, th, 0, 100)
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
