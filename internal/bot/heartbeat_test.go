package bot

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBuiltin records every cadence the heartbeat installs.
type fakeBuiltin struct {
	mu    sync.Mutex
	jobs  []CronJob
	fail  error
	calls int
}

func (f *fakeBuiltin) SetBuiltin(job CronJob) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		return f.fail
	}
	f.jobs = append(f.jobs, job)
	return nil
}

func (f *fakeBuiltin) exprs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.jobs))
	for i, j := range f.jobs {
		out[i] = j.Expr
	}
	return out
}

func newArmedHeartbeat(t *testing.T, name string) (*Heartbeat, *fakeBuiltin) {
	t.Helper()
	sched := &fakeBuiltin{}
	hb := NewHeartbeat(name, sched)
	if err := hb.Arm(); err != nil {
		t.Fatal(err)
	}
	return hb, sched
}

func TestHeartbeatArmsWorkingCadence(t *testing.T) {
	hb, sched := newArmedHeartbeat(t, "amiran")
	if hb.Tier() != 0 {
		t.Fatalf("a fresh heartbeat must start at the working cadence, tier = %d", hb.Tier())
	}
	got := sched.exprs()
	if len(got) != 1 {
		t.Fatalf("Arm should install exactly one job, got %v", got)
	}
	if hb.Armed() != got[0] {
		t.Fatalf("Armed() = %q but installed %q", hb.Armed(), got[0])
	}
	// Two wakes an hour is the working cadence.
	if n := len(strings.Split(strings.Fields(got[0])[0], ",")); n != 2 {
		t.Fatalf("tier 0 should wake twice an hour, expr %q", got[0])
	}
}

// The whole feature is "an idle fleet stays cheap", so the intervals themselves need pinning: an
// expression that merely parses and differs from its neighbours can still cost 20x.
func TestHeartbeatCadenceIntervals(t *testing.T) {
	want := []int{48, 24, 12, 6} // fires per day: every 30m, hourly, 2-hourly, 4-hourly
	if len(want) != len(heartbeatCadences) {
		t.Fatalf("expected %d cadences, got %d", len(want), len(heartbeatCadences))
	}
	day := time.Date(2026, 7, 25, 0, 0, 0, 0, time.Local)
	for tier, cadence := range heartbeatCadences {
		sched, err := ParseSchedule(cadence(7))
		if err != nil {
			t.Fatalf("tier %d: %v", tier, err)
		}
		fires := 0
		for m := 0; m < 24*60; m++ {
			if sched.Matches(day.Add(time.Duration(m) * time.Minute)) {
				fires++
			}
		}
		if fires != want[tier] {
			t.Errorf("tier %d fires %d times a day, want %d (expr %q)",
				tier, fires, want[tier], cadence(7))
		}
		if tier > 0 && want[tier] >= want[tier-1] {
			t.Errorf("tier %d is not slower than tier %d", tier, tier-1)
		}
	}
	// The working cadence must outlast a turn's own wall-clock budget, or a heartbeat that runs to
	// its budget becomes due again the moment it stops.
	if mins := 24 * 60 / want[0]; mins <= 20 {
		t.Errorf("tier 0 interval is %d min, not comfortably above the 20 min turn budget", mins)
	}
}

// The re-arm loop is the actual feature: an IDLE answer must reach the scheduler as a slower job.
func TestHeartbeatIdleAnswersReArmTheScheduler(t *testing.T) {
	hb, sched := newArmedHeartbeat(t, "amiran")
	base := hb.Armed()

	hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	if hb.Armed() != base {
		t.Fatal("a single idle run should not slow the cadence yet")
	}
	hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	if hb.Armed() == base {
		t.Fatal("two consecutive idle runs should have installed a slower cadence")
	}
	if hb.Tier() != 1 {
		t.Fatalf("tier = %d, want 1", hb.Tier())
	}
	if got := sched.exprs(); len(got) != 2 || got[1] != hb.Armed() {
		t.Fatalf("the scheduler should hold the new cadence, installed %v, armed %q", got, hb.Armed())
	}

	// A run that did work snaps straight back, whatever tier it had reached.
	for i := 0; i < 20; i++ {
		hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	}
	if hb.Tier() != len(heartbeatCadences)-1 {
		t.Fatalf("tier should saturate at %d, got %d", len(heartbeatCadences)-1, hb.Tier())
	}
	hb.AfterCronRun(WorkHeartbeatJobID, "pushed 9e0ac9d, CI green")
	if hb.Armed() != base {
		t.Fatalf("work should restore the working cadence, armed %q want %q", hb.Armed(), base)
	}
}

func TestHeartbeatIgnoresOtherJobs(t *testing.T) {
	hb, sched := newArmedHeartbeat(t, "lisa")
	before := sched.exprs()
	// The memory review answering nothing must not be read as the bot being idle.
	hb.AfterCronRun(MemoryReviewJobID, HeartbeatIdleMarker)
	hb.AfterCronRun(MemoryReviewJobID, HeartbeatIdleMarker)
	hb.AfterCronRun("amiran-work-check", HeartbeatIdleMarker)
	if hb.Tier() != 0 {
		t.Fatalf("another job's answer must not change the heartbeat tier, got %d", hb.Tier())
	}
	if len(sched.exprs()) != len(before) {
		t.Fatal("another job's answer must not re-arm the heartbeat")
	}
}

func TestHeartbeatAddressedResets(t *testing.T) {
	hb, _ := newArmedHeartbeat(t, "kate")
	base := hb.Armed()
	hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	if hb.Armed() == base {
		t.Fatal("expected a slower cadence after two idle runs")
	}
	hb.Addressed()
	if hb.Armed() != base {
		t.Fatalf("being addressed must restore the working cadence, got %q", hb.Armed())
	}
}

// Teammates are bots: every handoff is an @mention and every check-in a DM, so a full reset per
// message would let ordinary inter-bot traffic pin the fleet at its fastest cadence forever.
func TestHeartbeatAddressedStepsOneTierAtATime(t *testing.T) {
	hb, _ := newArmedHeartbeat(t, "amiran")
	for i := 0; i < 2*len(heartbeatCadences)*idlesPerTier; i++ {
		hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	}
	deepest := hb.Tier()
	if deepest != len(heartbeatCadences)-1 {
		t.Fatalf("tier = %d, want the slowest %d", deepest, len(heartbeatCadences)-1)
	}
	hb.Addressed()
	if got := hb.Tier(); got != deepest-1 {
		t.Fatalf("one message moved tier %d -> %d, want one step to %d", deepest, got, deepest-1)
	}
	// A real conversation is several messages, so it still gets back to the working cadence.
	for i := 0; i < len(heartbeatCadences); i++ {
		hb.Addressed()
	}
	if hb.Tier() != 0 {
		t.Fatalf("a few messages should restore the working cadence, tier = %d", hb.Tier())
	}
}

// A lost update here would leave the scheduler running a cadence the heartbeat does not know
// about, with nothing to correct it - the bot would poll hourly while reporting tier 0.
func TestHeartbeatStateAndSchedulerNeverDisagree(t *testing.T) {
	hb, sched := newArmedHeartbeat(t, "jane")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker) }()
		go func() { defer wg.Done(); hb.Addressed() }()
	}
	wg.Wait()
	installed := sched.exprs()
	if last := installed[len(installed)-1]; last != hb.Armed() {
		t.Fatalf("scheduler holds %q but the heartbeat believes %q", last, hb.Armed())
	}
	want := heartbeatCadences[hb.Tier()](nameOffset("jane", heartbeatOffsetSlots))
	if hb.Armed() != want {
		t.Fatalf("armed %q does not match tier %d (%q)", hb.Armed(), hb.Tier(), want)
	}
}

func TestMemoryReviewDoesNotCollideWithTheHeartbeat(t *testing.T) {
	// Both minutes come from the bot name; an unsalted hash reduced by two divisors of 60
	// collides for every name, so every bot ran two fresh agents at once every night.
	for _, name := range []string{"amiran", "lisa", "kate", "jane", "demetre", "mark"} {
		hb, _ := newArmedHeartbeat(t, name)
		beat, err := ParseSchedule(hb.Armed())
		if err != nil {
			t.Fatal(err)
		}
		review, err := ParseSchedule(MemoryReviewJob(name).Expr)
		if err != nil {
			t.Fatal(err)
		}
		day := time.Date(2026, 7, 25, 0, 0, 0, 0, time.Local)
		for m := 0; m < 24*60; m++ {
			at := day.Add(time.Duration(m) * time.Minute)
			if review.Matches(at) && beat.Matches(at) {
				t.Errorf("%s: memory review and heartbeat both fire at %s (review %q, beat %q)",
					name, at.Format("15:04"), MemoryReviewJob(name).Expr, hb.Armed())
			}
		}
	}
}

func TestHeartbeatArmFailureKeepsPreviousCadence(t *testing.T) {
	sched := &fakeBuiltin{}
	hb := NewHeartbeat("demetre", sched)
	if err := hb.Arm(); err != nil {
		t.Fatal(err)
	}
	base := hb.Armed()
	sched.fail = errRefused{}
	hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	// The bot keeps waking at the old cadence rather than losing its heartbeat.
	if hb.Armed() != base {
		t.Fatalf("a failed re-arm must leave the previous cadence in force, got %q", hb.Armed())
	}
}

type errRefused struct{}

func (errRefused) Error() string { return "refused" }

func TestHeartbeatArmReportsFailure(t *testing.T) {
	// Arm failing means the bot would run with no wake-up at all, so it must be reportable.
	hb := NewHeartbeat("amiran", &fakeBuiltin{fail: errRefused{}})
	if err := hb.Arm(); err == nil {
		t.Fatal("Arm must report a failure to install the heartbeat")
	}
}

func TestHeartbeatCadencesAreValidAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	// Any offset must produce a valid expression, whatever the modulus becomes later.
	for _, offset := range []int{0, 1, 19, 20, 41, 59} {
		for tier, cadence := range heartbeatCadences {
			expr := cadence(offset)
			if _, err := ParseSchedule(expr); err != nil {
				t.Fatalf("tier %d offset %d expr %q does not parse: %v", tier, offset, expr, err)
			}
		}
	}
	for tier, cadence := range heartbeatCadences {
		expr := cadence(7)
		if seen[expr] {
			t.Fatalf("tier %d repeats an earlier cadence %q", tier, expr)
		}
		seen[expr] = true
	}
}

func TestHeartbeatOffsetsDifferPerBot(t *testing.T) {
	// A fleet must not wake in lockstep, or every bot hits the provider at the same minute. Every
	// bot of the shipped fleet needs its own slot, not just two of them.
	fleet := []string{"amiran", "lisa", "kate", "jane", "demetre"}
	exprs := map[string]string{}
	for _, name := range fleet {
		hb, _ := newArmedHeartbeat(t, name)
		if other, clash := exprs[hb.Armed()]; clash {
			t.Errorf("%s and %s share wake minutes %q", name, other, hb.Armed())
		}
		exprs[hb.Armed()] = name
	}
}

func TestIsIdleAnswer(t *testing.T) {
	idle := []string{
		"IDLE", "idle", " IDLE ", "`IDLE`", "**IDLE**", "IDLE.", "\nIdle\n",
		"IDLE - still waiting on the QA mailbox",
		"IDLE: nothing assigned to me",
		"IDLE\nnothing to advance today",
		// The operating protocol trains NO_REPLY for "nothing to say", and an empty answer is the
		// same statement. Accepting only the marker would mean the backoff never engaged.
		"NO_REPLY", "no_reply", "", "   ",
	}
	for _, s := range idle {
		if !IsIdleAnswer(s) {
			t.Errorf("IsIdleAnswer(%q) = false, want true", s)
		}
	}
	notIdle := []string{
		"pushed 9e0ac9d",
		"IDLENESS is not the issue", "Not IDLE - I am working on #32",
		"advanced #32; the matrix now runs. IDLE",
	}
	for _, s := range notIdle {
		if IsIdleAnswer(s) {
			t.Errorf("IsIdleAnswer(%q) = true, want false", s)
		}
	}
}

func TestWorkHeartbeatJobShape(t *testing.T) {
	j := WorkHeartbeatJob("*/20 * * * *")
	if j.ID != WorkHeartbeatJobID {
		t.Fatalf("ID = %q", j.ID)
	}
	if j.At != "" {
		t.Fatal("the heartbeat must be recurring, not a one-shot")
	}
	if !strings.Contains(j.Prompt, HeartbeatIdleMarker) {
		t.Fatal("the prompt must tell the agent which marker means idle")
	}
	if !strings.Contains(j.Prompt, "NO_REPLY") {
		t.Fatal("the prompt must steer away from NO_REPLY, which the protocol otherwise trains")
	}
}

func TestSchedulerRefusesToDropTheHeartbeat(t *testing.T) {
	s, _ := NewScheduler(nil, nil)
	hb := NewHeartbeat("amiran", s)
	if err := hb.Arm(); err != nil {
		t.Fatal(err)
	}
	// The whole point of the heartbeat is that an agent cannot strand itself by removing it.
	if err := s.Remove(WorkHeartbeatJobID); err == nil {
		t.Fatal("removing the built-in heartbeat should be refused")
	}
	if err := s.Set(CronJob{ID: WorkHeartbeatJobID, Expr: "0 5 * * *", Prompt: "hijacked"}); err == nil {
		t.Fatal("replacing the built-in heartbeat should be refused")
	}
	if !s.IsBuiltin(WorkHeartbeatJobID) {
		t.Fatal("the heartbeat should report as built-in")
	}
	// The runtime itself must still be able to re-arm it at a new cadence.
	hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	list := s.List()
	if len(list) != 1 || list[0].Expr != hb.Armed() {
		t.Fatalf("re-arm did not reach the scheduler: %+v vs armed %q", list, hb.Armed())
	}
}

func TestNameOffsetIsStableAndBounded(t *testing.T) {
	for _, name := range []string{"amiran", "lisa", "kate", ""} {
		a, b := nameOffset(name, 20), nameOffset(name, 20)
		if a != b {
			t.Fatalf("nameOffset(%q) is not stable: %d then %d", name, a, b)
		}
		if a < 0 || a >= 20 {
			t.Fatalf("nameOffset(%q) = %d, out of range", name, a)
		}
	}
}
