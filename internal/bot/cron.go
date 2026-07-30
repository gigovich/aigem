package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// RunFunc executes one cron job when it fires.
type RunFunc func(ctx context.Context, job CronJob)

// deferredWarnEvery is how many consecutive gated ticks (one per minute) pass between warnings
// that scheduled work is being held back.
const deferredWarnEvery = 30

// maxDeferredTicks is how many minutes of UNBROKEN gating pass before the scheduler fires anyway.
// A turn is supposed to end on its own - the agent's own wall-clock budget cuts it off - so
// reaching this without a single free minute means something is wedged, and the built-in heartbeat
// matters more at that point than avoiding overlap.
const maxDeferredTicks = 90

type scheduledJob struct {
	job     CronJob
	sched   Schedule  // recurring cron; zero value for a one-shot
	at      time.Time // fire time for a one-shot; zero for recurring
	builtin bool      // injected by the runtime: never persisted, cannot be replaced or removed
}

// parseJob turns a stored CronJob into a scheduledJob, reading either its one-shot At instant or
// its recurring cron Expr. Exactly one must be set.
func parseJob(j CronJob) (scheduledJob, error) {
	if j.At != "" {
		if j.Expr != "" {
			return scheduledJob{}, fmt.Errorf("job %q has both at and expr", j.ID)
		}
		at, err := time.Parse(time.RFC3339, j.At)
		if err != nil {
			return scheduledJob{}, fmt.Errorf("at: %w", err)
		}
		return scheduledJob{job: j, at: at}, nil
	}
	sc, err := ParseSchedule(j.Expr)
	if err != nil {
		return scheduledJob{}, err
	}
	return scheduledJob{job: j, sched: sc}, nil
}

// Scheduler owns a bot's live cron jobs, persists changes, and fires due jobs each minute.
type Scheduler struct {
	mu       sync.Mutex
	jobs     map[string]scheduledJob
	order    []string
	running  map[string]bool
	pending  map[string]bool // recurring jobs whose due minute passed while they were held back
	nextScan int             // rotating start index, so one frequent job cannot starve the rest
	save     func([]CronJob) error
	run      RunFunc
	busy     func() bool
	deferred int // consecutive gated ticks, counting only while the gate stays closed
}

// NewScheduler parses the given jobs (skipping and reporting any with a bad expression) and
// returns a scheduler that persists future changes through save (save may be nil).
func NewScheduler(jobs []CronJob, save func([]CronJob) error) (*Scheduler, []error) {
	s := &Scheduler{
		jobs:    map[string]scheduledJob{},
		running: map[string]bool{},
		pending: map[string]bool{},
		save:    save,
	}
	var warns []error
	for _, j := range jobs {
		sj, err := parseJob(j)
		if err != nil {
			warns = append(warns, fmt.Errorf("cron job %q: %w", j.ID, err))
			continue
		}
		if _, ok := s.jobs[j.ID]; !ok {
			s.order = append(s.order, j.ID)
		}
		s.jobs[j.ID] = sj
	}
	return s, warns
}

// SetRunner installs the callback used to execute fired jobs.
func (s *Scheduler) SetRunner(run RunFunc) {
	s.mu.Lock()
	s.run = run
	s.mu.Unlock()
}

// SetBusy installs a predicate the scheduler consults before firing anything. While it reports
// true nothing fires: a due one-shot keeps its instant, a due recurring occurrence is remembered
// (see holdDueLocked), and both run on the first free tick. A fired job builds its own fresh agent
// and runs concurrently with chat turns, so without this gate a job due mid-turn would put a
// second agent on the same work - which is why watchdog intervals previously had to exceed the
// whole turn budget. The one exception is maxDeferredTicks: after that much unbroken gating the
// scheduler fires anyway rather than let a wedged turn silence the built-in heartbeat forever.
func (s *Scheduler) SetBusy(busy func() bool) {
	s.mu.Lock()
	s.busy = busy
	s.mu.Unlock()
}

// snapshot returns the persistable jobs in stable order, excluding built-ins. Caller holds s.mu.
func (s *Scheduler) snapshot() []CronJob {
	out := make([]CronJob, 0, len(s.order))
	for _, id := range s.order {
		if sj := s.jobs[id]; !sj.builtin {
			out = append(out, sj.job)
		}
	}
	return out
}

// Set adds or replaces a job (validating its expression or one-shot time) and persists the change.
func (s *Scheduler) Set(job CronJob) error {
	if job.ID == "" {
		return fmt.Errorf("cron job needs an id")
	}
	sj, err := parseJob(job)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.jobs[job.ID]; ok && existing.builtin {
		return fmt.Errorf("job %q is built-in and cannot be replaced", job.ID)
	}
	s.insertLocked(sj)
	if s.save != nil {
		return s.save(s.snapshot())
	}
	return nil
}

// SetBuiltin installs a runtime-owned recurring job: it fires like any recurring job but is
// never persisted, and Set/Remove refuse its id. A same-id job loaded from the persisted
// config is displaced (the built-in wins) and will be dropped from the config on the next
// save; that displacement is logged.
func (s *Scheduler) SetBuiltin(job CronJob) error {
	if job.ID == "" {
		return fmt.Errorf("cron job needs an id")
	}
	if job.At != "" {
		return fmt.Errorf("built-in job %q must be recurring, not one-shot", job.ID)
	}
	sj, err := parseJob(job)
	if err != nil {
		return err
	}
	sj.builtin = true
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.jobs[job.ID]; ok && !existing.builtin {
		slog.Warn("built-in job displaces a configured job with the same id; the configured job will "+
			"not fire and will be dropped from the persisted config", "id", job.ID)
	}
	s.insertLocked(sj)
	return nil
}

// insertLocked adds or replaces a job, keeping order stable. A replacement drops any held-back
// occurrence: it belonged to the schedule that was just discarded, and firing it would run the new
// definition at a time its own expression does not name. Caller holds s.mu.
func (s *Scheduler) insertLocked(sj scheduledJob) {
	if _, ok := s.jobs[sj.job.ID]; !ok {
		s.order = append(s.order, sj.job.ID)
	}
	delete(s.pending, sj.job.ID)
	s.jobs[sj.job.ID] = sj
}

// IsBuiltin reports whether id names a runtime-owned job.
func (s *Scheduler) IsBuiltin(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id].builtin
}

// dueAt reports whether the job should fire at now: a one-shot once its instant has arrived, a
// recurring job when its cron matches.
func (sj scheduledJob) dueAt(now time.Time) bool {
	if !sj.at.IsZero() {
		return !now.Before(sj.at)
	}
	return sj.sched.Matches(now)
}

// deleteLocked removes a job from the scheduler. Caller holds s.mu.
func (s *Scheduler) deleteLocked(id string) {
	delete(s.jobs, id)
	delete(s.running, id)
	delete(s.pending, id)
	for i, x := range s.order {
		if x == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Remove deletes a job and persists the change.
func (s *Scheduler) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sj, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("no cron job %q", id)
	}
	if sj.builtin {
		return fmt.Errorf("job %q is built-in and cannot be removed", id)
	}
	s.deleteLocked(id)
	if s.save != nil {
		return s.save(s.snapshot())
	}
	return nil
}

// List returns the current jobs in stable order, built-ins included.
func (s *Scheduler) List() []CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CronJob, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.jobs[id].job)
	}
	return out
}

// tick fires at most one due job. A due one-shot is removed before it runs so it fires exactly
// once, even across a restart (its At persists and fires on the next tick). Anything else that was
// due keeps its turn for a later tick: only one scheduled agent runs at a time, because each one
// is a fresh agent that would otherwise work the same ticket as its sibling.
func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	// Snapshot and call busy outside s.mu: the predicate reaches into the runtime's own locks,
	// and holding the scheduler lock across it would couple the two for no reason.
	s.mu.Lock()
	busy := s.busy
	s.mu.Unlock()
	gated := busy != nil && busy()

	s.mu.Lock()
	if !gated {
		// A free tick proves nothing is wedged, so the ceiling's count starts over. Counting
		// gated ticks across separate turns instead would let two ordinary long turns add up to
		// the ceiling and hand the next fire a free pass onto a live turn.
		s.deferred = 0
	} else if s.deferred < maxDeferredTicks {
		s.deferred++
		n := s.deferred
		s.holdDueLocked(now)
		s.mu.Unlock()
		if n%deferredWarnEvery == 0 {
			slog.Warn("scheduled work has been held back for a long stretch; a turn may be wedged",
				"gatedMinutes", n)
		}
		return
	}
	held := s.deferred
	run := s.run
	job, ok := CronJob{}, false
	if run != nil {
		job, ok = s.takeDueLocked(now)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	if gated {
		// Past the ceiling we fire anyway. A turn that never ends must not silence the built-in
		// heartbeat forever: a bot that cannot wake up at all is worse than two agents overlapping
		// once, and this is the only way out that does not need an operator.
		slog.Error("firing scheduled work while a turn is still running: work has been held back "+
			"far too long", "job", job.ID, "gatedMinutes", held)
	}
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.running, job.ID)
			s.mu.Unlock()
		}()
		run(ctx, job)
	}()
}

// holdDueLocked records the recurring occurrences that are due right now so a later tick still
// fires them. A one-shot needs no help - its instant stays past, so it stays due - but a recurring
// job matches only during its own minute, so without this a daily job gated at its single minute
// would silently skip the whole day. A job already running is skipped: it is mid-flight, not
// missed. Caller holds s.mu.
func (s *Scheduler) holdDueLocked(now time.Time) {
	for _, id := range s.order {
		sj := s.jobs[id]
		if sj.at.IsZero() && sj.dueAt(now) && !s.running[id] {
			s.pending[id] = true
		}
	}
}

// takeDueLocked picks the one job to run now and holds back every other due recurring job. The scan
// starts after whatever ran last, so a frequent job cannot monopolise the single slot and starve a
// rarer one - a job due every minute would otherwise always be found first. Caller holds s.mu.
func (s *Scheduler) takeDueLocked(now time.Time) (CronJob, bool) {
	chosen := ""
	for i := range s.order {
		id := s.order[(s.nextScan+i)%len(s.order)]
		sj := s.jobs[id]
		// pending only ever revives a recurring occurrence. Honouring it for a one-shot would fire
		// (and delete) a job whose instant has not arrived - e.g. one re-armed to tomorrow while an
		// occurrence of the recurring job it replaced was still held back.
		heldBack := sj.at.IsZero() && s.pending[id]
		if s.running[id] || (!sj.dueAt(now) && !heldBack) {
			continue
		}
		if chosen == "" {
			chosen = id
			continue
		}
		if sj.at.IsZero() {
			s.pending[id] = true
		}
	}
	if chosen == "" {
		return CronJob{}, false
	}
	for i, id := range s.order {
		if id == chosen {
			s.nextScan = (i + 1) % len(s.order)
			break
		}
	}
	sj := s.jobs[chosen]
	delete(s.pending, chosen)
	if sj.at.IsZero() {
		s.running[chosen] = true // recurring: guard against overlapping itself, keep the job
		return sj.job, true
	}
	s.deleteLocked(chosen) // one-shot: drop it so it cannot fire again
	if s.save != nil {
		// If this persist fails the job is gone in-memory but still on disk, so a restart could
		// re-fire it. Nothing here can return the error, so surface it in the log.
		if err := s.save(s.snapshot()); err != nil {
			slog.Warn("could not persist one-shot removal", "job", chosen, "err", err)
		}
	}
	return sj.job, true
}

// Run ticks once per aligned minute until ctx is done. A tick that overruns its minute, or a
// suspended machine, would otherwise skip minutes outright - and a recurring job matches only its
// own minute, so a skipped minute loses that occurrence with nothing left to recover it. Every
// minute that went by unticked is therefore replayed for its due jobs before the current one runs.
// This covers gaps within a run only: what is held is process memory, so minutes that passed while
// the bot was stopped are not recoverable, and a job whose single minute fell in that window waits
// for its next one.
func (s *Scheduler) Run(ctx context.Context) {
	last := time.Now().Truncate(time.Minute)
	for {
		next := last.Add(time.Minute)
		if d := time.Until(next); d > 0 {
			timer := time.NewTimer(d)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		} else if ctx.Err() != nil {
			return
		}
		now := time.Now().Truncate(time.Minute)
		if now.Before(next) {
			// Wall time moved backwards (an NTP step, a corrected VM clock). Resynchronise to it
			// rather than pressing on from a future bookkeeping minute, which would leave the
			// minutes in between neither ticked nor carried forward.
			slog.Warn("the clock moved backwards; resynchronising the scheduler",
				"expected", next.Format("15:04"), "actual", now.Format("15:04"))
			last = now
			s.tick(ctx, now)
			continue
		}
		for m, n := next, 0; m.Before(now) && n < maxReplayMinutes; m, n = m.Add(time.Minute), n+1 {
			s.holdMissed(m)
		}
		last = now
		s.tick(ctx, now)
	}
}

// maxReplayMinutes caps how far back the replay reaches. A day is far more than any real overrun,
// and it keeps a large forward clock jump (a restored VM, a corrected container clock) from
// scanning every job once per elapsed minute.
const maxReplayMinutes = 24 * 60

// holdMissed records the recurring jobs that were due in a minute the scheduler never ticked, so
// the next tick still fires them.
func (s *Scheduler) holdMissed(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.pending)
	s.holdDueLocked(at)
	if len(s.pending) > before {
		slog.Warn("a scheduler minute went unticked; its due jobs were carried forward",
			"minute", at.Format("15:04"))
	}
}
