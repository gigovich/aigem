package chatlink

import (
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/bot"
)

// stoppable is the subset of *time.Timer the debouncer needs, swapped for a fake in tests.
type stoppable interface{ Stop() bool }

// threadDebouncer coalesces a burst of replies in one thread into a single fire once the thread
// has been quiet for `delay`. Each new reply resets that thread's timer, so one quiet period
// yields exactly one fire - the owner reacts to the whole burst at once instead of per message,
// which is also what keeps two bots from triggering each other on every line.
type threadDebouncer struct {
	mu      sync.Mutex
	delay   time.Duration
	after   func(time.Duration, func()) stoppable // time.AfterFunc wrapper; overridable in tests
	fire    func(bot.ThreadID)
	pending map[string]bot.ThreadID
	timers  map[string]stoppable
	gen     map[string]int // active thread -> its current timer's seq; pruned on flush
	seq     int            // global monotonic timer id; never reused, so a stale timer no-ops
	stopped bool
	firing  sync.WaitGroup // tracks in-flight fire calls so stop can wait them out
}

func newThreadDebouncer(delay time.Duration, fire func(bot.ThreadID)) *threadDebouncer {
	return &threadDebouncer{
		delay:   delay,
		after:   func(d time.Duration, f func()) stoppable { return time.AfterFunc(d, f) },
		fire:    fire,
		pending: map[string]bot.ThreadID{},
		timers:  map[string]stoppable{},
		gen:     map[string]int{},
	}
}

// note records a new reply in the thread and (re)arms its quiet-period timer.
func (d *threadDebouncer) note(ref bot.ThreadID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.pending[string(ref)] = ref
	if t := d.timers[string(ref)]; t != nil {
		t.Stop()
	}
	d.seq++    // a globally unique, never-reused id; a stalled old timer's seq can never match a
	s := d.seq // later re-armed one, even after gen[root] is pruned and re-created on flush.
	d.gen[string(ref)] = s
	root := string(ref)
	d.timers[string(ref)] = d.after(d.delay, func() { d.flush(root, s) })
}

// flush fires once for the thread, unless a newer reply superseded this timer or the debouncer
// was stopped. fire runs without the lock, but firing.Add happens under it so stop's Wait sees
// every fire that will start - which is what keeps a fire from racing the transport's shutdown.
func (d *threadDebouncer) flush(rootID string, g int) {
	d.mu.Lock()
	if d.stopped || d.gen[rootID] != g {
		d.mu.Unlock()
		return
	}
	ref, ok := d.pending[rootID]
	delete(d.pending, rootID)
	delete(d.timers, rootID)
	delete(d.gen, rootID)
	if !ok {
		d.mu.Unlock()
		return
	}
	d.firing.Add(1)
	d.mu.Unlock()
	defer d.firing.Done()
	d.fire(ref)
}

// stop cancels every pending timer, blocks any future fire, and waits for any in-flight fire to
// return before it does - so the caller can then close the channel fire sends to without racing.
func (d *threadDebouncer) stop() {
	d.mu.Lock()
	d.stopped = true
	for id, t := range d.timers {
		t.Stop()
		delete(d.timers, id)
	}
	d.pending = map[string]bot.ThreadID{}
	d.gen = map[string]int{}
	d.mu.Unlock()
	d.firing.Wait()
}
