package runner_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/agent"
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
			env, reg := newEnvAndTools(t, cwd)
			s := runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Backend: deadBackend(), Title: req.Title,
				// As the daemon builds one: the hooks runner is what gives a
				// conversation started over a fresh identity.
				Hooks: env.Hooks,
			})
			if opened != nil {
				opened.Add(1)
			}
			if built != nil {
				built.set(s.Local)
			}
			return s, runner.Opened{
				Model: "test/model", Root: cwd,
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
	// Enough that a map walk cannot come out in creation order by luck: with
	// three, Go's small-map rotation hands the right answer about one run in
	// three.
	for range 10 {
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

// An oversized tool result is only ever shown as its head, and the whole of it
// is on disk under the seq of the event that was trimmed. Fetching it has to
// work while the run is live and after the daemon that produced it is gone,
// because those are the two states a page opens a run in.
func TestABlobIsFetchableWhileLiveAndAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	built := &lastSession{}
	first := newRunsRecording(t, path, nil, nil, built)
	v := create(t, first, runner.RunRequest{})

	events, detach, err := first.Subscribe(v.ID, uisession.Client{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer detach()

	// A turn driven from a closure, which is how a skill or a command runs one:
	// what matters here is a tool result the journal has to trim.
	big := strings.Repeat("x", 8<<10)
	err = built.get(t).Run("grep", "grep", func(_ context.Context, ev agent.Events) (string, error) {
		ev.OnToolEnd("call-1", "grep", big, nil)
		return "done", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, events, uisession.KindTurnEnd)

	seq := trimmedToolResult(t, first, v.ID, len(big))
	body, err := first.Blob(v.ID, seq)
	if err != nil {
		t.Fatalf("Blob on a live run: %v", err)
	}
	if body != big {
		t.Fatalf("the blob is %d bytes, want %d", len(body), len(big))
	}
	// A seq that was never trimmed has nothing stored, and says so rather than
	// answering with an empty body.
	if _, err := first.Blob(v.ID, seq+1000); !errors.Is(err, runner.ErrNoBlob) {
		t.Errorf("Blob for an unstored seq = %v, want ErrNoBlob", err)
	}
	if _, err := first.Blob("no-such-run", seq); !errors.Is(err, runner.ErrNoRun) {
		t.Errorf("Blob for an unknown run = %v, want ErrNoRun", err)
	}
	first.Close()

	// A different registry over the same table: no session, nothing but the
	// record and what was written beside the journal.
	second := newRuns(t, path, nil, nil)
	body, err = second.Blob(v.ID, seq)
	if err != nil {
		t.Fatalf("Blob after a restart: %v", err)
	}
	if body != big {
		t.Fatalf("the blob after a restart is %d bytes, want %d", len(body), len(big))
	}
}

// A run that never had a turn has a session id - it is stamped when the session
// is built - but no journal, because that is written from the first turn. There
// is nothing to read, and the answer is a missing body rather than the raw
// not-exist error from somewhere inside the state directory.
func TestABlobIsRefusedForARunThatHasNotHadATurn(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	v := create(t, runs, runner.RunRequest{})
	if _, err := runs.Blob(v.ID, 1); !errors.Is(err, runner.ErrNoBlob) {
		t.Fatalf("Blob before the first turn = %v, want ErrNoBlob", err)
	}
}

// trimmedToolResult finds the seq of the tool result the journal trimmed, and
// fails the test unless the run actually recorded one - a Blob that returned
// the right bytes for the wrong reason would otherwise pass.
func trimmedToolResult(t *testing.T, runs *runner.Runs, id string, full int) uint64 {
	t.Helper()
	evs, err := runs.Events(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Kind != uisession.KindToolEnd {
			continue
		}
		if !ev.Blob || ev.Bytes != full || len(ev.Text) >= full {
			t.Fatalf("tool result = %d bytes blob=%v bytes=%d, want a trimmed head "+
				"promising a stored body of %d", len(ev.Text), ev.Blob, ev.Bytes, full)
		}
		return ev.Seq
	}
	t.Fatalf("no tool result in the timeline: %+v", evs)
	return 0
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

	// resolve: an id this session never asked about is refused, and refused as
	// what it is. "Already decided" is the other refusal - two people answering
	// at once - and telling them apart is telling somebody who got there first
	// from somebody who answered the wrong conversation.
	err = runs.Apply(v.ID, runner.RunOp{
		Op: runner.OpResolve, ID: "a1", Decision: uisession.DecisionOnce, By: "web",
	})
	if !errors.Is(err, uisession.ErrNoApproval) {
		t.Errorf("resolve = %v, want ErrNoApproval", err)
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

// A daemon that is killed never closes its runs. What the table holds at that
// moment is all the next one gets, so the record has to learn the session's id
// and name when the conversation gets them - not when somebody closes it.
//
// The second registry is opened over the same file with the first still live,
// which is what a crash looks like from the next daemon's side.
func TestARecordLearnsItsSessionWithoutAnythingBeingClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	runs := newRuns(t, path, nil, nil)
	v := create(t, runs, runner.RunRequest{})
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "name this"}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Nothing has been closed. Everything below reads the file.
	after := newRuns(t, path, nil, nil)
	got, err := after.Get(v.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SessionID == "" {
		t.Error("the record does not name the session; its journal cannot be found again")
	}
	if got.Title == "" {
		t.Error("the record has no title; the run lists as untitled forever")
	}
}

// The ordinary message must not rewrite the table. It is the one operation that
// happens over and over, and the whole table is rewritten atomically each time.
func TestASecondMessageDoesNotRewriteTheTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	runs := newRuns(t, path, nil, nil)
	v := create(t, runs, runner.RunRequest{})
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "first"}); err != nil {
		t.Fatal(err)
	}
	first := modTime(t, path)

	// Far enough apart that a write cannot land inside the filesystem's
	// timestamp resolution.
	time.Sleep(20 * time.Millisecond)
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "second"}); err != nil {
		t.Fatal(err)
	}
	if again := modTime(t, path); !again.Equal(first) {
		t.Errorf("the table was rewritten for a message that changed nothing about it")
	}
}

func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

// A run is closed when the record says so, not only when the live session is
// gone. A client filtering on status would otherwise see every run this daemon
// ever held as open.
func TestAClosedRunSaysSoInTheRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	runs := newRuns(t, path, nil, nil)
	a := create(t, runs, runner.RunRequest{})
	b := create(t, runs, runner.RunRequest{})

	if err := runs.CloseRun(a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := runs.Get(a.ID); got.Status != runner.RunClosed {
		t.Errorf("status after CloseRun = %q, want %q", got.Status, runner.RunClosed)
	}
	runs.Close()
	if got, _ := runs.Get(b.ID); got.Status != runner.RunClosed {
		t.Errorf("status after Close = %q, want %q", got.Status, runner.RunClosed)
	}
	// And on disk, which is what the next daemon reads before it corrects
	// anything.
	table, err := store.New[[]runner.Run](path).Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range table {
		if rec.Status != runner.RunClosed {
			t.Errorf("%s is %q on disk after a shutdown, want %q",
				rec.ID, rec.Status, runner.RunClosed)
		}
	}
}

// Whatever Open allocated before failing is still the caller's to give back.
func TestAFailedOpenReleasesWhatItAllocated(t *testing.T) {
	var released atomic.Int64
	fail := errors.New("the provider is not signed in")
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(context.Context, runner.RunRequest) (*runner.Session, runner.Opened, error) {
			return nil, runner.Opened{Release: func() { released.Add(1) }}, fail
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)
	if _, err := runs.Create(context.Background(), runner.RunRequest{}); !errors.Is(err, fail) {
		t.Fatalf("Create = %v, want the failure Open reported", err)
	}
	if released.Load() != 1 {
		t.Errorf("release ran %d times after a failed open, want once", released.Load())
	}
}

// An Open that reports success without a session is a wiring mistake, and the
// run must not join the table on the strength of it.
func TestAnOpenThatBuiltNoSessionIsRefusedAndReleased(t *testing.T) {
	var released atomic.Int64
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(context.Context, runner.RunRequest) (*runner.Session, runner.Opened, error) {
			return nil, runner.Opened{Release: func() { released.Add(1) }}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)
	if _, err := runs.Create(context.Background(), runner.RunRequest{}); err == nil {
		t.Fatal("a run with no session was accepted")
	}
	if released.Load() != 1 {
		t.Errorf("release ran %d times, want once", released.Load())
	}
	if list := runs.List(); len(list) != 0 {
		t.Errorf("the table holds %+v, want nothing", list)
	}
}

// The ceiling has to count the runs being built as well as the ones that have
// arrived. A page opening several tabs at once, or any client issuing its POSTs
// in parallel, walks straight past a check that only counts finished ones - and
// the window is the whole of however long opening a session takes.
func TestTheCeilingHoldsAgainstCreatesInFlight(t *testing.T) {
	cwd := project(t)
	release := make(chan struct{})
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			// Every open is still in flight until the test lets them all go, so
			// nothing has reached the table while the checks are being made.
			<-release
			_, reg := newEnvAndTools(t, cwd)
			return runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Backend: deadBackend(),
			}), runner.Opened{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	const tries = 60
	var wg sync.WaitGroup
	errs := make([]error, tries)
	for i := range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = runs.Create(context.Background(), runner.RunRequest{})
		}()
	}
	// Let them all get past the check before any of them finishes.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	var made, refused int
	for _, err := range errs {
		switch {
		case err == nil:
			made++
		case errors.Is(err, runner.ErrTooManyRuns):
			refused++
		default:
			t.Fatalf("Create = %v, want either a run or ErrTooManyRuns", err)
		}
	}
	if made > 32 {
		t.Errorf("%d runs were opened at once, and the limit is 32", made)
	}
	if refused == 0 {
		t.Fatal("nothing was refused; the test never reached the ceiling")
	}
	if live := len(runs.List()); live != 32 {
		// Exactly, not at most: a count that could be transiently high refuses
		// creates while slots are free, and tells the person "44 are open" out
		// of a limit of 32.
		t.Errorf("%d runs were opened, want the ceiling filled exactly", live)
	}
	// And the sentence it refused with is one a person can act on.
	for _, err := range errs {
		if errors.Is(err, runner.ErrTooManyRuns) {
			if !strings.Contains(err.Error(), "32 is the limit") {
				t.Errorf("refusal = %v, want it to name the limit it is at", err)
			}
			break
		}
	}
}

// Two runs opened at the same moment must not be listed in an order their own
// timestamps contradict: a page that sorts by Created would shuffle them.
func TestCreatedNeverContradictsTheOrderRunsWereOpenedIn(t *testing.T) {
	runs := newRuns(t, "", nil, nil)
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := runs.Create(context.Background(), runner.RunRequest{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	list := runs.List()
	for i := 1; i < len(list); i++ {
		if list[i].Created.Before(list[i-1].Created) {
			t.Fatalf("%s is listed after %s but was created %v earlier",
				list[i].ID, list[i-1].ID, list[i-1].Created.Sub(list[i].Created))
		}
	}
}

// A second Close must not return while the first is still working. The caller
// that returns from it goes on to tear down the environment those sessions
// need, and "somebody else is already doing it" is not the same answer as "it
// is done".
func TestASecondCloseWaitsForTheFirst(t *testing.T) {
	cwd := project(t)
	var finished atomic.Int64
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			_, reg := newEnvAndTools(t, cwd)
			s := runner.NewSession(runner.Spec{Mode: req.Mode, Tools: reg, Backend: deadBackend()})
			return s, runner.Opened{Release: func() {
				// Slow enough that a caller which returned early would be seen
				// doing so, rather than the test depending on scheduling.
				time.Sleep(200 * time.Millisecond)
				finished.Store(time.Now().UnixNano())
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Create(context.Background(), runner.RunRequest{}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	returned := make([]int64, 4)
	for i := range returned {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runs.Close()
			returned[i] = time.Now().UnixNano()
		}()
	}
	wg.Wait()

	done := finished.Load()
	if done == 0 {
		t.Fatal("the shutdown never released the run")
	}
	for i, at := range returned {
		if at < done {
			t.Errorf("Close %d returned %v before the shutdown finished",
				i, time.Duration(done-at))
		}
	}
}

// The live half of a view is what a page draws while the conversation is
// running, and every field of it is read off a different part of the session. A
// view that dropped one would show a run that never seems to be doing anything.
func TestAViewReportsWhatOnlyTheLiveSessionKnows(t *testing.T) {
	built := &lastSession{}
	runs := newRunsRecording(t, "", nil, nil, built)
	v := create(t, runs, runner.RunRequest{Title: "opened as"})

	// Before anything has happened: the record's own title, and no sequence.
	if v.Title != "opened as" {
		t.Errorf("title = %q, want the one it was created with", v.Title)
	}
	if v.Seq != 0 || v.Waiting {
		t.Errorf("a fresh run = %+v, want nothing emitted and nobody waiting", v)
	}

	// A client attaching is an event, so the sequence has to move.
	_, detach, err := runs.Subscribe(v.ID, uisession.Client{Kind: "web"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	got, err := runs.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq == 0 {
		t.Error("the view reports no sequence after the session emitted an event")
	}
	if got.SessionID == "" {
		t.Error("the view does not name the session")
	}
	if got.Title != "opened as" {
		t.Errorf("title = %q; a session that has not named itself must not blank the record",
			got.Title)
	}

	_ = built
}

// A run parked on an approval is the state the whole interface is built around:
// it is what the status bar counts and what tells "thinking" apart from
// "waiting for somebody who walked away".
func TestAViewSaysWhenARunIsWaitingOnAnApproval(t *testing.T) {
	cwd := project(t)
	model := newFakeModel(t)
	// A tool that asks: reading is not gated, writing is.
	model.script(sseToolCall("write_file", `{"path":"notes.md","content":"hello"}`))
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			_, reg := newEnvAndTools(t, cwd)
			return runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Backend: model.ref("m"),
			}), runner.Opened{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	v := create(t, runs, runner.RunRequest{})
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "read it"}); err != nil {
		t.Fatal(err)
	}
	// The turn parks on the approval the tool call needs, and stays there:
	// nobody answers it.
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := runs.Get(v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Waiting {
			if !got.Running {
				t.Error("a run parked on an approval reports no turn in flight")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never reported waiting on an approval: %+v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The record's title is the last name the conversation had. A conversation
// started over has none until it is used again, and a view that copied that
// emptiness would blank a run in the list for no reason a person could see.
func TestARecordKeepsItsNameWhenTheConversationStartsOver(t *testing.T) {
	built := &lastSession{}
	runs := newRunsRecording(t, "", nil, nil, built)
	v := create(t, runs, runner.RunRequest{Title: "the first thing"})
	if v.Title != "the first thing" {
		t.Fatalf("title = %q, want the one the run was opened with", v.Title)
	}

	// Reset gives the conversation a fresh identity and no name.
	sess := built.get(t)
	if err := sess.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if name := sess.Meta().Title; name != "" {
		t.Fatalf("the conversation named itself %q; this test needs it nameless", name)
	}
	got, err := runs.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "the first thing" {
		t.Errorf("title = %q after the conversation started over, want the name it had", got.Title)
	}
}

// A conversation that starts over takes a new id, and the journal follows it.
// A view still reporting the old one would send a reader to the timeline of a
// conversation that is no longer there.
func TestAViewFollowsTheSessionToANewIdentity(t *testing.T) {
	built := &lastSession{}
	runs := newRunsRecording(t, "", nil, nil, built)
	v := create(t, runs, runner.RunRequest{})
	before, err := runs.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := built.get(t).Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	after, err := runs.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.SessionID == "" || after.SessionID == before.SessionID {
		t.Errorf("session id = %q after the conversation started over, want a new one (was %q)",
			after.SessionID, before.SessionID)
	}
}

// The model a run switched to is what the record has to say afterwards, and it
// has to survive the daemon: a page that lists the old one is showing a fact
// that stopped being true.
func TestASuccessfulModelSwitchIsRecordedAndPersisted(t *testing.T) {
	cwd := project(t)
	model := newFakeModel(t)
	models := testModelRegistry(t, cwd, model)
	path := filepath.Join(t.TempDir(), "runs.json")
	runs, err := runner.NewRuns(runner.RunsConfig{
		Store: store.New[[]runner.Run](path),
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			_, reg := newEnvAndTools(t, cwd)
			s := runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Backend: model.ref("first"), Models: models,
			})
			return s, runner.Opened{Model: "local/first"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	v := create(t, runs, runner.RunRequest{})
	if v.Model != "local/first" {
		t.Fatalf("model = %q, want the one the run was opened with", v.Model)
	}
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSwitchModel, Ref: "local/second"}); err != nil {
		t.Fatalf("switch_model: %v", err)
	}
	got, err := runs.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "local/second" {
		t.Errorf("model = %q after the switch, want local/second", got.Model)
	}
	// And on disk, without anything being closed.
	table, err := store.New[[]runner.Run](path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 1 || table[0].Model != "local/second" {
		t.Errorf("the table records %+v, want the model it switched to", table)
	}
}

// testModelRegistry offers two models on one fake endpoint, so a switch has
// somewhere to switch to.
func testModelRegistry(t *testing.T, cwd string, model *fakeModel) *llm.Registry {
	t.Helper()
	reg, warns := llm.NewRegistry(cwd, llm.Provider{
		ID: llm.LocalProviderID, BaseURL: model.srv.URL,
		API: llm.APICompletions, Auth: llm.AuthNone,
		Models: []llm.ModelInfo{
			{ID: "first", Provider: llm.LocalProviderID, ContextWindow: 8192},
			{ID: "second", Provider: llm.LocalProviderID, ContextWindow: 8192},
		},
	})
	for _, w := range warns {
		t.Logf("models config: %s", w)
	}
	return reg
}

// A conversation ends where its environment does. Releasing first would run the
// SessionEnd hook against an environment already torn down.
func TestAReleaseHappensAfterTheSessionHasEnded(t *testing.T) {
	cwd := project(t)
	var closedFirst bool
	var released bool
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			_, reg := newEnvAndTools(t, cwd)
			s := runner.NewSession(runner.Spec{Mode: req.Mode, Tools: reg, Backend: deadBackend()})
			// Asked of the session itself rather than watched for on the event
			// stream: a subscriber notices the close on a goroutine the
			// scheduler owes nothing to, so a test built on that observation
			// passes or fails by timing rather than by order.
			return s, runner.Opened{Release: func() {
				released = true
				_, _, err := s.Local.Subscribe(uisession.Client{}, 0)
				closedFirst = errors.Is(err, uisession.ErrClosed)
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	v := create(t, runs, runner.RunRequest{})
	if err := runs.CloseRun(v.ID); err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("the environment the run worked in was never released")
	}
	if !closedFirst {
		t.Error("the session was still open when what it ran in was released")
	}
}

// Two runs must never be handed the same id: the journal is keyed by it, and a
// second run answering to a first one's URL would show its conversation.
func TestAFailedOpenDoesNotReturnItsIdToThePool(t *testing.T) {
	cwd := project(t)
	var fail atomic.Bool
	fail.Store(true)
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			if fail.Load() {
				return nil, runner.Opened{}, errors.New("not signed in")
			}
			_, reg := newEnvAndTools(t, cwd)
			return runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Backend: deadBackend(),
			}), runner.Opened{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	if _, err := runs.Create(context.Background(), runner.RunRequest{}); err == nil {
		t.Fatal("the failing open succeeded")
	}
	fail.Store(false)
	first := create(t, runs, runner.RunRequest{}).ID
	second := create(t, runs, runner.RunRequest{}).ID
	if first == second {
		t.Fatalf("two runs were handed %q", first)
	}
	if first == "RUN-1" {
		t.Errorf("id = %q, which the failed open had already been given", first)
	}
}

// A table with two rows under one id leaves one of them unreachable, and a row
// with no id is unreachable already. Neither is something a daemon wrote, and
// both are corrected rather than carried.
func TestATableWithUnreachableRowsIsCorrected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	file := store.New[[]runner.Run](path)
	now := time.Now()
	if err := file.Save([]runner.Run{
		{ID: "RUN-1", Title: "the one that stays", Status: runner.RunClosed, Created: now},
		{ID: "RUN-1", Title: "the shadow", Status: runner.RunClosed, Created: now},
		{ID: "", Title: "unreachable", Status: runner.RunClosed, Created: now},
	}); err != nil {
		t.Fatal(err)
	}

	runs := newRuns(t, path, nil, nil)
	list := runs.List()
	if len(list) != 1 {
		t.Fatalf("the table holds %+v, want the one reachable row", list)
	}
	if list[0].Title != "the one that stays" {
		t.Errorf("kept %q, want the first row under the id", list[0].Title)
	}
	table, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 1 {
		t.Errorf("the file still holds %d rows, want the correction written back", len(table))
	}
}

// Most of what changes about a run does not happen during an HTTP request. A
// conversation takes its name on the first message, which arrives up a socket,
// and a daemon that only announced what its own routes did would leave every
// other tab showing an untitled run until something else moved.
func TestEveryChangeToARecordIsAnnounced(t *testing.T) {
	cwd := project(t)
	model := newFakeModel(t)
	models := testModelRegistry(t, cwd, model)

	var mu sync.Mutex
	var seen []runner.RunView
	runs, err := runner.NewRuns(runner.RunsConfig{
		Notify: func(v runner.RunView) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, v)
		},
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			env, reg := newEnvAndTools(t, cwd)
			return runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Backend: model.ref("first"),
				Models: models, Hooks: env.Hooks,
			}), runner.Opened{Model: "local/first"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	// The last announcement is what a page would have applied. Asserting on it
	// rather than on a running total keeps this about "was this change
	// announced" rather than about how many times anything fired.
	last := func() (runner.RunView, int) {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			return runner.RunView{}, 0
		}
		return seen[len(seen)-1], len(seen)
	}

	v, err := runs.Create(context.Background(), runner.RunRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := last()
	if got.ID != v.ID {
		t.Fatalf("opening a run announced %+v, want the run itself", got)
	}

	// The first message is where the conversation gets its name.
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "name it"}); err != nil {
		t.Fatal(err)
	}
	got, _ = last()
	if got.Title == "" || got.ID != v.ID {
		t.Fatalf("the first message announced %+v, want the run with the name it took", got)
	}

	waitIdle(t, runs, v.ID)
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSwitchModel, Ref: "local/second"}); err != nil {
		t.Fatal(err)
	}
	got, _ = last()
	if got.Model != "local/second" {
		t.Fatalf("a model switch announced %+v, want the new model", got)
	}

	if err := runs.CloseRun(v.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = last()
	if got.ID != v.ID || got.Live || got.Status != runner.RunClosed {
		t.Fatalf("closing announced %+v, want a record with no session", got)
	}

	// A second message announces the turn starting and the turn ending, and
	// nothing else: Running is read off the live session, so a record that says
	// a conversation is running has changed when it stops. What must not happen
	// is an announcement per keystroke-sized action, and a turn is not one.
	second, err := runs.Create(context.Background(), runner.RunRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runs.Apply(second.ID, runner.RunOp{Op: runner.OpSubmit, Text: "first"}); err != nil {
		t.Fatal(err)
	}
	// Each turn is let finish before the next: a second message under a running
	// one is refused, and a turn still in flight when the cleanup closes the
	// registry has its request to the provider cut off mid-body.
	waitIdle(t, runs, second.ID)
	_, before := waitAnnounced(t, last, 0)
	if err := runs.Apply(second.ID, runner.RunOp{Op: runner.OpSubmit, Text: "second"}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, runs, second.ID)
	got, after := waitAnnounced(t, last, before+2)
	if after != before+2 {
		t.Errorf("a second message announced %d changes, want the turn's start and end",
			after-before)
	}
	if got.Running {
		t.Error("the last announcement of a finished turn still says the run is running")
	}
}

// waitAnnounced blocks until at least want announcements have been made, and
// returns the last one. The watcher that announces a turn's end is a goroutine,
// so a test that read straight away would be asking before it had run.
func waitAnnounced(
	t *testing.T,
	last func() (runner.RunView, int),
	want int,
) (runner.RunView, int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, n := last()
		if n >= want || time.Now().After(deadline) {
			return v, n
		}
		time.Sleep(time.Millisecond)
	}
}

// waitIdle blocks until the run has no turn in flight.
func waitIdle(t *testing.T, runs *runner.Runs, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		got, err := runs.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the turn never ended")
}

// Switching model under a running turn is refused: the turn already started
// against the model being replaced, and the switch would read the agent's
// messages while the turn is writing them.
func TestAModelSwitchIsRefusedWhileATurnIsRunning(t *testing.T) {
	cwd := project(t)
	model := newSlowModel(t)
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			_, reg := newEnvAndTools(t, cwd)
			return runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Backend: model.ref("m"),
			}), runner.Opened{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	v := create(t, runs, runner.RunRequest{})
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "take a while"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := runs.Get(v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn never started")
		}
		time.Sleep(time.Millisecond)
	}
	if err := runs.Apply(v.ID, runner.RunOp{
		Op: runner.OpSwitchModel, Ref: "local/whatever",
	}); !errors.Is(err, uisession.ErrBusy) {
		t.Fatalf("switch_model mid-turn = %v, want ErrBusy", err)
	}
}

// runsAgainst builds a registry whose every run is one session against backend,
// which is what the tests below need and newRuns deliberately does not give:
// its sessions talk to a provider that is not there.
func runsAgainst(t *testing.T, cwd string, spec runner.Spec) *runner.Runs {
	t.Helper()
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			_, reg := newEnvAndTools(t, cwd)
			s := spec
			s.Mode, s.Tools = req.Mode, reg
			return runner.NewSession(s), runner.Opened{Model: "test/model", Root: cwd}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRuns: %v", err)
	}
	t.Cleanup(runs.Close)
	return runs
}

// subscribeRun attaches to a run the way the run socket does.
func subscribeRun(t *testing.T, runs *runner.Runs, id string) <-chan uisession.Event {
	t.Helper()
	events, detach, err := runs.Subscribe(id, uisession.Client{Kind: "web"}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(detach)
	return events
}

// Interrupt is the button a person presses when the model is doing the wrong
// thing, and the only one whose whole job is to end work already under way. A
// turn that ran on to its own end anyway would look identical in the table and
// be wrong in the only way that matters.
func TestInterruptEndsTheTurnThatIsRunning(t *testing.T) {
	held := newHeldModel(t)
	cwd := project(t)
	runs := runsAgainst(t, cwd, runner.Spec{Backend: held.model.ref("held-model")})
	v := create(t, runs, runner.RunRequest{})
	events := subscribeRun(t, runs, v.ID)

	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "go"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// The turn is unambiguously running only once it has reached the provider,
	// and the provider does not answer until this test lets it.
	held.waitForRequest(t)
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpInterrupt}); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	ev := waitFor(t, events, uisession.KindTurnEnd)
	// Released here rather than by a cleanup: the provider handler is holding an
	// httptest connection, and the server's own Close waits for it.
	held.release()
	if !ev.Interrupted {
		t.Fatal("the turn ended without being marked interrupted: it finished rather than stopped")
	}
	// The turn is over as far as the session is concerned, not just as far as
	// the timeline is: an operation that only a session between turns accepts
	// is refused by the model reference it names rather than by ErrBusy.
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSwitchModel, Ref: "nowhere/nothing"}); errors.Is(
		err, uisession.ErrBusy) {
		t.Fatal("the session still reports a turn in progress after the interrupt")
	}
}

// Switching model is metadata in two places that have to agree: the record a
// browser lists the run from, and the window the session measures compaction
// against. A switch that moved one and not the other would show the new model
// beside the old model's limits.
func TestSwitchingModelUpdatesTheRecordAndTheContextWindow(t *testing.T) {
	model := newFakeModel(t)
	cwd := project(t)
	// The provider declares a 4000-token window, so a window of 4000 after the
	// switch can only have come from it.
	models, warns := llm.NewRegistry(cwd, llm.LocalProvider(model.srv.URL, "switched-model", 4000, 0))
	if len(warns) != 0 {
		t.Fatalf("model registry warnings: %v", warns)
	}
	runs := runsAgainst(t, cwd, runner.Spec{
		Backend: model.ref("first-model"), Models: models, CtxSize: 4096,
	})
	v := create(t, runs, runner.RunRequest{})
	events := subscribeRun(t, runs, v.ID)

	if err := runs.Apply(v.ID, runner.RunOp{
		Op: runner.OpSwitchModel, Ref: "local/switched-model",
	}); err != nil {
		t.Fatalf("switch_model: %v", err)
	}
	if ev := waitFor(t, events, uisession.KindSessionMeta); ev.Ctx != 4000 {
		t.Errorf("context window after the switch = %d, want the new model's 4000", ev.Ctx)
	}
	after, err := runs.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Model != "local/switched-model" {
		t.Errorf("the record still names %q, want the model the run switched to", after.Model)
	}
}

// The last thing a page is told about a run that ended must not be that it is
// running.
//
// A published view is built outside the registry's lock, because building one
// asks the session five questions and the two locks are deliberately never held
// together. A close landing in that window would otherwise publish the closed
// record first and the stale one after - and `live` is the field a client reads
// before it decides whether to go on dialling.
func TestTheLastWordOnAClosedRunIsThatItIsClosed(t *testing.T) {
	cwd := project(t)
	var mu sync.Mutex
	var seen []runner.RunView
	runs, err := runner.NewRuns(runner.RunsConfig{
		Notify: func(v runner.RunView) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, v)
		},
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			_, reg := newEnvAndTools(t, cwd)
			return runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Backend: deadBackend(),
			}), runner.Opened{Model: "local/one"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	v, err := runs.Create(context.Background(), runner.RunRequest{})
	if err != nil {
		t.Fatal(err)
	}

	// The close happens while the announcement's view is built and not yet
	// sent. That is the ordering the check guards against, and it is otherwise
	// a window measured in microseconds.
	closed := make(chan struct{})
	restore := runner.PauseBetweenBuildAndPublish(func() {
		select {
		case <-closed:
			return
		default:
		}
		if err := runs.CloseRun(v.ID); err != nil {
			t.Error(err)
		}
		close(closed)
	})
	defer restore()

	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpStepMode, On: true}); err != nil {
		t.Fatal(err)
	}
	<-closed

	mu.Lock()
	defer mu.Unlock()
	var last runner.RunView
	for _, got := range seen {
		if got.ID == v.ID {
			last = got
		}
	}
	if last.Live || last.Status != runner.RunClosed {
		t.Fatalf("the last word on %s was %+v, want a record with no session", v.ID, last)
	}
}
