package bot

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/gigovich/aigem/internal/agent"
)

type scriptedRunner struct {
	answer string
	err    error
	ran    int
	events agent.Events
}

func (r *scriptedRunner) Run(_ context.Context, _ string, ev agent.Events) (string, error) {
	r.ran++
	r.events = ev
	return r.answer, r.err
}

// busyProbe records the enter/release accounting a scheduled run owes the gate.
type busyProbe struct {
	mu       sync.Mutex
	entered  int
	released int
}

func (b *busyProbe) enter() func() {
	b.mu.Lock()
	b.entered++
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.released++
		b.mu.Unlock()
	}
}

func (b *busyProbe) counts() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.entered, b.released
}

// Without this accounting the busy gate only ever sees chat turns, so a long scheduled run could
// be joined by the next one - the thing the gate exists to prevent.
func TestCronRunnerCountsAsBusyAndReleases(t *testing.T) {
	probe := &busyProbe{}
	run := &scriptedRunner{answer: "did a thing"}
	NewCronRunner(slog.Default(), func() (Runner, error) { return run, nil }, nil, probe.enter, nil)(
		context.Background(), CronJob{ID: "j", Prompt: "p"})
	if in, out := probe.counts(); in != 1 || out != 1 {
		t.Fatalf("entered %d released %d, want 1 and 1", in, out)
	}
	if run.ran != 1 {
		t.Fatalf("the job ran %d times", run.ran)
	}
}

func TestCronRunnerReleasesBusyOnEveryFailurePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build AgentBuilder
	}{
		{"build fails", func() (Runner, error) { return nil, errors.New("no workdir") }},
		{"run fails", func() (Runner, error) { return &scriptedRunner{err: errors.New("429")}, nil }},
	} {
		probe := &busyProbe{}
		NewCronRunner(slog.Default(), tc.build, nil, probe.enter, nil)(context.Background(), CronJob{ID: "j"})
		if in, out := probe.counts(); in != 1 || out != 1 {
			t.Errorf("%s: entered %d released %d, want 1 and 1 (a leak wedges the gate forever)",
				tc.name, in, out)
		}
	}
}

func TestCronRunnerTagsRunsWithTheJobID(t *testing.T) {
	run := &scriptedRunner{answer: "ok"}
	NewCronRunner(slog.Default(), func() (Runner, error) { return run, nil }, nil, nil, nil)(
		context.Background(), CronJob{ID: "work-heartbeat", Prompt: "p"})
	if run.events.OnToolStart == nil {
		t.Fatal("a scheduled run must get step events, or it is invisible in the log")
	}
}

func TestCronRunnerFeedsTheHeartbeat(t *testing.T) {
	sched := &fakeBuiltin{}
	hb := NewHeartbeat("amiran", sched)
	if err := hb.Arm(); err != nil {
		t.Fatal(err)
	}
	base := hb.Armed()
	answer := HeartbeatIdleMarker
	runner := NewCronRunner(slog.Default(), func() (Runner, error) { return &scriptedRunner{answer: answer}, nil },
		hb, nil, nil)

	// Two idle heartbeat runs slow the cadence down.
	runner(context.Background(), CronJob{ID: WorkHeartbeatJobID})
	runner(context.Background(), CronJob{ID: WorkHeartbeatJobID})
	if hb.Armed() == base {
		t.Fatal("two idle heartbeat runs should have slowed the cadence")
	}
	// A run that reports work restores it.
	answer = "advanced #32: contract tests green"
	runner(context.Background(), CronJob{ID: WorkHeartbeatJobID})
	if hb.Armed() != base {
		t.Fatalf("a productive run should restore the working cadence, got %q", hb.Armed())
	}
}

func TestCronRunnerIgnoresOtherJobsForTheHeartbeat(t *testing.T) {
	sched := &fakeBuiltin{}
	hb := NewHeartbeat("lisa", sched)
	if err := hb.Arm(); err != nil {
		t.Fatal(err)
	}
	runner := NewCronRunner(slog.Default(),
		func() (Runner, error) { return &scriptedRunner{answer: HeartbeatIdleMarker}, nil }, hb, nil, nil)
	for i := 0; i < 6; i++ {
		runner(context.Background(), CronJob{ID: MemoryReviewJobID})
	}
	if hb.Tier() != 0 {
		t.Fatalf("another job's answer must not change the heartbeat tier, got %d", hb.Tier())
	}
}

// A provider refusing all day must not be paid for at the fastest cadence.
func TestCronRunnerBacksOffOnRepeatedFailure(t *testing.T) {
	sched := &fakeBuiltin{}
	hb := NewHeartbeat("kate", sched)
	if err := hb.Arm(); err != nil {
		t.Fatal(err)
	}
	base := hb.Armed()
	runner := NewCronRunner(slog.Default(),
		func() (Runner, error) { return &scriptedRunner{err: errors.New("429 usage limit")}, nil },
		hb, nil, nil)
	runner(context.Background(), CronJob{ID: WorkHeartbeatJobID})
	runner(context.Background(), CronJob{ID: WorkHeartbeatJobID})
	if hb.Armed() == base {
		t.Fatal("repeated failures should slow the heartbeat down")
	}
}

// A late idle verdict from a run that was already in flight must not climb back to the tier the
// conversation just pulled the bot out of. An odd idle count is the case that exposes it: plain
// subtraction would land one step short of the boundary and let the next idle undo the move.
func TestHeartbeatLateIdleCannotUndoAConversation(t *testing.T) {
	hb := NewHeartbeat("jane", &fakeBuiltin{})
	if err := hb.Arm(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ { // odd, so idles sits mid-tier
		hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	}
	before := hb.Tier()
	hb.Addressed()
	faster := hb.Tier()
	if faster >= before {
		t.Fatalf("being addressed should speed the heartbeat up: %d -> %d", before, faster)
	}
	// The run that was already in flight now reports idle.
	hb.AfterCronRun(WorkHeartbeatJobID, HeartbeatIdleMarker)
	if hb.Tier() != faster {
		t.Fatalf("a late idle verdict moved the tier from %d to %d", faster, hb.Tier())
	}
}

// The scored outcome is the one line that explains a bot's cadence after the fact; it was lost
// once already, when the runner moved out of the command layer.
func TestCronRunnerLogsTheScoredOutcome(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	hb := NewHeartbeat("amiran", &fakeBuiltin{})
	if err := hb.Arm(); err != nil {
		t.Fatal(err)
	}
	NewCronRunner(slog.Default(), func() (Runner, error) { return &scriptedRunner{answer: HeartbeatIdleMarker}, nil },
		hb, nil, nil)(context.Background(), CronJob{ID: WorkHeartbeatJobID})
	out := buf.String()
	if !strings.Contains(out, "heartbeat outcome") || !strings.Contains(out, "idle=true") {
		t.Fatalf("the run's score must be logged, got %q", out)
	}
}
