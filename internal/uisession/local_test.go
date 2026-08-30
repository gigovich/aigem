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
	"github.com/gigovich/aigem/internal/session"
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

// A fresh conversation starts from the same standing as the first one did, so
// a tool approved for the old one has to ask again.
func TestResetPolicyForgetsAlways(t *testing.T) {
	l := newSession(t)
	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	go func() { l.confirmTool("bash", nil) }()
	req := waitFor(t, ch, KindApprovalRequest)
	if err := l.Resolve(req.ID, DecisionAlways, "c"); err != nil {
		t.Fatal(err)
	}
	// While the policy stands, the same tool does not ask.
	go func() { l.confirmTool("bash", nil) }()
	time.Sleep(20 * time.Millisecond)
	if _, open := l.Pending(); open != nil {
		t.Fatal("an always-allowed tool asked again")
	}

	l.ResetPolicy()
	go func() { l.confirmTool("bash", nil) }()
	if got := waitFor(t, ch, KindApprovalRequest); got.ID == req.ID {
		t.Fatal("expected a new request after the policy was reset")
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
	// Subscribe hands each new reader the ring size as its queue cap, so setting
	// it between the two calls is how the test gives them different caps: a small
	// queue for the slow reader so the test does not have to emit thousands of
	// events, and room for the fast one so it is bounded by its own reading speed
	// rather than by the emitter's burst.
	l.mu.Lock()
	l.ringCap = cap
	l.mu.Unlock()
	slow, stopSlow, err := l.Subscribe(Client{ID: "slow"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stopSlow()
	l.mu.Lock()
	l.ringCap = total + 8
	l.mu.Unlock()
	fast, stopFast, err := l.Subscribe(Client{ID: "fast"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stopFast()

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

// ---- persistence ----

// A conversation is saved under the id its first turn minted, restores with its
// messages and its name, and a fresh one keeps the old one resumable.
func TestSaveLoadAndReset(t *testing.T) {
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
		ModelRef: func() string { return "test/model" },
		Ring:     128,
	})
	t.Cleanup(l.Close)

	ch, stop, err := l.Subscribe(Client{ID: "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if got := l.Meta().ID; got != "" {
		t.Fatalf("a session with no turns has id %q; it should not have one yet", got)
	}
	if err := l.Submit("remember this", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, KindTurnEnd)

	meta := l.Meta()
	if meta.ID == "" || meta.Title != "remember this" || meta.Model != "test/model" {
		t.Fatalf("meta after the first turn = %+v", meta)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}

	// A fresh conversation saves the old one on the way out and keeps nothing.
	if err := l.Reset(); err != nil {
		t.Fatal(err)
	}
	if got := l.Meta(); got.ID != "" || got.Title != "" {
		t.Fatalf("meta after Reset = %+v, want empty", got)
	}

	restored, err := l.Load(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Title != "remember this" || len(restored.Messages) == 0 {
		t.Fatalf("restored %+v, want the saved conversation", restored.Meta)
	}
	if got := l.Meta().ID; got != meta.ID {
		t.Fatalf("session id after Load = %q, want %q", got, meta.ID)
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
	// A turn ends by saving the conversation, so a test that runs one and does
	// not point the state directory somewhere of its own writes into the
	// developer's real ~/.local/state/aigem.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
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
	t.Setenv("XDG_STATE_HOME", t.TempDir())
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

// Subscribing after a burst must deliver the whole backlog before the tail, in
// order and without dropping the front of it. The queue is handed to the pump
// as a slice and appended to concurrently, which is exactly the shape that
// loses its head if the two disagree about where it starts.
func TestSubscribeDeliversWholeBacklog(t *testing.T) {
	l := newSession(t)
	const n = 15
	for range n {
		l.emit(Event{Kind: KindNotice})
	}
	ch, stop, err := l.Subscribe(Client{ID: "late"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	l.emit(Event{Kind: KindNotice})

	var got []uint64
	for len(got) < n+2 { // the backlog, the presence on subscribe, and the tail
		got = append(got, recv(t, ch).Seq)
	}
	for i, seq := range got {
		if seq != uint64(i+1) {
			t.Fatalf("delivered %v; the stream must start at 1 and be contiguous", got)
		}
	}
}

// A message that is only images still names the conversation after what was
// sent, rather than after the empty string that came with it.
func TestSubmitTitleForImagesOnly(t *testing.T) {
	for _, c := range []struct {
		text   string
		images int
		want   string
	}{
		{"", 1, "1 image"},
		{"  ", 3, "3 images"},
		{"look at this", 2, "look at this"},
		{"", 0, "(untitled)"},
	} {
		if got := submitTitle(c.text, c.images); got != c.want {
			t.Errorf("submitTitle(%q, %d) = %q, want %q", c.text, c.images, got, c.want)
		}
	}
}

// A turn outlives the event that says it ended: finishTurn emits KindTurnEnd and
// only then saves. So Close cancelling the turn and returning left a goroutine
// writing into a state directory its caller believes it is done with - a test
// whose t.TempDir cannot be removed, and outside a test a save landing after
// whatever was going to replace it.
//
// The turn here is a closure that does not stop the instant its context is
// cancelled - a skill midway through a shell command. An ordinary turn returns
// on cancel promptly, which leaves only the save in the window and is why this
// went unnoticed as an occasional cleanup failure rather than as a bug.
func TestCloseWaitsForTheTurnItCancelled(t *testing.T) {
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
		Ring: 64,
	})

	release := make(chan struct{})
	if err := l.Run("skill", "skill", func(context.Context, agent.Events) (string, error) {
		<-release
		return "done", nil
	}); err != nil {
		t.Fatal(err)
	}

	closed := make(chan struct{})
	go func() { l.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a turn was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned after the turn was released")
	}

	// The save at the end of the turn is what Close was waiting for, so it has
	// already landed by the time this runs. Without the wait it is a coin flip,
	// and the losing side is a write into a directory nobody is watching.
	metas, err := session.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("the conversation was not saved before Close returned: %+v", metas)
	}
}

// The ordering inside Close is load-bearing, and the wait is the last step for
// a reason: a turn that parks *after* failPendingLocked has run is released by
// close(l.done), so waiting before that closes would leave it parked and Close
// would sit out its whole bound. The bound means the wrong order is a pause
// rather than a hang, which is exactly why this asserts on how long it took.
func TestCloseReleasesATurnThatParksWhileItIsClosing(t *testing.T) {
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
		Ring: 64,
	})

	parked := make(chan struct{})
	if err := l.Run("skill", "skill", func(context.Context, agent.Events) (string, error) {
		close(parked)
		// The shape an approval parks in: an answer, or the session ending.
		select {
		case <-l.done:
		case <-time.After(30 * time.Second):
			return "", errors.New("never released")
		}
		return "done", nil
	}); err != nil {
		t.Fatal(err)
	}
	<-parked

	start := time.Now()
	l.Close()
	if elapsed := time.Since(start); elapsed > closeWait/2 {
		t.Errorf("Close took %s: it waited for a turn it had not released yet", elapsed)
	}
}

// Every caller of Close waits, not only the first. A second one returning early
// would hand its caller the guarantee without the wait that makes it true.
func TestASecondCloseWaitsToo(t *testing.T) {
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
		Ring: 64,
	})

	release := make(chan struct{})
	if err := l.Run("skill", "skill", func(context.Context, agent.Events) (string, error) {
		<-release
		return "done", nil
	}); err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan struct{})
	go func() { l.Close(); close(firstDone) }()
	// The first Close is inside its wait; the second must join it rather than
	// walk past.
	time.Sleep(50 * time.Millisecond)
	secondDone := make(chan struct{})
	go func() { l.Close(); close(secondDone) }()
	select {
	case <-secondDone:
		t.Fatal("a second Close returned while the first was still waiting for the turn")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	for _, c := range []chan struct{}{firstDone, secondDone} {
		select {
		case <-c:
		case <-time.After(10 * time.Second):
			t.Fatal("a Close never returned after the turn was released")
		}
	}
}

// The bound itself. A turn parked on something no cancellation reaches would
// otherwise hold the process on its way out, with the terminal already gone and
// nothing left to say why.
func TestCloseGivesUpOnATurnThatWillNotUnwind(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	prev := closeWait
	closeWait = 100 * time.Millisecond
	t.Cleanup(func() { closeWait = prev })

	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := New(Config{
		Tools: reg,
		NewAgent: func(confirm agent.ConfirmFunc) *agent.Agent {
			return agent.New(&scriptedClient{}, reg, 0.3, confirm, "")
		},
		Ring: 64,
	})

	stuck := make(chan struct{})
	// Released at the end so the goroutine does not outlive the test.
	t.Cleanup(func() { close(stuck) })
	if err := l.Run("skill", "skill", func(context.Context, agent.Events) (string, error) {
		<-stuck
		return "done", nil
	}); err != nil {
		t.Fatal(err)
	}

	// On a deadline rather than inline: without the bound this does not take too
	// long, it never returns, and a test that hangs reports the defect as the
	// package timing out ten minutes later.
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		l.Close()
		done <- time.Since(start)
	}()
	select {
	case elapsed := <-done:
		if elapsed > 2*time.Second {
			t.Errorf("Close waited %s for a turn that never unwinds", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close never gave up on a turn that never unwinds")
	}
}
