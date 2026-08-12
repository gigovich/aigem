package fanout

import (
	"testing"
	"time"
)

type frame struct {
	seq  uint64
	drop bool
	last uint64
}

func newTest(cap int, backlog []frame, skipTo uint64) *Sub[frame] {
	return New(Config[frame]{
		QueueCap: cap,
		Backlog:  backlog,
		SkipTo:   skipTo,
		SeqOf:    func(f frame) uint64 { return f.seq },
		OnDrop:   func(last uint64) frame { return frame{drop: true, last: last} },
	})
}

func recv(t *testing.T, ch <-chan frame) (frame, bool) {
	t.Helper()
	select {
	case f, ok := <-ch:
		return f, ok
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a frame")
		return frame{}, false
	}
}

// The whole point of the bound: a reader that stops reading is dropped rather
// than allowed to stall Push, and it is told where to resume so reconnecting
// leaves no hole below the marker.
func TestFallingBehindDropsWithAResumePoint(t *testing.T) {
	const cap = 4
	s := newTest(cap, nil, 0)
	go s.Run()
	defer s.Stop()

	// Nothing is read, so everything queues. Push must never block.
	done := make(chan struct{})
	go func() {
		for i := uint64(1); i <= 40; i++ {
			s.Push(frame{seq: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Push blocked on a reader that stopped reading")
	}

	var last uint64
	for {
		f, ok := recv(t, s.Out())
		if !ok {
			t.Fatal("the channel closed before the drop marker arrived")
		}
		if f.drop {
			if f.last != last {
				t.Fatalf("drop marker carries %d, want %d (the last frame actually delivered)",
					f.last, last)
			}
			break
		}
		if f.seq != last+1 {
			t.Fatalf("got seq %d after %d: the prefix below the marker must be contiguous",
				f.seq, last)
		}
		last = f.seq
	}
	// How many frames land below the marker is not fixed: the pump may already
	// hold one, which frees a slot for one more Push. What is fixed is that the
	// reader was cut off long before the 40th and that the prefix has no hole.
	if last == 0 || last >= 40 {
		t.Fatalf("delivered up to seq %d before the marker; want a bounded prefix", last)
	}
	if _, ok := recv(t, s.Out()); ok {
		t.Fatal("the channel must close after the drop marker")
	}
}

// Backlog is history the reader asked for. It must not count against the cap, or
// a stream longer than the cap is dropped for being long rather than for being
// slow.
func TestBacklogDoesNotCountAgainstTheCap(t *testing.T) {
	const cap = 2
	backlog := []frame{{seq: 1}, {seq: 2}, {seq: 3}, {seq: 4}, {seq: 5}}
	s := newTest(cap, backlog, 0)
	go s.Run()
	defer s.Stop()

	for i := uint64(6); i <= 7; i++ {
		s.Push(frame{seq: i})
	}
	for want := uint64(1); want <= 7; want++ {
		f, ok := recv(t, s.Out())
		if !ok {
			t.Fatalf("channel closed at seq %d", want)
		}
		if f.drop {
			t.Fatalf("dropped at seq %d: the backlog was counted against the cap", want)
		}
		if f.seq != want {
			t.Fatalf("got seq %d, want %d", f.seq, want)
		}
	}
}

// A subscriber registered before its history was fetched keeps what arrives in
// that window and drops what the history also carries, so the reader sees each
// frame once and in order.
func TestPrependSplicesHistoryAndDropsDuplicates(t *testing.T) {
	s := newTest(16, nil, 3)
	// Below SkipTo: the history will carry these.
	s.Push(frame{seq: 2})
	s.Push(frame{seq: 3})
	// Above it: only the queue has these.
	s.Push(frame{seq: 4})
	s.Push(frame{seq: 5})
	s.Prepend([]frame{{seq: 1}, {seq: 2}, {seq: 3}})
	go s.Run()
	defer s.Stop()

	for want := uint64(1); want <= 5; want++ {
		f, ok := recv(t, s.Out())
		if !ok {
			t.Fatalf("channel closed at seq %d", want)
		}
		if f.seq != want {
			t.Fatalf("got seq %d, want %d", f.seq, want)
		}
	}
}

// Stop is called on a subscriber that may never have existed - replacing a
// client id stops whatever was there before, and usually nothing was.
func TestStopIsNilSafeAndRepeatable(t *testing.T) {
	var nilSub *Sub[frame]
	nilSub.Stop()

	s := newTest(4, nil, 0)
	go s.Run()
	s.Stop()
	s.Stop()
	if _, ok := recv(t, s.Out()); ok {
		t.Fatal("the channel must close once the subscriber is stopped")
	}
}
