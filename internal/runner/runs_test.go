package runner_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/store"
	"github.com/gigovich/aigem/internal/uisession"
)

// newRuns builds a registry whose sessions are real ones against a model that
// is not there: everything the table does happens before a turn, and a run
// nobody sends a message to never reaches the backend.
func newRuns(t *testing.T, path string, opened *atomic.Int64, released *atomic.Int64) *runner.Runs {
	t.Helper()
	cwd := project(t)
	var file *store.File[[]runner.Run]
	if path != "" {
		file = store.New[[]runner.Run](path)
	}
	runs, err := runner.NewRuns(runner.RunsConfig{
		Store: file,
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			_, reg := newEnvAndTools(t, cwd)
			s := runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Backend: deadBackend(), Title: req.Title,
			})
			if opened != nil {
				opened.Add(1)
			}
			return s, runner.Opened{
				Model: "test/model", Root: cwd, Title: req.Title,
				Release: func() {
					if released != nil {
						released.Add(1)
					}
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRuns: %v", err)
	}
	t.Cleanup(runs.Close)
	return runs
}

func create(t *testing.T, runs *runner.Runs, req runner.RunRequest) runner.RunView {
	t.Helper()
	v, err := runs.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return v
}

// The table is the whole point of the type: an id a URL can name, and a record
// the next reader gets back.
func TestARunIsListedUnderTheIdItWasGiven(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	v := create(t, runs, runner.RunRequest{Title: "first"})
	if v.ID == "" || !v.Live || v.Status != runner.RunOpen {
		t.Fatalf("run = %+v, want a live, open run with an id", v)
	}
	if v.Mode != runner.ModeInteractive {
		t.Errorf("mode = %q, want the interactive default", v.Mode)
	}

	got, err := runs.Get(v.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != v.ID || got.Title != "first" {
		t.Errorf("Get = %+v, want the run that was created", got)
	}
	if list := runs.List(); len(list) != 1 || list[0].ID != v.ID {
		t.Errorf("List = %+v, want the one run", list)
	}
}

func TestRunsAreListedOldestFirst(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	var want []string
	for range 3 {
		want = append(want, create(t, runs, runner.RunRequest{}).ID)
	}
	var got []string
	for _, v := range runs.List() {
		got = append(got, v.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List = %v, want %v", got, want)
		}
	}
}

func TestAnUnknownRunIsRefusedByEveryOperation(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	if _, err := runs.Get("RUN-nope"); !errors.Is(err, runner.ErrNoRun) {
		t.Errorf("Get = %v, want ErrNoRun", err)
	}
	if _, err := runs.Events("RUN-nope", 0, 0); !errors.Is(err, runner.ErrNoRun) {
		t.Errorf("Events = %v, want ErrNoRun", err)
	}
	if _, _, err := runs.Subscribe("RUN-nope", uisession.Client{}, 0); !errors.Is(err, runner.ErrNoRun) {
		t.Errorf("Subscribe = %v, want ErrNoRun", err)
	}
	if err := runs.Apply("RUN-nope", runner.RunOp{Op: runner.OpInterrupt}); !errors.Is(err, runner.ErrNoRun) {
		t.Errorf("Apply = %v, want ErrNoRun", err)
	}
	if err := runs.CloseRun("RUN-nope"); !errors.Is(err, runner.ErrNoRun) {
		t.Errorf("CloseRun = %v, want ErrNoRun", err)
	}
}

// Autonomous runs need a ticket and a dedicated worktree, and neither exists
// yet. Opening one anyway would be a session with the autonomous policy and
// none of what the policy assumes.
func TestAModeWithNothingBehindItIsRefusedBeforeASessionIsBuilt(t *testing.T) {
	var opened atomic.Int64
	runs := newRuns(t, "", &opened, nil)
	_, err := runs.Create(context.Background(), runner.RunRequest{Mode: runner.ModeAutonomous})
	if !errors.Is(err, runner.ErrRunMode) {
		t.Fatalf("Create = %v, want ErrRunMode", err)
	}
	if opened.Load() != 0 {
		t.Error("a session was built for a mode the registry refuses")
	}
}

// Closing a run ends the session and keeps the record: a run a person is done
// with is one they can still read.
func TestClosingARunKeepsTheRecordAndReleasesWhatWasHeld(t *testing.T) {
	var released atomic.Int64
	runs := newRuns(t, "", nil, &released)
	v := create(t, runs, runner.RunRequest{})

	if err := runs.CloseRun(v.ID); err != nil {
		t.Fatalf("CloseRun: %v", err)
	}
	if released.Load() != 1 {
		t.Errorf("release ran %d times, want once", released.Load())
	}
	got, err := runs.Get(v.ID)
	if err != nil {
		t.Fatalf("Get after close: %v", err)
	}
	if got.Live || got.Status != runner.RunClosed {
		t.Errorf("run after close = %+v, want a record with no session", got)
	}

	// Two tabs pressing the same button is the ordinary case, and closing twice
	// must not release twice either.
	if err := runs.CloseRun(v.ID); err != nil {
		t.Errorf("second CloseRun: %v", err)
	}
	if released.Load() != 1 {
		t.Errorf("release ran %d times after two closes, want once", released.Load())
	}
}

// Everything that needs the live session is refused once it is gone, and says
// which of the two things went wrong.
func TestAClosedRunRefusesWhatNeedsItsSession(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	v := create(t, runs, runner.RunRequest{})
	if err := runs.CloseRun(v.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runs.Subscribe(v.ID, uisession.Client{}, 0); !errors.Is(err, runner.ErrRunClosed) {
		t.Errorf("Subscribe = %v, want ErrRunClosed", err)
	}
	if _, err := runs.Artifacts(v.ID); !errors.Is(err, runner.ErrRunClosed) {
		t.Errorf("Artifacts = %v, want ErrRunClosed", err)
	}
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpInterrupt}); !errors.Is(err, runner.ErrRunClosed) {
		t.Errorf("Apply = %v, want ErrRunClosed", err)
	}
	// The timeline is still readable: a session that never had a turn has no
	// journal, so this is an empty one rather than a failure.
	if _, err := runs.Events(v.ID, 0, 0); err != nil {
		t.Errorf("Events on a closed run = %v, want a readable timeline", err)
	}
}

// A subscriber is the stream a browser renders, resumed from where it got to.
func TestASubscriberGetsTheRunsEventsAndDetaches(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	v := create(t, runs, runner.RunRequest{})

	events, detach, err := runs.Subscribe(v.ID, uisession.Client{Kind: "web"}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Attaching is itself an event: the other clients are shown who is there.
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("the stream closed before it said anything")
		}
		if ev.Kind != uisession.KindPresence {
			t.Fatalf("first event = %q, want presence", ev.Kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived on the stream")
	}
	detach()

	replayed, err := runs.Events(v.ID, 0, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(replayed) == 0 {
		t.Fatal("the timeline is empty after a client attached and left")
	}
	if got, err := runs.Events(v.ID, replayed[len(replayed)-1].Seq, 0); err != nil || len(got) != 0 {
		t.Fatalf("Events past the end = %v, %v; want an empty page", got, err)
	}
}

func TestATimelinePageIsBoundedByItsLimit(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	v := create(t, runs, runner.RunRequest{})
	// Two clients attaching and leaving is four presence events, which is
	// enough to page.
	for range 2 {
		_, detach, err := runs.Subscribe(v.ID, uisession.Client{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		detach()
	}
	page, err := runs.Events(v.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("page = %d events, want 2", len(page))
	}
	if page[0].Seq >= page[1].Seq {
		t.Errorf("a page is out of order: %d then %d", page[0].Seq, page[1].Seq)
	}
}

// Step mode is the toggle a person sees, and it is the inverse of the session's
// auto mode. Getting that round the wrong way would approve every tool call for
// somebody who asked to be shown each one.
func TestStepModeIsTheInverseOfAutoApproval(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	v := create(t, runs, runner.RunRequest{})

	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpStepMode, On: false}); err != nil {
		t.Fatal(err)
	}
	if got, _ := runs.Get(v.ID); got.Step {
		t.Error("step mode reads as on after it was turned off")
	}
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpStepMode, On: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := runs.Get(v.ID); !got.Step {
		t.Error("step mode reads as off after it was turned on")
	}
}

// A fresh interactive run asks about every tool call. A toggle that started
// life at the wrong end would approve the first batch for somebody who never
// touched it.
func TestAnInteractiveRunStartsInStepMode(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	if v := create(t, runs, runner.RunRequest{}); !v.Step {
		t.Error("a new interactive run does not ask about tool calls")
	}
}

func TestAnUnknownOperationIsRefusedByName(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	v := create(t, runs, runner.RunRequest{})
	err := runs.Apply(v.ID, runner.RunOp{Op: "detonate"})
	if err == nil || !strings.Contains(err.Error(), "detonate") {
		t.Fatalf("Apply = %v, want the operation named back", err)
	}
}

// The table outlives the process, and the sessions do not. A run left marked
// open would offer the next daemon's client a socket onto nothing.
func TestARestartFindsTheRunsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	first := newRuns(t, path, nil, nil)
	v := create(t, first, runner.RunRequest{Title: "before the restart"})
	first.Close()

	second := newRuns(t, path, nil, nil)
	got, err := second.Get(v.ID)
	if err != nil {
		t.Fatalf("Get after a restart: %v", err)
	}
	if got.Title != "before the restart" {
		t.Errorf("title = %q, want the one it was created with", got.Title)
	}
	if got.Live || got.Status != runner.RunClosed {
		t.Errorf("run after a restart = %+v, want a record with no session", got)
	}
}

// A restarted daemon must not hand out an id whose journal it still holds.
func TestARestartDoesNotReuseAnId(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	first := newRuns(t, path, nil, nil)
	was := create(t, first, runner.RunRequest{}).ID
	first.Close()

	second := newRuns(t, path, nil, nil)
	if now := create(t, second, runner.RunRequest{}).ID; now == was {
		t.Fatalf("the second daemon handed out %q again", now)
	}
}

// Shutdown saves and ends every conversation, and says so in the table: a
// daemon that stopped is not a daemon holding sessions.
func TestCloseEndsEveryRunAndIsSafeTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	var released atomic.Int64
	runs := newRuns(t, path, nil, &released)
	a := create(t, runs, runner.RunRequest{})
	b := create(t, runs, runner.RunRequest{})

	runs.Close()
	runs.Close()
	if released.Load() != 2 {
		t.Errorf("release ran %d times, want once per run", released.Load())
	}
	for _, id := range []string{a.ID, b.ID} {
		got, err := runs.Get(id)
		if err != nil {
			t.Fatalf("Get after Close: %v", err)
		}
		if got.Live {
			t.Errorf("%s is still live after Close", id)
		}
	}
	if _, err := runs.Create(context.Background(), runner.RunRequest{}); !errors.Is(err, runner.ErrRunsClosed) {
		t.Errorf("Create after Close = %v, want ErrRunsClosed", err)
	}
}

// A daemon that could not find its state directory still holds conversations;
// it just forgets they happened.
func TestARegistryWithNoStoreStillWorks(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	if _, err := runs.Create(context.Background(), runner.RunRequest{}); err != nil {
		t.Fatalf("Create with no store: %v", err)
	}
}

func TestARegistryNeedsAWayToOpenASession(t *testing.T) {
	if _, err := runner.NewRuns(runner.RunsConfig{}); err == nil {
		t.Fatal("a registry with no Open was built")
	}
}
