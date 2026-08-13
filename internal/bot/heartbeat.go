package bot

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// WorkHeartbeatJobID is the reserved id of the runtime-injected work heartbeat.
const WorkHeartbeatJobID = "work-heartbeat"

// HeartbeatIdleMarker is what a heartbeat run answers when it found nothing to advance. The
// runtime reads it to decide whether to back the interval off, so it must be an exact match.
const HeartbeatIdleMarker = "IDLE"

const workHeartbeatPrompt = "Work heartbeat: a timer woke you to check that nothing you own is " +
	"silently stalled. No human reads this answer, so do not post to chat just because you woke " +
	"up. Read your memory, then check the tracker for the work assigned to you and the standing " +
	"duties of your role. If something is yours to advance, advance it now - take the next " +
	"concrete step rather than reporting that you would.\n\n" +
	"Two things are never yours to advance on a wake-up: work under a hold nobody has explicitly " +
	"lifted, and work whose blocking question you asked and never got answered. Neither passing " +
	"time nor how the tracker looks changes that - leave them and advance something else.\n\n" +
	"Your answer goes to the runtime, not to chat. It only says how productive this wake was, " +
	"which sets how soon you are woken again:\n" +
	"- If you advanced anything, answer with a one-line summary of what you actually did. An " +
	"empty answer or NO_REPLY reads as \"nothing to advance\" and slows your wake-ups down.\n" +
	"- If there is genuinely no step you could take on anything you own, start your answer " +
	"with the English word " + HeartbeatIdleMarker + " (use it even if you normally write in " +
	"another language); you may add a short reason after it on the same line.\n\n" +
	"Before answering " + HeartbeatIdleMarker + ", make sure it is true and that being stuck " +
	"is visible to someone other than you: each item you cannot move must have its ticket " +
	"record what it waits for, and whoever can clear that must already have been asked. If " +
	"either is missing, doing that IS this wake's work - do it and report it."

// heartbeatCadences are the intervals the heartbeat backs off through, fastest first, while it
// keeps finding nothing to do: every 30 minutes, hourly, every 2 hours, every 4 hours. Tier 0 is
// the working cadence; the slow tiers exist so an idle fleet costs little. Each entry takes the
// bot's minute offset and reduces it modulo 60, so no offset can produce an invalid minute.
//
// Tier 0 deliberately exceeds the default per-turn wall-clock budget. A cadence at or below it
// would let a heartbeat that runs to its budget become due again the moment it stops, giving a bot
// stuck in a long unproductive loop an unbroken duty cycle - the most expensive state there is.
var heartbeatCadences = []func(offset int) string{
	func(o int) string { return everyMinutes(o, 30) },
	func(o int) string { return everyMinutes(o, 60) },
	func(o int) string { return fmt.Sprintf("%d */2 * * *", o%60) },
	func(o int) string { return fmt.Sprintf("%d */4 * * *", o%60) },
}

// everyMinutes builds a cron minute list that fires every step minutes, phase-shifted by offset.
func everyMinutes(offset, step int) string {
	var mins []string
	for m := 0; m < 60; m += step {
		mins = append(mins, strconv.Itoa((offset+m)%60))
	}
	return strings.Join(mins, ",") + " * * * *"
}

// heartbeatIntervals label the cadences above for a reader. They are a parallel array to
// heartbeatCadences and must stay the same length and order: the tier indexes both.
// TestHeartbeatCadencesAreAllLabelled is the guard.
var heartbeatIntervals = []string{"30m", "1h", "2h", "4h"}

// idlesPerTier is how many consecutive idle heartbeats it takes to drop to the next tier.
const idlesPerTier = 2

// builtinSetter installs a runtime-owned job. *Scheduler satisfies it.
type builtinSetter interface {
	SetBuiltin(job CronJob) error
}

// Heartbeat owns the built-in wake-up job and the backoff behind it. State changes and the
// re-arming they imply happen together under one lock: computing a new cadence in one goroutine
// while another installs a different one would leave the scheduler running a cadence the
// controller does not think is in force, with nothing to ever correct it.
type Heartbeat struct {
	sched  builtinSetter
	offset int

	mu    sync.Mutex
	log   *slog.Logger
	idles int
	armed string // cron expression currently installed
}

// NewHeartbeat returns the heartbeat for a bot. Its wake minutes are derived from the bot name so
// a fleet spreads across the hour instead of hitting the provider in lockstep.
func NewHeartbeat(botName string, sched builtinSetter) *Heartbeat {
	return &Heartbeat{sched: sched, offset: nameOffset(botName, heartbeatOffsetSlots)}
}

// heartbeatOffsetSlots is the range of per-bot minute offsets. It matches the fastest cadence's
// step so the whole window is usable and no two bots need to share a slot.
const heartbeatOffsetSlots = 30

// Arm installs the heartbeat at its working cadence. A failure here is fatal to a bot start:
// without the heartbeat a bot that runs out of chat wake-ups has no way back to its own work.
func (h *Heartbeat) Arm() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.applyLocked()
}

// AfterCronRun feeds one scheduled run's outcome back into the cadence. It ignores every job but
// the heartbeat, so the caller can hand it every cron result without branching.
func (h *Heartbeat) AfterCronRun(jobID, answer string) {
	if jobID != WorkHeartbeatJobID {
		return
	}
	idle := IsIdleAnswer(answer)
	h.observe(idle)
	// How a wake was scored is the one thing needed to explain a bot's cadence after the fact.
	h.mu.Lock()
	log := h.loggerLocked()
	h.mu.Unlock()
	log.Info("heartbeat outcome", "idle", idle, "tier", h.Tier())
}

// Addressed speeds the heartbeat up by one tier, because a bot someone is talking to is probably
// about to have work. It deliberately does not jump straight back to the working cadence: the
// teammates are bots too, every handoff is an @mention and every check-in a DM, so a full reset
// would let ordinary inter-bot traffic pin the whole fleet at its fastest cadence forever. A real
// conversation is several messages, so it still restores tier 0 quickly.
func (h *Heartbeat) Addressed() {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Snap to the bottom of the next faster tier rather than subtracting. A heartbeat run can be
	// long enough to contain a whole conversation, so its verdict often lands after this; landing
	// on the boundary means one late "idle" cannot climb back to the tier we just left, and no
	// verdict has to be thrown away to achieve that.
	if t := h.tierLocked(); t > 0 {
		h.idles = (t - 1) * idlesPerTier
	} else {
		h.idles = 0
	}
	h.applyOrLogLocked()
}

func (h *Heartbeat) observe(idle bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if idle {
		// Stop counting once the slowest tier is reached, so a long-idle bot is exactly one
		// message away from speeding up rather than dozens.
		if max := len(heartbeatCadences)*idlesPerTier - 1; h.idles < max {
			h.idles++
		}
	} else {
		h.idles = 0
	}
	h.applyOrLogLocked()
}

func (h *Heartbeat) applyOrLogLocked() {
	if err := h.applyLocked(); err != nil {
		h.logArmFailure(err)
	}
}

// applyLocked installs the current tier's expression when it differs from what is already armed.
// Caller holds h.mu.
func (h *Heartbeat) applyLocked() error {
	want := heartbeatCadences[h.tierLocked()](h.offset)
	if want == h.armed {
		return nil
	}
	if err := h.sched.SetBuiltin(WorkHeartbeatJob(want)); err != nil {
		return fmt.Errorf("arm work heartbeat at %q: %w", want, err)
	}
	h.armed = want
	h.logArmed(want, h.tierLocked())
	return nil
}

func (h *Heartbeat) tierLocked() int {
	t := h.idles / idlesPerTier
	if t >= len(heartbeatCadences) {
		t = len(heartbeatCadences) - 1
	}
	return t
}

// Tier is the current backoff tier: 0 while there is work, rising as idle runs accumulate.
func (h *Heartbeat) Tier() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tierLocked()
}

// Cadence is the current interval as a label. The roster shows it beside the tier, because "t3"
// on its own says a bot has been idle a while without saying when it next looks.
func (h *Heartbeat) Cadence() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return heartbeatIntervals[h.tierLocked()]
}

// Armed is the cron expression currently installed.
func (h *Heartbeat) Armed() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.armed
}

// IsIdleAnswer reports whether a heartbeat run's answer means "nothing to advance".
//
// Three answers count: the marker, the runtime's silence sentinel, and nothing at all. Only the
// marker is asked for, but the operating protocol trains every bot that saying nothing means
// replying NO_REPLY, and a heartbeat prompt that says "post nothing unless you have real news"
// primes exactly that reflex - so accepting only the marker would mean the backoff essentially
// never engaged. A run that genuinely advanced something reports what it did, which is none of
// these. The cost of a wrong guess is asymmetric: one tier slower, undone by the next productive
// run, against paying the fastest cadence forever.
//
// The marker only has to open the answer: a bot that adds why it is idle ("IDLE - still waiting on
// the mailbox") is still telling us to check back less often.
func IsIdleAnswer(answer string) bool {
	first := strings.TrimSpace(answer)
	if first == "" {
		return true
	}
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(strings.Trim(strings.TrimSpace(first), "`*_"))
	if isNoReply(first) {
		return true
	}
	if !strings.HasPrefix(strings.ToUpper(first), HeartbeatIdleMarker) {
		return false
	}
	rest := strings.TrimSpace(first[len(HeartbeatIdleMarker):])
	// Require a separator, so "IDLENESS..." or a sentence merely starting with those letters is
	// not read as the marker.
	return rest == "" || strings.ContainsAny(rest[:1], ".,:;-–—()")
}

// WorkHeartbeatJob returns the built-in heartbeat job at the given cron expression.
func WorkHeartbeatJob(expr string) CronJob {
	return CronJob{ID: WorkHeartbeatJobID, Expr: expr, Prompt: workHeartbeatPrompt}
}

// SetLogger installs the logger the heartbeat reports through. Set before Arm.
func (h *Heartbeat) SetLogger(l *slog.Logger) {
	if l == nil {
		return
	}
	h.mu.Lock()
	h.log = l
	h.mu.Unlock()
}

// loggerLocked is the installed logger, or the default. Caller holds h.mu.
func (h *Heartbeat) loggerLocked() *slog.Logger {
	if h.log == nil {
		return slog.Default()
	}
	return h.log
}

func (h *Heartbeat) logArmed(expr string, tier int) {
	h.loggerLocked().Info("work heartbeat armed", "expr", expr, "tier", tier)
}

// logArmFailure reports a re-arm that did not take. The previous cadence stays in force,
// so the bot keeps waking - just not at the interval it now wants.
func (h *Heartbeat) logArmFailure(err error) {
	h.loggerLocked().Warn("could not re-arm the work heartbeat; the previous cadence stays in force", "err", err)
}
