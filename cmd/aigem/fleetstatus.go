package main

import (
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/chat"
)

// liveFleet is what only the process running the bots can say about them: which
// ones came up, on which model, how far each heartbeat has backed off, and what
// each is next due to do.
//
// None of it is in the store. A heartbeat tier lives in memory and dies with
// the process, and the chat package must not import the one that runs bots -
// the store is the seam between them. So the fleet records itself here and the
// API is handed a reader.
type liveFleet struct {
	// names is every bot this process was asked to run, fixed for the lifetime
	// of the run. It is what makes "stopped" sayable at all: without it a bot
	// that never came up is indistinguishable from one nobody asked for, and
	// the state an operator would otherwise read journalctl to find is exactly
	// the one that goes missing.
	names []string

	mu   sync.Mutex
	bots map[string]*runningBot
}

// runningBot is one started bot's live handles. They are read, never written,
// from the roster.
type runningBot struct {
	model string
	hb    *bot.Heartbeat
	sched *bot.Scheduler
}

func newLiveFleet(names []string) *liveFleet {
	return &liveFleet{names: names, bots: map[string]*runningBot{}}
}

// started records a bot that is up. A restart replaces the entry, because every
// handle in it belongs to the run that just ended.
func (f *liveFleet) started(name string, rb *runningBot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bots[name] = rb
}

// stopped drops a bot back to "not running". It is called from the same teardown
// that unregisters the bot from the roster, so the two cannot disagree.
func (f *liveFleet) stopped(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.bots, name)
}

// status reports every bot this process runs, keyed by actor id, for the fleet
// screen. A bot with no entry is one that has not started - not one that is
// unknown - so it is reported rather than omitted.
func (f *liveFleet) status() map[string]chat.LiveBot {
	// The handles are picked up under the lock and asked outside it. Each one
	// takes a lock of its own, held by a goroutine that may in turn be waiting
	// on something slow, and a roster poll must not be able to park the start
	// or the teardown of a bot behind it.
	f.mu.Lock()
	live := make(map[string]*runningBot, len(f.bots))
	for name, rb := range f.bots {
		live[name] = rb
	}
	f.mu.Unlock()

	now := time.Now()
	out := make(map[string]chat.LiveBot, len(f.names))
	for _, name := range f.names {
		rb, ok := live[name]
		if !ok {
			out[chat.BotActor(name)] = chat.LiveBot{}
			continue
		}
		// Both from one call: the cadence and the tier are one fact, and reading
		// them separately can pair a tier with the neighbouring tier's label.
		cadence, tier := rb.hb.State()
		l := chat.LiveBot{Running: true, Model: rb.model, Heartbeat: cadence, Tier: tier}
		if job, at, found := rb.sched.NextRun(now); found {
			l.NextJob, l.NextRun = job, &at
		}
		out[chat.BotActor(name)] = l
	}
	return out
}
