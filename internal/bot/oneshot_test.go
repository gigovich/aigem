package bot

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestOneShotFiresOnceAndSelfDeletes(t *testing.T) {
	var saved [][]CronJob
	s, warns := NewScheduler(nil, func(j []CronJob) error {
		cp := append([]CronJob(nil), j...)
		saved = append(saved, cp)
		return nil
	})
	if len(warns) != 0 {
		t.Fatalf("warns: %v", warns)
	}
	fireAt := time.Now().UTC()
	if err := s.Set(CronJob{ID: "j1", At: fireAt.Format(time.RFC3339), Prompt: "do it"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	fired := make(chan struct{}, 4)
	s.SetRunner(func(context.Context, CronJob) { fired <- struct{}{} })

	// A tick at/after the fire time runs it once (asynchronously) and removes it synchronously.
	s.tick(context.Background(), fireAt.Add(time.Minute))
	if jobs := s.List(); len(jobs) != 0 {
		t.Fatalf("one-shot not self-deleted: %+v", jobs)
	}
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("one-shot did not fire")
	}
	// A later tick must not re-run it.
	s.tick(context.Background(), fireAt.Add(2*time.Minute))
	select {
	case <-fired:
		t.Fatal("one-shot fired again")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestOneShotNotDueYet(t *testing.T) {
	s, _ := NewScheduler(nil, nil)
	future := time.Now().UTC().Add(time.Hour)
	_ = s.Set(CronJob{ID: "j1", At: future.Format(time.RFC3339), Prompt: "later"})
	var runs int
	s.SetRunner(func(context.Context, CronJob) { runs++ })
	s.tick(context.Background(), time.Now())
	if runs != 0 {
		t.Fatalf("future one-shot fired early: runs = %d", runs)
	}
	if len(s.List()) != 1 {
		t.Fatal("future one-shot should still be scheduled")
	}
}

func TestScheduleToolDelayCreatesOneShot(t *testing.T) {
	s, _ := NewScheduler(nil, nil)
	tool := NewScheduleTool(s)
	out, err := tool.Run(context.Background(), json.RawMessage(
		`{"action":"set","id":"j1","delay":"30m","prompt":"finish #34 and report"}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	jobs := s.List()
	if len(jobs) != 1 || jobs[0].At == "" || jobs[0].Expr != "" {
		t.Fatalf("expected one-shot job, got %+v (out=%q)", jobs, out)
	}
	at, err := time.Parse(time.RFC3339, jobs[0].At)
	if err != nil {
		t.Fatalf("bad At: %v", err)
	}
	if d := time.Until(at); d < 25*time.Minute || d > 35*time.Minute {
		t.Fatalf("fire time off: %v from now", d)
	}
}

func TestScheduleToolRejectsBothOrNeither(t *testing.T) {
	s, _ := NewScheduler(nil, nil)
	tool := NewScheduleTool(s)
	for _, args := range []string{
		`{"action":"set","id":"j1","prompt":"p"}`,
		`{"action":"set","id":"j1","expr":"0 9 * * *","delay":"10m","prompt":"p"}`,
	} {
		if _, err := tool.Run(context.Background(), json.RawMessage(args)); err == nil {
			t.Fatalf("expected error for %s", args)
		}
	}
}
