package bot

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/agent"
)

// fleetRuntime builds a runtime whose transport produces nothing, so only what
// the test delivers in-process reaches it.
func fleetRuntime(t *testing.T) (*Runtime, *fakeTransport) {
	t.Helper()
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	return NewRuntime(ft, func(string) Runner { return nil }, 1), ft
}

// allParticipation says yes, which is what the fleet tests are not about: they
// exercise routing, and the entitlement check has its own tests.
type allParticipation struct{}

func (allParticipation) IsParticipant(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

func TestFleetNilIsSafe(t *testing.T) {
	var f *Fleet
	f.Register(Member{Name: "a", Role: "manager"})
	f.Unregister("a")
	if f.Has("a") || f.Busy("a") || f.Deliver(context.Background(), "a", Inbound{}) || f.Roster() != nil {
		t.Fatal("a nil fleet must behave as if nobody else is running")
	}
}

func TestFleetDeliverReachesRegisteredBotOnly(t *testing.T) {
	rt, _ := fleetRuntime(t)
	f := NewFleet()
	f.Register(Member{Name: "jane", Actor: "bot:jane", Role: "tester", Runtime: rt,
		Participation: allParticipation{}})

	if !f.Has("jane") {
		t.Fatal("registered bot not on the roster")
	}
	if f.Deliver(context.Background(), "amiran", Inbound{Text: "hi"}) {
		t.Fatal("delivered to a bot that is not in this process")
	}
	if !f.Deliver(context.Background(), "jane",
		Inbound{Kind: "mention", Thread: "t_0102030405060708", Text: "hi", MessageSeq: 1}) {
		t.Fatal("delivery to a registered bot failed")
	}
	select {
	case got := <-rt.enqueued:
		if got.MessageSeq != 1 {
			t.Fatalf("message seq = %d, want 1", got.MessageSeq)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing was queued on the receiving runtime")
	}

	f.Unregister("jane")
	if f.Deliver(context.Background(), "jane", Inbound{}) {
		t.Fatal("delivered to a bot that has stopped")
	}
}

func TestFleetRosterReportsBusy(t *testing.T) {
	idle, _ := fleetRuntime(t)
	busy, _ := fleetRuntime(t)
	done := busy.EnterTurn()
	defer done()

	f := NewFleet()
	f.Register(Member{Name: "kate", Actor: "bot:" + "kate", Role: "architect", Runtime: idle, Participation: allParticipation{}})
	f.Register(Member{Name: "amiran", Actor: "bot:" + "amiran", Role: "developer", Runtime: busy, Participation: allParticipation{}})

	roster := f.Roster()
	if len(roster) != 2 {
		t.Fatalf("roster size = %d, want 2", len(roster))
	}
	if roster[0].Name != "amiran" || roster[1].Name != "kate" {
		t.Fatalf("roster is not sorted by name: %+v", roster)
	}
	if !roster[0].Busy || roster[1].Busy {
		t.Fatalf("busy flags wrong: %+v", roster)
	}
	if !f.Busy("amiran") || f.Busy("kate") {
		t.Fatal("Busy disagrees with the roster")
	}
	if f.Busy("nobody") {
		t.Fatal("a bot that is not local must not be reported busy: nothing is known about it")
	}
}

func TestRuntimeEnqueueDoesNotBlockWhenFull(t *testing.T) {
	rt, _ := fleetRuntime(t)
	for i := 0; i < cap(rt.enqueued); i++ {
		if !rt.Enqueue(Inbound{Text: "x"}) {
			t.Fatalf("queue rejected message %d while it still had room", i)
		}
	}
	done := make(chan bool, 1)
	go func() { done <- rt.Enqueue(Inbound{Text: "overflow"}) }()
	select {
	case accepted := <-done:
		if accepted {
			t.Fatal("a full queue must reject, so the caller falls back to chat")
		}
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked on a full queue; a handoff must never wait on the receiver")
	}
}

func TestRuntimeRoutesEachMessageOnce(t *testing.T) {
	rt, _ := fleetRuntime(t)
	if rt.alreadyRouted(1) {
		t.Fatal("first sighting of a message must be routed")
	}
	if !rt.alreadyRouted(1) {
		t.Fatal("the second copy of a message must be dropped")
	}
	// A resume or a thread update has no message behind it, so no identity to
	// compare - it must never be dropped.
	for range 2 {
		if rt.alreadyRouted(0) {
			t.Fatal("something with no message behind it must always be routed")
		}
	}
}

func TestRuntimeRoutedMessageMemoryIsBounded(t *testing.T) {
	rt, _ := fleetRuntime(t)
	for i := range uint64(maxSeenMsgs + 10) {
		rt.alreadyRouted(i + 1)
	}
	rt.mu.Lock()
	size := len(rt.seenMsgs)
	rt.mu.Unlock()
	if size > maxSeenMsgs {
		t.Fatalf("routed-message memory grew to %d, past the %d cap", size, maxSeenMsgs)
	}
	if !rt.alreadyRouted(uint64(maxSeenMsgs + 10)) {
		t.Fatal("the most recent message must still be remembered")
	}
}

func TestRuntimeServeRoutesEnqueuedMessages(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	runner := &fakeRunner{started: make(chan struct{}, 4), release: make(chan struct{})}
	close(runner.release)
	rt := NewRuntime(ft, func(string) Runner { return runner }, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rt.Serve(ctx) }()

	if !rt.Enqueue(Inbound{Kind: "mention", Thread: ThreadID("r1"),
		Text: "handoff from a teammate", MessageSeq: 9}) {
		t.Fatal("Enqueue rejected the delivery")
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("an in-process delivery was never served")
	}

	// The same post arriving over the websocket afterwards must not run again.
	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("r1"),
		Text: "handoff from a teammate", MessageSeq: 9}
	select {
	case <-runner.started:
		t.Fatal("the chat copy of an already-delivered post ran a second turn")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRuntimeRefusesDeliveryAfterServeStops(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 1)}
	rt := NewRuntime(ft, func(string) Runner { return nil }, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rt.Serve(ctx); close(done) }()
	cancel()
	<-done

	// Accepting here would tell the sender their teammate was woken by a runtime that has
	// stopped reading, leaving the handoff with no path at all.
	if rt.Enqueue(Inbound{Text: "too late"}) {
		t.Fatal("a stopped runtime must refuse delivery so the caller falls back to chat")
	}
}

func TestCronRunnerTakesAFleetTurnSlot(t *testing.T) {
	limiter := NewTurnLimiter(1)
	held, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{})
	run := NewCronRunner(slog.Default(), func() (Runner, error) {
		return runnerStub(func() { close(ran) }), nil
	}, nil, nil, limiter)

	go run(context.Background(), CronJob{ID: "j", Prompt: "go"})
	select {
	case <-ran:
		t.Fatal("a scheduled run started while the fleet cap was full; cron must queue like a chat turn")
	case <-time.After(100 * time.Millisecond):
	}
	held()
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the scheduled run never got its slot after one was freed")
	}
}

type runnerStub func()

func (f runnerStub) Run(_ context.Context, _ string, _ agent.Events) (string, error) {
	f()
	return "", nil
}

// TestRuntimeTurnTakesAFleetSlot covers the chat half of the fleet cap. Without it a
// SetTurnLimiter that silently failed to apply would be invisible, and the fleet would run at
// its per-bot ceiling multiplied by the number of bots - the state the cap exists to prevent.
func TestRuntimeTurnTakesAFleetSlot(t *testing.T) {
	limiter := NewTurnLimiter(1)
	held, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ft := &fakeTransport{in: make(chan Inbound, 1)}
	runner := &fakeRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	close(runner.release)
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)
	rt.SetTurnLimiter(limiter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rt.Serve(ctx) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("c1"), Text: "hi"}
	select {
	case <-runner.started:
		t.Fatal("a chat turn ran while the fleet cap was full")
	case <-time.After(100 * time.Millisecond):
	}
	held()
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the turn never got its slot after one was freed")
	}
}

// TestPanicInATurnDoesNotStopTheBot is the containment the docs promise: bots share a process
// now, so an unrecovered panic in one turn would take every bot down with it.
func TestPanicInATurnDoesNotStopTheBot(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 2)}
	ran := make(chan string, 2)
	rt := NewRuntime(ft, func(string) Runner {
		return panickingRunner{ran: ran}
	}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() { _ = rt.Serve(ctx); close(served) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("c1"), Text: "boom"}
	<-ran
	select {
	case <-served:
		t.Fatal("a panicking turn stopped the whole bot")
	case <-time.After(100 * time.Millisecond):
	}

	// The same thread must still be served: a panic that walked past the lock release would
	// wedge this conversation forever and pin a thread the cap can never evict.
	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("c1"), Text: "again"}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the thread stayed locked after a panicking turn")
	}
	rt.mu.Lock()
	st := rt.threads["c1"]
	rt.mu.Unlock()
	if st == nil {
		t.Fatal("the panicking thread lost its state")
	}
	// A held lock also makes the thread permanently unevictable, so wait for the second turn to
	// give it back rather than only proving the turn started.
	deadline := time.Now().Add(2 * time.Second)
	for len(st.lock) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the thread lock was still held after the turns finished")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Another thread must still be served too.
	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("c2"), Text: "again"}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the bot stopped serving after a panicking turn")
	}
}

// TestPanicInACronJobDoesNotEscape covers the same containment for a scheduled run, which
// executes on the scheduler's goroutine rather than the runtime's.
func TestPanicInACronJobDoesNotEscape(t *testing.T) {
	ran := make(chan string, 1)
	run := NewCronRunner(slog.Default(), func() (Runner, error) {
		return panickingRunner{ran: ran}, nil
	}, nil, nil, nil)
	done := make(chan struct{})
	go func() {
		run(context.Background(), CronJob{ID: "j", Prompt: "boom"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the cron runner never returned")
	}
	if len(ran) == 0 {
		t.Fatal("the job never ran")
	}
}

type panickingRunner struct{ ran chan string }

func (p panickingRunner) Run(_ context.Context, input string, _ agent.Events) (string, error) {
	p.ran <- input
	panic("tool blew up")
}

// TestRuntimeBoundsItsThreadAgents covers the one structure that used to grow without limit.
// Each entry holds a whole conversation's history, and with every bot's agents in one heap an
// unbounded map takes the entire process down rather than one bot.
func TestRuntimeBoundsItsThreadAgents(t *testing.T) {
	rt, _ := fleetRuntime(t)
	for i := 0; i < maxThreads+50; i++ {
		rt.state("channel/" + strconv.Itoa(i))
	}
	rt.mu.Lock()
	n := len(rt.threads)
	rt.mu.Unlock()
	if n > maxThreads {
		t.Fatalf("kept %d thread agents, past the %d cap", n, maxThreads)
	}
	// The most recent conversation must survive: it is the one most likely to continue.
	rt.mu.Lock()
	_, fresh := rt.threads["channel/"+strconv.Itoa(maxThreads+49)]
	rt.mu.Unlock()
	if !fresh {
		t.Fatal("eviction dropped the most recently used thread")
	}
}

// TestRuntimeNeverEvictsABusyThread: evicting a thread mid-turn would let a second turn start on
// the same conversation, and evicting one with a pending update would drop that update.
func TestRuntimeNeverEvictsABusyThread(t *testing.T) {
	rt, _ := fleetRuntime(t)
	busy := rt.state("channel/busy")
	busy.lock <- struct{}{} // as a running turn holds it
	pending := rt.state("channel/pending")
	pending.setPending(Inbound{Kind: "thread_update"})

	for i := 0; i < maxThreads+50; i++ {
		rt.state("channel/" + strconv.Itoa(i))
	}
	rt.mu.Lock()
	_, keptBusy := rt.threads["channel/busy"]
	_, keptPending := rt.threads["channel/pending"]
	rt.mu.Unlock()
	if !keptBusy {
		t.Error("evicted a thread with a turn in flight")
	}
	if !keptPending {
		t.Error("evicted a thread that was owed a coalesced update")
	}
}
