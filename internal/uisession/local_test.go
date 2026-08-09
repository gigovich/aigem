package uisession

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
)

// recv takes one event, failing the test rather than hanging if none arrives.
func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return Event{}
}

// waitFor takes events until one matches, so a test can assert on a kind
// without listing every event that legitimately precedes it.
func waitFor(t *testing.T, ch <-chan Event, kind Kind) Event {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed before a %s event", kind)
			}
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s event", kind)
		}
	}
}

func newSession(t *testing.T) *Local {
	t.Helper()
	l := New(Config{Ring: 64})
	t.Cleanup(l.Close)
	return l
}

// Two front-ends can answer the same question at the same time. The first
// answer has to stand, the second has to be told so rather than silently
// overwriting a decision the tool has already acted on, and both have to learn
// the outcome from the stream.
func TestResolveFirstAnswerWins(t *testing.T) {
	l := newSession(t)
	laptop, stopA, err := l.Subscribe(Client{ID: "laptop", Kind: "tui"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stopA()
	phone, stopB, err := l.Subscribe(Client{ID: "phone", Kind: "web"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stopB()

	answered := make(chan bool, 1)
	go func() { answered <- l.confirmTool("bash", json.RawMessage(`{"cmd":"ls"}`)) }()

	req := waitFor(t, laptop, KindApprovalRequest)
	if req.Approval == nil || req.Approval.Tool != "bash" {
		t.Fatalf("unexpected request: %+v", req)
	}
	waitFor(t, phone, KindApprovalRequest)

	if err := l.Resolve(req.ID, DecisionOnce, "phone"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if err := l.Resolve(req.ID, DecisionDeny, "laptop"); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second Resolve: err = %v, want ErrAlreadyDecided", err)
	}

	select {
	case ok := <-answered:
		if !ok {
			t.Fatal("the tool call was denied; the first answer allowed it")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the tool call was never unparked")
	}

	// Both clients, including the one whose answer lost, learn who decided.
	for name, ch := range map[string]<-chan Event{"laptop": laptop, "phone": phone} {
		ev := waitFor(t, ch, KindApprovalResolved)
		if ev.ID != req.ID || ev.Decision != DecisionOnce || ev.By != "phone" {
			t.Errorf("%s saw %+v, want id=%s decision=once by=phone", name, ev, req.ID)
		}
	}
}

// The refusal on a write outside the working directory is the last option and
// "Always" is not offered, because the sandbox never remembers a write. A client
// that sends one anyway must be refused rather than quietly granted a directory.
func TestResolveRejectsUnofferedDecision(t *testing.T) {
	l := newSession(t)
	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	decided := make(chan tools.PathDecision, 1)
	go func() {
		decided <- l.approvePath("/etc/hosts", tools.PathIntent{Tool: "write_file", Write: true})
	}()

	req := waitFor(t, ch, KindApprovalRequest)
	if got := len(req.Approval.Options); got != 2 {
		t.Fatalf("write request offered %d options, want 2 (no remembered grant)", got)
	}
	if err := l.Resolve(req.ID, DecisionAlways, "c"); !errors.Is(err, ErrBadDecision) {
		t.Fatalf("Resolve(always) = %v, want ErrBadDecision", err)
	}
	if err := l.Resolve(req.ID, DecisionDeny, "c"); err != nil {
		t.Fatal(err)
	}
	if d := <-decided; d != tools.PathDeny {
		t.Fatalf("decision = %v, want PathDeny", d)
	}
}

// Concurrent subagents ask at once. One question is shown at a time, and an
// "Always" answers the ones still queued for that tool instead of asking again.
func TestQueuedRequestsSettledByPolicy(t *testing.T) {
	l := newSession(t)
	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	const n = 3
	results := make(chan bool, n)
	go func() { results <- l.confirmTool("bash", json.RawMessage(`{"cmd":"a"}`)) }()
	req := waitFor(t, ch, KindApprovalRequest)
	// Only once the first request is open are the rest guaranteed to queue
	// behind it rather than race for the open slot.
	for i := 1; i < n; i++ {
		go func() { results <- l.confirmTool("bash", json.RawMessage(`{"cmd":"b"}`)) }()
	}
	// Give the queued calls time to arrive; they must not produce a second
	// dialog, which the assertion after the answer checks.
	time.Sleep(50 * time.Millisecond)

	if err := l.Resolve(req.ID, DecisionAlways, "c"); err != nil {
		t.Fatal(err)
	}
	for range n {
		select {
		case ok := <-results:
			if !ok {
				t.Fatal("a call was denied after Always")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a queued call was never settled by the session policy")
		}
	}
	if id, req := l.Pending(); req != nil {
		t.Fatalf("request %s still open; Always should have settled the queue", id)
	}
}

// A tool call parked on an approval must not outlive the session, or closing a
// session would leak a goroutine holding the agent's turn open.
func TestCloseRefusesPendingApprovals(t *testing.T) {
	l := New(Config{Ring: 16})
	answered := make(chan bool, 1)
	go func() { answered <- l.confirmTool("bash", nil) }()

	deadline := time.After(3 * time.Second)
	for {
		if _, req := l.Pending(); req != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("request never opened")
		case <-time.After(time.Millisecond):
		}
	}
	l.Close()
	select {
	case ok := <-answered:
		if ok {
			t.Fatal("a call parked at close was allowed; it must be refused")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("closing the session left a tool call parked forever")
	}
}

// A reader that stops reading must not stall the fan-out for anyone else. It is
// dropped with a marker telling it where to resume, and everything queued below
// that point is still delivered, so reconnecting leaves no hole.
func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	l := newSession(t)
	const (
		cap   = 4
		total = 40
	)
	slow, stopSlow, err := l.Subscribe(Client{ID: "slow"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stopSlow()
	fast, stopFast, err := l.Subscribe(Client{ID: "fast"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stopFast()
	// Shrink the slow reader's queue so the test does not have to emit thousands
	// of events, and give the fast one room so it is bounded by its own reading
	// speed rather than by the emitter's burst.
	l.mu.Lock()
	l.subs["slow"].cap = cap
	l.subs["fast"].cap = total + 8
	l.mu.Unlock()

	done := make(chan struct{})
	go func() {
		for i := range total {
			l.emit(Event{Kind: KindNotice, Text: strings.Repeat("x", i%3)})
		}
		close(done)
	}()

	// The fast reader keeps up, which is the real assertion: emit never blocked
	// on the reader that stopped.
	seen := 0
	for seen < total {
		ev := recv(t, fast)
		if ev.Kind == KindNotice {
			seen++
		}
		if ev.Kind == KindDesync {
			t.Fatal("the fast reader was dropped")
		}
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("emit blocked on the slow subscriber")
	}

	// The slow reader gets a contiguous prefix and then the marker.
	var last uint64
	for {
		ev := recv(t, slow)
		if ev.Kind == KindDesync {
			if ev.From != last {
				t.Fatalf("desync From = %d, want %d (the last event actually delivered)", ev.From, last)
			}
			break
		}
		if last != 0 && ev.Seq != last+1 {
			t.Fatalf("gap before the desync marker: %d then %d", last, ev.Seq)
		}
		last = ev.Seq
	}
	if _, ok := <-slow; ok {
		t.Fatal("channel stayed open after the desync marker")
	}
}

// Reconnecting with the last sequence number seen must produce exactly the
// missing events and then the live tail: no gap, no repeat.
func TestSubscribeSplicesBacklogAndTail(t *testing.T) {
	l := newSession(t)
	for i := range 5 {
		l.emit(Event{Kind: KindNotice, Text: string(rune('a' + i))})
	}

	ch, stop, err := l.Subscribe(Client{ID: "late"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	l.emit(Event{Kind: KindNotice, Text: "f"})

	var got []string
	for len(got) < 4 {
		ev := recv(t, ch)
		if ev.Kind != KindNotice {
			continue // Subscribe announces the new client; not what is under test.
		}
		got = append(got, ev.Text)
	}
	if want := "c,d,e,f"; strings.Join(got, ",") != want {
		t.Fatalf("replayed %q, want %q", strings.Join(got, ","), want)
	}
}

// A client that has been away longer than the retained history must be told to
// reload rather than handed a stream that silently starts mid-conversation.
func TestReplayTruncated(t *testing.T) {
	l := New(Config{Ring: 4})
	t.Cleanup(l.Close)
	for range 10 {
		l.emit(Event{Kind: KindNotice})
	}
	if _, err := l.Replay(1); !errors.Is(err, ErrTruncated) {
		t.Fatalf("Replay(1) err = %v, want ErrTruncated", err)
	}
	evs, err := l.Replay(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Seq != 9 {
		t.Fatalf("Replay(8) returned %d events starting at %d, want 2 starting at 9", len(evs), evs[0].Seq)
	}
}

// ---- turns ----

// scriptedClient answers with one tool call, then prose.
type scriptedClient struct {
	calls int
	tool  string
}

func (s *scriptedClient) Stream(_ context.Context, _ []llm.Message, toolDefs []llm.Tool, _ float64,
	onEvent func(llm.StreamEvent)) (llm.Message, error) {
	s.calls++
	if toolDefs != nil && s.calls == 1 && s.tool != "" {
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "call-1", Type: "function",
			Function: llm.FunctionCall{Name: s.tool, Arguments: `{"path":"."}`},
		}}}, nil
	}
	if onEvent != nil {
		onEvent(llm.StreamEvent{Content: "hi"})
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "hi"}, nil
}

func TestSubmitRunsATurn(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := New(Config{
		Tools: reg,
		NewAgent: func(confirm agent.ConfirmFunc) *agent.Agent {
			return agent.New(&scriptedClient{tool: "list_dir"}, reg, 0.3, confirm, "")
		},
		Ring: 128,
	})
	t.Cleanup(l.Close)

	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if err := l.Submit("what is here?", nil); err != nil {
		t.Fatal(err)
	}
	if ev := waitFor(t, ch, KindUserMessage); ev.Text != "what is here?" {
		t.Fatalf("user message = %q", ev.Text)
	}
	waitFor(t, ch, KindTurnStart)
	start := waitFor(t, ch, KindToolStart)
	if start.ID == "" || start.Name != "list_dir" {
		t.Fatalf("tool start = %+v, want a list_dir call carrying its id", start)
	}
	end := waitFor(t, ch, KindToolEnd)
	if end.ID != start.ID {
		t.Fatalf("tool end id = %q, want %q", end.ID, start.ID)
	}
	done := waitFor(t, ch, KindTurnEnd)
	if done.Error != "" || done.Text != "hi" {
		t.Fatalf("turn end = %+v, want the answer with no error", done)
	}
	if l.Running() {
		t.Fatal("session still reports a running turn")
	}
}

// A message typed while the agent is working joins the running turn instead of
// being dropped, which is what makes typing on a phone mid-turn useful.
func TestSubmitDuringTurnInjects(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	l := New(Config{
		Tools: reg,
		NewAgent: func(confirm agent.ConfirmFunc) *agent.Agent {
			return agent.New(&blockingClient{release: release}, reg, 0.3, confirm, "")
		},
		Ring: 128,
	})
	t.Cleanup(l.Close)

	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if err := l.Submit("first", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, KindTurnStart)
	if err := l.Submit("second", nil); err != nil {
		t.Fatalf("mid-turn Submit: %v", err)
	}
	ev := waitFor(t, ch, KindUserMessage)
	for ev.Text != "second" {
		ev = waitFor(t, ch, KindUserMessage)
	}
	if !ev.Injected {
		t.Fatal("the mid-turn message was not marked as injected")
	}
	close(release)
	waitFor(t, ch, KindTurnEnd)
}

// blockingClient holds the first request open until released, so a test can act
// while a turn is genuinely in flight.
type blockingClient struct {
	calls   int
	release chan struct{}
}

func (b *blockingClient) Stream(ctx context.Context, _ []llm.Message, _ []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	b.calls++
	if b.calls == 1 {
		select {
		case <-b.release:
		case <-ctx.Done():
			return llm.Message{}, ctx.Err()
		}
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "done"}, nil
}
