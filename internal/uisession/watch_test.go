package uisession

import (
	"sync"
	"testing"
	"time"
)

// Watch is what lets something keep a record of a session honest without
// attending the conversation. Everything below is a promise it makes that
// Subscribe deliberately does not.

func TestAWatcherIsWokenOnlyByTheKindsItAskedFor(t *testing.T) {
	l := New(Config{Ring: 64})
	moved, stop, err := l.Watch(KindTurnStart, KindTurnEnd)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	emit(l, KindContent, KindNotice)
	if woken(moved) {
		t.Fatal("a kind the watcher did not ask for woke it")
	}
	emit(l, KindTurnStart)
	if !woken(moved) {
		t.Error("a kind the watcher asked for did not wake it")
	}
}

// A wake-up says the session moved, not what happened, so a second one while
// the first is pending has nothing to add. That is what makes the last change
// of a turn impossible to lose: a queue can overflow, and after a turn ends
// there may be no next event to repair the drop.
func TestWakeUpsCoalesceRatherThanQueue(t *testing.T) {
	l := New(Config{Ring: 64})
	moved, stop, err := l.Watch(KindTurnStart, KindTurnEnd)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	emit(l, KindTurnStart, KindTurnEnd, KindTurnStart, KindTurnEnd)
	if !woken(moved) {
		t.Fatal("four changes woke the watcher not at all")
	}
	if woken(moved) {
		t.Error("four changes queued more than one wake-up")
	}
}

// The filter is at the source, not in the reader's loop. A turn emits one
// content event per streamed token, and a watcher that had to read past a few
// hundred of them would lose the turn_end behind them - the one drop nothing
// later repairs.
func TestAFloodOfEventsDoesNotPushOutTheChangeAWatcherWaitsFor(t *testing.T) {
	l := New(Config{Ring: 64})
	moved, stop, err := l.Watch(KindTurnEnd)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	l.mu.Lock()
	for range 500 {
		l.emitLocked(Event{Kind: KindContent, Text: "x"})
	}
	l.emitLocked(Event{Kind: KindTurnEnd})
	l.mu.Unlock()

	if !woken(moved) {
		t.Error("the turn's end was lost behind the deltas that preceded it")
	}
}

// A watcher that has stopped reading must not stop the turn that is emitting.
func TestAWatcherThatIsNotReadingDoesNotBlockTheTurn(t *testing.T) {
	l := New(Config{Ring: 64})
	_, stop, err := l.Watch(KindNotice)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		l.mu.Lock()
		defer l.mu.Unlock()
		for range 100 {
			l.emitLocked(Event{Kind: KindNotice})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emitting blocked on a watcher that stopped reading")
	}
}

// A watcher is not a client: presence is what the other tabs are shown, and a
// registry keeping its own table current has no business appearing on it.
func TestAWatcherDoesNotAppearInPresence(t *testing.T) {
	l := New(Config{Ring: 64})
	events, unsubscribe, err := l.Subscribe(Client{ID: "c-1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()

	_, stop, err := l.Watch(KindPresence)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	l.mu.Lock()
	l.emitLocked(Event{Kind: KindNotice})
	l.mu.Unlock()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Kind == KindPresence {
				for _, c := range ev.Clients {
					if c.ID != "c-1" {
						t.Fatalf("presence lists %+v, want only the subscribed client", ev.Clients)
					}
				}
			}
			if ev.Kind == KindNotice {
				return
			}
		case <-deadline:
			t.Fatal("the notice never arrived")
		}
	}
}

// The channel is closed when the session is, which is what ends the loop
// reading it - and a detach afterwards must not close it a second time.
func TestClosingTheSessionEndsItsWatchers(t *testing.T) {
	l := New(Config{Ring: 64})
	moved, stop, err := l.Watch(KindNotice)
	if err != nil {
		t.Fatal(err)
	}

	l.Close()
	select {
	case _, ok := <-moved:
		if ok {
			t.Error("the watcher channel delivered after the session closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing the session did not close its watcher")
	}
	// Both of these would panic on a channel closed twice.
	stop()
	stop()
}

func TestWatchingAClosedSessionIsRefused(t *testing.T) {
	l := New(Config{Ring: 64})
	l.Close()
	if _, _, err := l.Watch(KindNotice); err == nil {
		t.Error("watching a closed session was allowed")
	}
}

// A caller that computes its set of kinds is entitled to compute an empty one.
func TestAWatcherThatAskedForNothingIsNeverWoken(t *testing.T) {
	l := New(Config{Ring: 64})
	moved, stop, err := l.Watch()
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	emit(l, KindTurnEnd, KindNotice, KindContent)
	if woken(moved) {
		t.Error("a watcher that asked for nothing was woken")
	}
}

// Attaching and detaching from several goroutines while a session emits is what
// a daemon holding thirty-two runs does. Under -race this is the test that says
// the list is guarded.
func TestWatchersCanComeAndGoWhileTheSessionEmits(t *testing.T) {
	l := New(Config{Ring: 64})
	var wg sync.WaitGroup
	stopEmitting := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopEmitting:
				return
			default:
			}
			l.mu.Lock()
			l.emitLocked(Event{Kind: KindNotice})
			l.mu.Unlock()
		}
	}()
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				moved, stop, err := l.Watch(KindNotice)
				if err != nil {
					return
				}
				select {
				case <-moved:
				default:
				}
				stop()
				stop()
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stopEmitting)
	wg.Wait()
}

func emit(l *Local, kinds ...Kind) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range kinds {
		l.emitLocked(Event{Kind: k})
	}
}

// woken reports whether a wake-up is waiting. The emit above is synchronous, so
// there is nothing to wait for: either it is there or it is not.
func woken(moved <-chan struct{}) bool {
	select {
	case _, ok := <-moved:
		return ok
	default:
		return false
	}
}
