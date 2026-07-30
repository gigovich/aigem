package mattermost

import (
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/bot"
)

type fakeTimer struct{ stopped bool }

func (f *fakeTimer) Stop() bool {
	was := f.stopped
	f.stopped = true
	return !was
}

// newFakeDebouncer returns a debouncer whose timers never fire on their own; the test invokes
// the captured callbacks to control timing deterministically.
func newFakeDebouncer(fire func(bot.ThreadRef)) (*threadDebouncer, *[]func()) {
	scheduled := &[]func(){}
	d := newThreadDebouncer(time.Minute, fire)
	d.after = func(_ time.Duration, f func()) stoppable {
		*scheduled = append(*scheduled, f)
		return &fakeTimer{}
	}
	return d, scheduled
}

func TestDebounceCoalescesBurst(t *testing.T) {
	var fired []bot.ThreadRef
	d, scheduled := newFakeDebouncer(func(ref bot.ThreadRef) { fired = append(fired, ref) })

	ref := bot.ThreadRef{ChannelID: "c1", RootID: "root1"}
	d.note(ref)
	d.note(ref)
	d.note(ref)

	// A real clock would only fire the last (live) timer; the earlier ones were stopped. Invoke
	// every scheduled callback - the superseded ones must no-op via the generation guard.
	for _, f := range *scheduled {
		f()
	}
	if len(fired) != 1 {
		t.Fatalf("burst of 3 replies should fire once, fired %d times", len(fired))
	}
	if fired[0] != ref {
		t.Fatalf("fired %+v, want %+v", fired[0], ref)
	}
}

func TestDebounceSeparateThreadsFireIndependently(t *testing.T) {
	var fired []string
	d, scheduled := newFakeDebouncer(func(ref bot.ThreadRef) { fired = append(fired, ref.RootID) })

	d.note(bot.ThreadRef{ChannelID: "c1", RootID: "a"})
	d.note(bot.ThreadRef{ChannelID: "c1", RootID: "b"})
	for _, f := range *scheduled {
		f()
	}
	if len(fired) != 2 {
		t.Fatalf("two threads should fire twice, got %v", fired)
	}
}

func TestDebounceRearmsAfterFire(t *testing.T) {
	var fired int
	d, scheduled := newFakeDebouncer(func(bot.ThreadRef) { fired++ })

	ref := bot.ThreadRef{ChannelID: "c1", RootID: "root1"}
	d.note(ref)
	(*scheduled)[0]()
	if fired != 1 {
		t.Fatalf("first quiet period should fire once, got %d", fired)
	}
	// New activity after the fire arms a fresh timer and fires again.
	d.note(ref)
	(*scheduled)[1]()
	if fired != 2 {
		t.Fatalf("new activity should fire again, got %d", fired)
	}
}

// stop must block until an already-running fire returns, so the transport can safely close the
// channel that fire sends to without racing a send-on-closed panic.
func TestDebounceStopWaitsForInFlightFire(t *testing.T) {
	fireStarted := make(chan struct{})
	releaseFire := make(chan struct{})
	d := newThreadDebouncer(time.Minute, func(bot.ThreadRef) {
		close(fireStarted)
		<-releaseFire
	})
	var captured func()
	d.after = func(_ time.Duration, f func()) stoppable { captured = f; return &fakeTimer{} }

	d.note(bot.ThreadRef{ChannelID: "c1", RootID: "root1"})
	go captured() // runs flush -> fire, which blocks in the callback
	<-fireStarted

	stopReturned := make(chan struct{})
	go func() { d.stop(); close(stopReturned) }()

	select {
	case <-stopReturned:
		t.Fatal("stop returned while a fire was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFire)
	select {
	case <-stopReturned:
	case <-time.After(time.Second):
		t.Fatal("stop did not return after the in-flight fire completed")
	}
}

func TestDebounceNoteAfterStopIsNoop(t *testing.T) {
	var fired int
	d, scheduled := newFakeDebouncer(func(bot.ThreadRef) { fired++ })
	d.stop()
	d.note(bot.ThreadRef{ChannelID: "c1", RootID: "root1"})
	if len(*scheduled) != 0 {
		t.Fatalf("note after stop should not arm a timer, armed %d", len(*scheduled))
	}
	if fired != 0 {
		t.Fatalf("fired = %d", fired)
	}
}

func TestDebounceStopCancels(t *testing.T) {
	var fired int
	d, scheduled := newFakeDebouncer(func(bot.ThreadRef) { fired++ })

	d.note(bot.ThreadRef{ChannelID: "c1", RootID: "root1"})
	d.stop()
	for _, f := range *scheduled {
		f()
	}
	if fired != 0 {
		t.Fatalf("stop must cancel pending fires, got %d", fired)
	}
}
