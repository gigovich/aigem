package runner_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/session"
	"github.com/gigovich/aigem/internal/store"
	"github.com/gigovich/aigem/internal/uisession"
)

// newRuns builds a registry whose sessions are real ones against a model that
// is not there: everything the table does happens before a turn, and a run
// nobody sends a message to never reaches the backend.
func newRuns(t *testing.T, path string, opened *atomic.Int64, released *atomic.Int64) *runner.Runs {
	t.Helper()
	return newRunsRecording(t, path, opened, released, nil)
}

// newRunsRecording is newRuns for the tests that need the session behind a run.
// The registry hands none out - that is the point of the type - so the test's
// own Open is what remembers it.
func newRunsRecording(t *testing.T, path string, opened, released *atomic.Int64,
	built *lastSession,
) *runner.Runs {
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
			if built != nil {
				built.set(s.Local)
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

// A daemon that was killed leaves records claiming sessions that went with the
// process. This is the case NewRuns' correction exists for, and the only way to
// reach it is to write the table a crash would have left.
func TestATableLeftByACrashComesBackClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	file := store.New[[]runner.Run](path)
	if err := file.Save([]runner.Run{
		{ID: "RUN-7", SessionID: "s-7", Mode: runner.ModeInteractive, Title: "mid-flight",
			Status: runner.RunOpen, Created: time.Now(), Updated: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	runs := newRuns(t, path, nil, nil)
	got, err := runs.Get("RUN-7")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != runner.RunClosed || got.Live {
		t.Errorf("run = %+v, want a record with no session", got)
	}
	if got.Title != "mid-flight" {
		t.Errorf("title = %q, want the one the crashed daemon recorded", got.Title)
	}
	// And the correction is written back, so a daemon that starts and dies
	// again does not have to make it a second time.
	table, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 1 || table[0].Status != runner.RunClosed {
		t.Errorf("table on disk = %+v, want the correction saved", table)
	}
}

// A run must not survive the shutdown that raced it. Nothing else would ever
// close it, and its SessionEnd hook would run against an environment the daemon
// has already torn down.
func TestARunOpenedWhileTheRegistryIsClosingIsClosedWithIt(t *testing.T) {
	cwd := project(t)
	var released atomic.Int64
	opening := make(chan struct{})
	proceed := make(chan struct{})
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			close(opening)
			<-proceed
			_, reg := newEnvAndTools(t, cwd)
			s := runner.NewSession(runner.Spec{Mode: req.Mode, Tools: reg, Backend: deadBackend()})
			return s, runner.Opened{Release: func() { released.Add(1) }}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	created := make(chan error, 1)
	go func() {
		_, err := runs.Create(context.Background(), runner.RunRequest{})
		created <- err
	}()
	<-opening

	closed := make(chan struct{})
	go func() {
		runs.Close()
		close(closed)
	}()
	// Close must not be able to finish while the session is still being built.
	select {
	case <-closed:
		t.Fatal("Close returned while a run was still being opened")
	case <-time.After(100 * time.Millisecond):
	}
	close(proceed)

	if err := <-created; !errors.Is(err, runner.ErrRunsClosed) {
		t.Errorf("Create during a shutdown = %v, want ErrRunsClosed", err)
	}
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("Close never returned")
	}
	// The promise Close makes: what it returns from is saved and released, not
	// asked to be.
	if released.Load() != 1 {
		t.Errorf("release ran %d times, want once for the run that was mid-flight",
			released.Load())
	}
}

// Two tabs pressing the same button, at the same moment. Only one of them may
// close the session, and the record has to end up consistent either way.
func TestConcurrentClosesCloseTheSessionOnce(t *testing.T) {
	var released atomic.Int64
	runs := newRuns(t, "", nil, &released)
	v := create(t, runs, runner.RunRequest{})

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = runs.CloseRun(v.ID)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("close %d = %v, want nil", i, err)
		}
	}
	if released.Load() != 1 {
		t.Errorf("release ran %d times, want once", released.Load())
	}
	if got, _ := runs.Get(v.ID); got.Live || got.Status != runner.RunClosed {
		t.Errorf("run = %+v, want a record with no session", got)
	}
}

// The conversation names itself on its first turn. A record that never learned
// its name is one the list shows as untitled forever, and a record without the
// session id cannot find its journal again after a restart.
func TestARunKeepsTheNameItsConversationGaveItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	runs := newRuns(t, path, nil, nil)
	v := create(t, runs, runner.RunRequest{})

	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "rename the widget factory"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	named, err := runs.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if named.Title == "" || named.SessionID == "" {
		t.Fatalf("run after a submit = %+v, want a name and a session id", named)
	}
	if err := runs.CloseRun(v.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := runs.Get(v.ID)
	if after.Title != named.Title || after.SessionID != named.SessionID {
		t.Errorf("closed run = %+v, want to keep %q / %q",
			after, named.Title, named.SessionID)
	}
	// And across a restart, which is the point of writing it down.
	second := newRuns(t, path, nil, nil)
	restarted, err := second.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Title != named.Title || restarted.SessionID != named.SessionID {
		t.Errorf("run after a restart = %+v, want to keep %q / %q",
			restarted, named.Title, named.SessionID)
	}
}

// The timeline of a run whose daemon is gone is only on disk. Reading it back
// is the reason uisession.ReadJournal exists.
func TestAClosedRunServesTheTimelineFromItsJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	first := newRuns(t, path, nil, nil)
	v := create(t, first, runner.RunRequest{})
	if err := first.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	live, err := first.Events(v.ID, 0, 0)
	if err != nil || len(live) == 0 {
		t.Fatalf("Events while live = %d, %v; want a timeline", len(live), err)
	}
	first.Close()

	// A different registry over the same table: no session, no ring, nothing
	// but the record and what was journalled.
	second := newRuns(t, path, nil, nil)
	got, err := second.Events(v.ID, 0, 0)
	if err != nil {
		t.Fatalf("Events after a restart: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("a closed run served an empty timeline; its journal was not read")
	}
	var found bool
	for _, ev := range got {
		if ev.Kind == uisession.KindUserMessage && ev.Text == "hello" {
			found = true
		}
	}
	if !found {
		t.Errorf("the journal did not carry the message that was sent: %+v", got)
	}
	// And the cursor still means the same thing against it.
	rest, err := second.Events(v.ID, got[len(got)-1].Seq, 0)
	if err != nil || len(rest) != 0 {
		t.Errorf("Events past the end = %d, %v; want an empty page", len(rest), err)
	}
}

// Every operation carries fields that only differ by name. A mapping that put
// the model reference where the approval id goes would compile, run, and be
// invisible until somebody used it.
func TestEachOperationReachesTheRightPartOfTheSession(t *testing.T) {
	built := &lastSession{}
	runs := newRunsRecording(t, "", nil, nil, built)
	v := create(t, runs, runner.RunRequest{})

	// submit: the text is what the conversation records.
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "the message"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	evs, err := runs.Events(v.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var said bool
	for _, ev := range evs {
		if ev.Kind == uisession.KindUserMessage && ev.Text == "the message" {
			said = true
		}
	}
	if !said {
		t.Errorf("no user message carrying the submitted text: %+v", evs)
	}

	// command: the name selects the handler and the args are what it is given,
	// which are two string fields next to each other in the op.
	var gotArgs string
	built.get(t).Handle("rename", func(args string) error {
		gotArgs = args
		return nil
	})
	if err := runs.Apply(v.ID, runner.RunOp{
		Op: runner.OpCommand, Name: "rename", Args: "the widget factory",
	}); err != nil {
		t.Fatalf("command: %v", err)
	}
	if gotArgs != "the widget factory" {
		t.Errorf("the handler was given %q, want the argument line", gotArgs)
	}
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpCommand, Name: "nosuchcommand"}); !errors.Is(
		err, uisession.ErrUnknownCommand) {
		t.Errorf("an unknown command = %v, want ErrUnknownCommand", err)
	}

	// resolve: an approval nobody is waiting on is refused, and refused as the
	// ordinary outcome it is rather than as something else.
	err = runs.Apply(v.ID, runner.RunOp{
		Op: runner.OpResolve, ID: "a1", Decision: uisession.DecisionOnce, By: "web",
	})
	if !errors.Is(err, uisession.ErrAlreadyDecided) {
		t.Errorf("resolve = %v, want ErrAlreadyDecided", err)
	}

	// switch_model: the ref is what is resolved, and a failed switch leaves the
	// record alone.
	before, _ := runs.Get(v.ID)
	err = runs.Apply(v.ID, runner.RunOp{Op: runner.OpSwitchModel, Ref: "nowhere/nothing"})
	if err == nil {
		t.Error("switching to a model that does not resolve succeeded")
	}
	if after, _ := runs.Get(v.ID); after.Model != before.Model {
		t.Errorf("model = %q after a failed switch, want %q", after.Model, before.Model)
	}
}

// A client can loop on create. Each run holds a registry, a model handle and an
// event ring, and without a ceiling that loop is an out-of-memory with nothing
// in the way of it.
func TestTheDaemonWillNotHoldAnUnboundedNumberOfRuns(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	var last error
	var opened []string
	for range 40 {
		v, err := runs.Create(context.Background(), runner.RunRequest{})
		if err != nil {
			last = err
			break
		}
		opened = append(opened, v.ID)
	}
	if !errors.Is(last, runner.ErrTooManyRuns) {
		t.Fatalf("creating 40 runs ended with %v, want ErrTooManyRuns", last)
	}
	// Closing one gives its place back: the ceiling is on what is live, not on
	// what has ever existed.
	if err := runs.CloseRun(opened[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Create(context.Background(), runner.RunRequest{}); err != nil {
		t.Errorf("Create after closing one = %v, want it to be allowed", err)
	}
}

// lastSession remembers the session the test's own Open built.
type lastSession struct {
	mu sync.Mutex
	l  *uisession.Local
}

func (s *lastSession) set(l *uisession.Local) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.l = l
}

func (s *lastSession) get(t *testing.T) *uisession.Local {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.l == nil {
		t.Fatal("no session has been opened yet")
	}
	return s.l
}

// slowModel answers a completion in several deltas, so a test can act while a
// turn is still running.
type slowModel struct {
	srv *httptest.Server
}

func newSlowModel(t *testing.T) *slowModel {
	t.Helper()
	m := &slowModel{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for range 4 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"word \"}}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *slowModel) ref(model string) *llm.Ref { return llm.NewRef(llm.New(m.srv.URL, model)) }

// A conversation is written at the end of every turn, by the turn. That is why
// closing a run does not save for itself - and it is the invariant that would
// have to be broken before that omission lost anybody's work.
func TestATurnPersistsItselfWithNobodyClosingTheRun(t *testing.T) {
	cwd := project(t)
	model := newSlowModel(t)
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			_, reg := newEnvAndTools(t, cwd)
			s := runner.NewSession(runner.Spec{Mode: req.Mode, Tools: reg, Backend: model.ref("m")})
			return s, runner.Opened{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	v := create(t, runs, runner.RunRequest{})
	events, detach, err := runs.Subscribe(v.ID, uisession.Client{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "say something"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitForKind(t, events, uisession.KindTurnEnd)

	// Nothing has closed the run, and nothing has asked it to save.
	got, err := runs.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The save happens just after the turn_end event rather than just before
	// it, so this waits for the write rather than assuming it has landed.
	var saved *session.Session
	deadline := time.Now().Add(10 * time.Second)
	for {
		saved, err = session.Load(got.SessionID)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the turn did not persist the conversation: %v", err)
	}
	var asked bool
	for _, m := range saved.Messages {
		if strings.Contains(m.Content, "say something") {
			asked = true
		}
	}
	if !asked {
		t.Errorf("the saved conversation does not hold the message that was sent: %+v", saved.Messages)
	}
}

// waitForKind reads the stream until the named event arrives.
func waitForKind(t *testing.T, events <-chan uisession.Event, kind uisession.Kind) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the stream ended before %s", kind)
			}
			if ev.Kind == kind {
				return
			}
		case <-deadline:
			t.Fatalf("%s never arrived", kind)
		}
	}
}
