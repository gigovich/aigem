package bot

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSchedulerSetRemoveListPersist(t *testing.T) {
	var saved [][]CronJob
	var mu sync.Mutex
	s, warns := NewScheduler(nil, func(jobs []CronJob) error {
		mu.Lock()
		saved = append(saved, append([]CronJob{}, jobs...))
		mu.Unlock()
		return nil
	})
	if len(warns) != 0 {
		t.Fatalf("unexpected warns: %v", warns)
	}
	if err := s.Set(CronJob{ID: "a", Expr: "0 9 * * *", Prompt: "morning"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(CronJob{ID: "a", Expr: "0 10 * * *", Prompt: "later"}); err != nil {
		t.Fatal(err) // replace
	}
	if err := s.Set(CronJob{ID: "bad", Expr: "not a cron", Prompt: "x"}); err == nil {
		t.Fatal("Set with bad expr should error and not persist")
	}
	list := s.List()
	if len(list) != 1 || list[0].ID != "a" || list[0].Prompt != "later" {
		t.Fatalf("List = %+v", list)
	}
	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("a"); err == nil {
		t.Fatal("removing a missing job should error")
	}
	if len(s.List()) != 0 {
		t.Fatal("List should be empty after Remove")
	}
}

func TestNewSchedulerWarnsOnBadJob(t *testing.T) {
	_, warns := NewScheduler([]CronJob{
		{ID: "ok", Expr: "* * * * *", Prompt: "p"},
		{ID: "bad", Expr: "nope", Prompt: "p"},
	}, nil)
	if len(warns) != 1 {
		t.Fatalf("expected 1 warning for the bad job, got %v", warns)
	}
}

func TestSchedulerTickFires(t *testing.T) {
	s, _ := NewScheduler([]CronJob{{ID: "j", Expr: "* * * * *", Prompt: "do it"}}, nil)
	fired := make(chan CronJob, 1)
	s.SetRunner(func(_ context.Context, job CronJob) { fired <- job })
	s.tick(context.Background(), time.Now())
	select {
	case j := <-fired:
		if j.ID != "j" {
			t.Fatalf("fired wrong job %q", j.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("job did not fire")
	}
}

func TestSchedulerSkipsRunningJob(t *testing.T) {
	s, _ := NewScheduler([]CronJob{{ID: "j", Expr: "* * * * *", Prompt: "p"}}, nil)
	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	s.SetRunner(func(_ context.Context, _ CronJob) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
	})
	s.tick(context.Background(), time.Now()) // starts the job (blocks in runner)
	time.Sleep(20 * time.Millisecond)
	s.tick(context.Background(), time.Now()) // same minute, job still running -> skipped
	time.Sleep(20 * time.Millisecond)
	close(release)
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("expected the still-running job to fire once, got %d", got)
	}
}

func TestSchedulerTickSkipsNonMatching(t *testing.T) {
	// A job that only runs at minute 0; tick at minute 30 must not fire it.
	s, _ := NewScheduler([]CronJob{{ID: "j", Expr: "0 * * * *", Prompt: "p"}}, nil)
	fired := make(chan CronJob, 1)
	s.SetRunner(func(_ context.Context, job CronJob) { fired <- job })
	s.tick(context.Background(), at(t, "2026-06-20 10:30"))
	select {
	case <-fired:
		t.Fatal("non-matching job should not fire")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSchedulerBuiltinJob(t *testing.T) {
	var saved [][]CronJob
	s, _ := NewScheduler(nil, func(jobs []CronJob) error {
		saved = append(saved, append([]CronJob{}, jobs...))
		return nil
	})
	if err := s.SetBuiltin(CronJob{ID: "builtin", Expr: "* * * * *", Prompt: "review"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(CronJob{ID: "mine", Expr: "0 9 * * *", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	for _, snap := range saved {
		for _, j := range snap {
			if j.ID == "builtin" {
				t.Fatal("builtin job must never be persisted")
			}
		}
	}
	if len(s.List()) != 2 {
		t.Fatalf("List must include the builtin, got %+v", s.List())
	}
	if err := s.Set(CronJob{ID: "builtin", Expr: "0 9 * * *", Prompt: "hijack"}); err == nil {
		t.Fatal("Set must refuse a builtin id")
	}
	if err := s.Remove("builtin"); err == nil {
		t.Fatal("Remove must refuse a builtin id")
	}

	fired := make(chan CronJob, 1)
	s.SetRunner(func(_ context.Context, job CronJob) { fired <- job })
	// A fixed instant that cannot match job "mine" (0 9 * * *), so only the builtin fires.
	s.tick(context.Background(), time.Date(2026, 7, 21, 10, 30, 0, 0, time.UTC))
	select {
	case job := <-fired:
		if job.ID != "builtin" {
			t.Fatalf("fired %q", job.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("builtin job did not fire")
	}
}

func TestSchedulerBuiltinDisplacesPersistedJob(t *testing.T) {
	var saved [][]CronJob
	s, _ := NewScheduler([]CronJob{{ID: "memory-review", Expr: "0 12 * * *", Prompt: "user's own"}},
		func(jobs []CronJob) error {
			saved = append(saved, append([]CronJob{}, jobs...))
			return nil
		})
	if err := s.SetBuiltin(MemoryReviewJob("testbot")); err != nil {
		t.Fatal(err)
	}
	list := s.List()
	if len(list) != 1 || list[0].Prompt == "user's own" {
		t.Fatalf("builtin must win over the persisted job: %+v", list)
	}
	if err := s.Set(CronJob{ID: "other", Expr: "0 9 * * *", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	for _, j := range saved[len(saved)-1] {
		if j.ID == "memory-review" {
			t.Fatal("displaced job must not be persisted back")
		}
	}
}

func TestSchedulerSetBuiltinTwiceNoDuplicate(t *testing.T) {
	s, _ := NewScheduler(nil, nil)
	j := CronJob{ID: "b", Expr: "* * * * *", Prompt: "p"}
	if err := s.SetBuiltin(j); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBuiltin(j); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 1 {
		t.Fatalf("List = %+v", s.List())
	}
}

func TestSchedulerSetBuiltinRejectsOneShot(t *testing.T) {
	s, _ := NewScheduler(nil, nil)
	j := CronJob{ID: "b", At: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), Prompt: "p"}
	if err := s.SetBuiltin(j); err == nil {
		t.Fatal("one-shot builtin must be rejected")
	}
}

func TestSchedulerBusyGateDefersEverything(t *testing.T) {
	var saved [][]CronJob
	var mu sync.Mutex
	at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	s, _ := NewScheduler([]CronJob{
		{ID: "recurring", Expr: "* * * * *", Prompt: "p"},
		{ID: "once", At: at, Prompt: "p"},
	}, func(jobs []CronJob) error {
		mu.Lock()
		saved = append(saved, append([]CronJob{}, jobs...))
		mu.Unlock()
		return nil
	})
	fired := make(chan CronJob, 4)
	s.SetRunner(func(_ context.Context, job CronJob) { fired <- job })

	busy := true
	s.SetBusy(func() bool { return busy })
	s.tick(context.Background(), time.Now())
	if len(fired) != 0 {
		t.Fatalf("busy gate should defer every due job, fired %d", len(fired))
	}
	// A deferred one-shot must survive: dropping it while busy would lose the work outright.
	if len(s.List()) != 2 {
		t.Fatalf("deferred jobs should stay scheduled, got %+v", s.List())
	}
	mu.Lock()
	persisted := len(saved)
	mu.Unlock()
	if persisted != 0 {
		t.Fatalf("a deferred tick must not persist anything, saved %d times", persisted)
	}

	// Once free, both eventually run - one per tick, because two fresh agents must never work
	// the same ticket at once.
	busy = false
	got := map[string]bool{}
	for tick := 0; tick < 2; tick++ {
		s.tick(context.Background(), time.Now())
		select {
		case j := <-fired:
			got[j.ID] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("tick %d fired nothing; got so far %v", tick, got)
		}
	}
	if !got["recurring"] || !got["once"] {
		t.Fatalf("expected both jobs across two ticks, got %v", got)
	}
}

// A recurring job is only due during its own minute, so a gated tick has to remember it. Without
// that, a daily job gated at its single minute silently skips the whole day.
func TestSchedulerHeldRecurringJobStillFires(t *testing.T) {
	s, _ := NewScheduler([]CronJob{{ID: "daily", Expr: "30 10 * * *", Prompt: "p"}}, nil)
	fired := make(chan CronJob, 2)
	s.SetRunner(func(_ context.Context, j CronJob) { fired <- j })
	busy := true
	s.SetBusy(func() bool { return busy })

	at1030 := time.Date(2026, 7, 25, 10, 30, 0, 0, time.Local)
	s.tick(context.Background(), at1030)
	if len(fired) != 0 {
		t.Fatal("the gate should have held the job back")
	}
	busy = false
	s.tick(context.Background(), at1030.Add(time.Minute)) // 10:31: its minute has passed
	select {
	case j := <-fired:
		if j.ID != "daily" {
			t.Fatalf("fired %q", j.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the held occurrence was lost instead of deferred")
	}
	// And it must not fire again on later ticks just because it was once held.
	s.tick(context.Background(), at1030.Add(2*time.Minute))
	select {
	case j := <-fired:
		t.Fatalf("a held job fired twice: %q", j.ID)
	case <-time.After(200 * time.Millisecond):
	}
}

// Two jobs due in the same minute must not start two agents at once.
func TestSchedulerFiresOneJobPerTick(t *testing.T) {
	s, _ := NewScheduler([]CronJob{
		{ID: "a", Expr: "* * * * *", Prompt: "p"},
		{ID: "b", Expr: "* * * * *", Prompt: "p"},
	}, nil)
	started := make(chan CronJob, 4)
	release := make(chan struct{})
	s.SetRunner(func(_ context.Context, j CronJob) { started <- j; <-release })
	s.tick(context.Background(), time.Now())
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("nothing fired")
	}
	select {
	case j := <-started:
		t.Fatalf("a second job ran concurrently: %q", j.ID)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
}

// A turn that never ends must not silence the built-in heartbeat forever.
func TestSchedulerBusyGateHasACeiling(t *testing.T) {
	s, _ := NewScheduler([]CronJob{{ID: "j", Expr: "* * * * *", Prompt: "p"}}, nil)
	fired := make(chan CronJob, 2)
	s.SetRunner(func(_ context.Context, j CronJob) { fired <- j })
	s.SetBusy(func() bool { return true })
	for i := 0; i < maxDeferredTicks; i++ {
		s.tick(context.Background(), time.Now())
		if len(fired) != 0 {
			t.Fatalf("fired at tick %d, before the ceiling", i)
		}
	}
	s.tick(context.Background(), time.Now())
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("past the ceiling the scheduler must fire despite the gate")
	}
}

func TestSchedulerBusyGateResetsDeferredCount(t *testing.T) {
	s, _ := NewScheduler([]CronJob{{ID: "j", Expr: "* * * * *", Prompt: "p"}}, nil)
	s.SetRunner(func(_ context.Context, _ CronJob) {})
	busy := true
	s.SetBusy(func() bool { return busy })
	for i := 0; i < 3; i++ {
		s.tick(context.Background(), time.Now())
	}
	s.mu.Lock()
	n := s.deferred
	s.mu.Unlock()
	if n != 3 {
		t.Fatalf("deferred = %d, want 3", n)
	}
	busy = false
	s.tick(context.Background(), time.Now())
	s.mu.Lock()
	n = s.deferred
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("deferred should reset once a tick runs, got %d", n)
	}
}

// Replacing a job must discard its held-back occurrence: firing it would run the NEW definition at
// a time its own expression does not name, which is a surprise duplicate run.
func TestSchedulerReplacementDropsHeldOccurrence(t *testing.T) {
	s, _ := NewScheduler([]CronJob{{ID: "j", Expr: "30 10 * * *", Prompt: "old"}}, nil)
	fired := make(chan CronJob, 2)
	s.SetRunner(func(_ context.Context, j CronJob) { fired <- j })
	busy := true
	s.SetBusy(func() bool { return busy })

	at := time.Date(2026, 7, 25, 10, 30, 0, 0, time.Local)
	s.tick(context.Background(), at) // held back by the gate
	if err := s.Set(CronJob{ID: "j", Expr: "0 3 * * *", Prompt: "new"}); err != nil {
		t.Fatal(err)
	}
	busy = false
	s.tick(context.Background(), at.Add(time.Minute))
	select {
	case j := <-fired:
		t.Fatalf("the replaced job fired outside its new schedule (prompt %q)", j.Prompt)
	case <-time.After(300 * time.Millisecond):
	}
	// It still fires at the time its new expression does name.
	s.tick(context.Background(), time.Date(2026, 7, 26, 3, 0, 0, 0, time.Local))
	select {
	case j := <-fired:
		if j.Prompt != "new" {
			t.Fatalf("fired the old definition: %q", j.Prompt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the replaced job never fired at its own time")
	}
}

// A held-back recurring occurrence must never revive a one-shot that replaced its id: the
// one-shot's instant has not arrived, and firing it also deletes it.
func TestSchedulerHeldFlagNeverFiresAnEarlyOneShot(t *testing.T) {
	var saved [][]CronJob
	s, _ := NewScheduler([]CronJob{{ID: "x", Expr: "30 10 * * *", Prompt: "recurring"}},
		func(jobs []CronJob) error { saved = append(saved, append([]CronJob{}, jobs...)); return nil })
	fired := make(chan CronJob, 2)
	s.SetRunner(func(_ context.Context, j CronJob) { fired <- j })
	busy := true
	s.SetBusy(func() bool { return busy })

	at := time.Date(2026, 7, 25, 10, 30, 0, 0, time.Local)
	s.tick(context.Background(), at) // held back
	tomorrow := at.Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if err := s.Set(CronJob{ID: "x", At: tomorrow, Prompt: "one-shot"}); err != nil {
		t.Fatal(err)
	}
	busy = false
	s.tick(context.Background(), at.Add(time.Minute))
	select {
	case j := <-fired:
		t.Fatalf("a one-shot due tomorrow fired today (%+v)", j)
	case <-time.After(300 * time.Millisecond):
	}
	if len(s.List()) != 1 {
		t.Fatalf("the one-shot was deleted before its time: %+v", s.List())
	}
}

// The ceiling's window belongs to work actually released, not to empty ticks.
func TestSchedulerCeilingWindowNotBurnedByEmptyTicks(t *testing.T) {
	s, _ := NewScheduler([]CronJob{{ID: "daily", Expr: "30 10 * * *", Prompt: "p"}}, nil)
	fired := make(chan CronJob, 2)
	s.SetRunner(func(_ context.Context, j CronJob) { fired <- j })
	s.SetBusy(func() bool { return true })

	// Wedge for the whole ceiling at a time nothing is due.
	quiet := time.Date(2026, 7, 25, 3, 0, 0, 0, time.Local)
	for i := 0; i <= maxDeferredTicks; i++ {
		s.tick(context.Background(), quiet.Add(time.Duration(i)*time.Minute))
	}
	if len(fired) != 0 {
		t.Fatal("nothing was due, so nothing should have fired")
	}
	// Now something comes due while still wedged: it must not need a fresh 90 ticks.
	s.tick(context.Background(), time.Date(2026, 7, 25, 10, 30, 0, 0, time.Local))
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("past the ceiling, the first job to come due should fire immediately")
	}
}

// A minute the scheduler never ticked (a slow tick, a suspended machine) must not lose its jobs.
func TestSchedulerCarriesForwardAnUntickedMinute(t *testing.T) {
	s, _ := NewScheduler([]CronJob{{ID: "daily", Expr: "30 10 * * *", Prompt: "p"}}, nil)
	fired := make(chan CronJob, 2)
	s.SetRunner(func(_ context.Context, j CronJob) { fired <- j })

	// 10:30 is never ticked; the loop only notices at 10:33.
	s.holdMissed(time.Date(2026, 7, 25, 10, 30, 0, 0, time.Local))
	s.tick(context.Background(), time.Date(2026, 7, 25, 10, 33, 0, 0, time.Local))
	select {
	case j := <-fired:
		if j.ID != "daily" {
			t.Fatalf("fired %q", j.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the skipped minute's job was lost")
	}
}

// The ceiling measures UNBROKEN gating. Counting gated ticks across separate turns would let two
// ordinary long turns add up to the ceiling and hand the next fire a free pass onto a live turn.
func TestSchedulerCeilingNeedsUnbrokenGating(t *testing.T) {
	s, _ := NewScheduler([]CronJob{{ID: "daily", Expr: "30 10 * * *", Prompt: "p"}}, nil)
	fired := make(chan CronJob, 2)
	s.SetRunner(func(_ context.Context, j CronJob) { fired <- j })
	busy := true
	s.SetBusy(func() bool { return busy })

	quiet := time.Date(2026, 7, 25, 3, 0, 0, 0, time.Local)
	tickAt := func(min int) { s.tick(context.Background(), quiet.Add(time.Duration(min)*time.Minute)) }

	// Two gated stretches that together exceed the ceiling, separated by one free minute. Neither
	// reaches it alone, so nothing may be released.
	first, second := maxDeferredTicks*2/3, maxDeferredTicks/2
	for i := 0; i < first; i++ {
		tickAt(i)
	}
	busy = false
	tickAt(first) // a free tick proves nothing is wedged
	busy = true
	for i := 0; i < second; i++ {
		tickAt(first + 1 + i)
	}
	// Still gated and still under the ceiling, so a job coming due now must be held, not fired.
	s.tick(context.Background(), time.Date(2026, 7, 25, 10, 30, 0, 0, time.Local))
	select {
	case j := <-fired:
		t.Fatalf("the gate was bypassed by accumulated gating across turns (fired %q)", j.ID)
	case <-time.After(300 * time.Millisecond):
	}
	s.mu.Lock()
	n := s.deferred
	s.mu.Unlock()
	if n > second+1 {
		t.Fatalf("deferred = %d; a free tick should have reset the count, so it should be about %d",
			n, second+1)
	}
}

// A backwards clock step must not leave the bookkeeping ahead of the wall clock, which would skip
// the minutes in between without carrying their jobs forward.
func TestSchedulerHandlesBackwardsClockWithoutLosingMinutes(t *testing.T) {
	s, _ := NewScheduler([]CronJob{{ID: "daily", Expr: "30 10 * * *", Prompt: "p"}}, nil)
	fired := make(chan CronJob, 2)
	s.SetRunner(func(_ context.Context, j CronJob) { fired <- j })

	// Simulate what Run does when it notices the clock went back: resynchronise, then tick the
	// real minute. The job due at 10:30 must still fire when 10:30 comes round again.
	s.holdMissed(time.Date(2026, 7, 25, 10, 30, 0, 0, time.Local))
	s.tick(context.Background(), time.Date(2026, 7, 25, 10, 25, 0, 0, time.Local))
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the held occurrence was lost across the clock step")
	}
}
