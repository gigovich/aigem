package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/uisession"
	"github.com/gigovich/aigem/internal/web"
)

// The adapter is the only thing standing between the web package and the model
// registry, so what it puts in Meta has to be what the wire expects: the
// version this binary reports, and a model reference the registry can resolve
// rather than a label meant for a human.
func TestWebBackendMetaReportsTheVersionAndAResolvableModel(t *testing.T) {
	b := newWebBackend("1.2.3-test", nil, nil)
	meta, err := b.Meta(context.Background())
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Version != "1.2.3-test" {
		t.Errorf("Version = %q, want the version the backend was built with", meta.Version)
	}
	// Empty is a legitimate answer: the test environment has no provider signed
	// in, and a page has to be able to show that state.
	if meta.DefaultModel == "" {
		return
	}
	if _, _, err := b.models.Resolve(meta.DefaultModel); err != nil {
		t.Errorf("DefaultModel = %q, which the registry cannot resolve: %v", meta.DefaultModel, err)
	}
}

// testRuns builds a run table whose sessions are real ones against a model that
// is not there. Everything the adapter does happens before a turn, so nothing
// here ever reaches a provider.
func testRuns(t *testing.T) (*runner.Runs, *lastSession) {
	t.Helper()
	env, _, err := runner.Load(context.Background(), runner.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(env.Close)

	built := &lastSession{}
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			reg, err := env.NewTools()
			if err != nil {
				return nil, runner.Opened{}, err
			}
			s := runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Title: req.Title,
				// A closed port, so a turn that reached the model would fail
				// fast rather than hang on a real one.
				Backend: llm.NewRef(llm.New("http://127.0.0.1:9", "t")),
			})
			built.set(s.Local)
			return s, runner.Opened{Model: "test/model", Root: env.Cwd, Title: req.Title}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRuns: %v", err)
	}
	t.Cleanup(runs.Close)
	return runs, built
}

func openTestRun(t *testing.T, b *webBackend) web.Run {
	t.Helper()
	run, err := b.OpenRun(context.Background(), web.NewRun{Title: "a run"})
	if err != nil {
		t.Fatalf("OpenRun: %v", err)
	}
	return run
}

// The wire's record is the registry's, field for field. A translation that
// dropped one would show up as a screen that renders nothing and says nothing.
func TestTheAdapterPutsTheWholeRunOnTheWire(t *testing.T) {
	runs, _ := testRuns(t)
	b := newWebBackend("1.2.3-test", nil, runs)
	run := openTestRun(t, b)

	if run.ID == "" || run.Title != "a run" || run.Mode != "interactive" {
		t.Fatalf("run = %+v, want the record it was opened with", run)
	}
	if run.Status != "open" || !run.Live || !run.Step {
		t.Errorf("run = %+v, want a live, open run that asks about tool calls", run)
	}
	if run.Model != "test/model" || run.Root == "" || run.Created.IsZero() {
		t.Errorf("run = %+v, want the model, root and time the registry recorded", run)
	}

	listed, err := b.Runs(context.Background())
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != run.ID {
		t.Errorf("Runs = %+v, want the one run", listed)
	}
}

// The status code a page gets is decided here. Getting it wrong turns "reload
// this run" into "retry forever", or a missing run into a 500 the operator
// cannot tell from a broken daemon.
func TestTheAdapterClassifiesWhatTheRegistryReports(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   error
		want error
	}{
		{"nothing", nil, nil},
		{"no run", runner.ErrNoRun, web.ErrNoRun},
		{"closed run", runner.ErrRunClosed, web.ErrRunClosed},
		{"history gone", uisession.ErrTruncated, web.ErrHistoryGone},
		{"shutting down", runner.ErrRunsClosed, runner.ErrRunsClosed},
	} {
		if got := webRunError(tc.in); !errors.Is(got, tc.want) {
			t.Errorf("%s: webRunError(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}

	// Everything else is a sentence for the person at the browser. This daemon
	// serves one signed-in operator on their own machine, and "the daemon could
	// not carry that out" for a provider that is not signed in is a button that
	// stops working with no way to find out why.
	var refusal *web.Refusal
	err := webRunError(errors.New("model openai/gpt-5.6-sol requires authentication"))
	if !errors.As(err, &refusal) {
		t.Fatalf("an unclassified error became %v, want a Refusal", err)
	}
	if !strings.Contains(refusal.Reason, "requires authentication") {
		t.Errorf("reason = %q, want what the registry said", refusal.Reason)
	}
}

// An unsupported mode is the client's mistake and its reason is worth reading.
func TestAModeTheRegistryRefusesReachesTheClient(t *testing.T) {
	runs, _ := testRuns(t)
	b := newWebBackend("1.2.3-test", nil, runs)
	_, err := b.OpenRun(context.Background(), web.NewRun{Mode: "autonomous"})
	var refusal *web.Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "autonomous") {
		t.Fatalf("OpenRun = %v, want a refusal naming the mode", err)
	}
}

// The timeline crosses the seam as bytes, and they have to be the event's own
// encoding: the web package writes them out untouched.
func TestEventsCrossTheSeamAsTheirOwnEncoding(t *testing.T) {
	runs, _ := testRuns(t)
	b := newWebBackend("1.2.3-test", nil, runs)
	run := openTestRun(t, b)
	// Attaching is an event: the other clients are shown who is there.
	stream, err := b.WatchRun(context.Background(), run.ID, web.RunClient{Kind: "web"}, 0)
	if err != nil {
		t.Fatalf("WatchRun: %v", err)
	}
	defer stream.Close()

	select {
	case ev, ok := <-stream.Events():
		if !ok {
			t.Fatal("the stream closed before it said anything")
		}
		var decoded uisession.Event
		if err := json.Unmarshal(ev.Data, &decoded); err != nil {
			t.Fatalf("the frame is not an event: %v", err)
		}
		if decoded.Seq != ev.Seq {
			t.Errorf("frame seq = %d but the payload says %d", ev.Seq, decoded.Seq)
		}
		if decoded.Kind != uisession.KindPresence {
			t.Errorf("first event = %q, want presence", decoded.Kind)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing arrived on the stream")
	}

	events, err := b.RunEvents(context.Background(), run.ID, 0, 0)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("the timeline is empty after a client attached")
	}
}

// Closing a stream is what a disconnecting client does, and a handler defers it
// as well as closing early. Detaching twice would take the wrong subscriber off
// the session the second time.
func TestARunStreamIsSafeToCloseTwice(t *testing.T) {
	runs, _ := testRuns(t)
	b := newWebBackend("1.2.3-test", nil, runs)
	run := openTestRun(t, b)
	stream, err := b.WatchRun(context.Background(), run.ID, web.RunClient{Kind: "web"}, 0)
	if err != nil {
		t.Fatalf("WatchRun: %v", err)
	}
	stream.Close()
	stream.Close()
	// And the channel is closed, so a pump reading it ends rather than parking.
	for range stream.Events() {
	}
}

// A page redraws this list on every change, and a map's order is not one.
func TestArtifactsAreSortedByPath(t *testing.T) {
	runs, built := testRuns(t)
	b := newWebBackend("1.2.3-test", nil, runs)
	run := openTestRun(t, b)

	sess := built.get(t)
	// Enough of them, in reverse, that Go's map ordering cannot hand this test
	// a pass: three paths come out sorted about one run in six.
	var want []string
	for i := 9; i >= 0; i-- {
		p := fmt.Sprintf("/w/%d.go", i)
		sess.RecordFileChange(p, "before", "after", false)
		want = append(want, p)
	}
	sort.Strings(want)

	arts, err := b.RunArtifacts(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("RunArtifacts: %v", err)
	}
	var paths []string
	for _, a := range arts {
		paths = append(paths, a.Path)
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	if arts[0].Old != "before" || arts[0].New != "after" {
		t.Errorf("artifact = %+v, want both sides of the change", arts[0])
	}
	if arts[0].OldBytes != len("before") || arts[0].NewBytes != len("after") {
		t.Errorf("artifact = %+v, want the sizes of both sides", arts[0])
	}
}

// A run that changed a very large file holds both versions of it. Serialising
// them would copy an unbounded amount of memory per request, and the list of
// what changed is what the page actually needs.
func TestAVeryLargeChangeIsListedWithoutItsContent(t *testing.T) {
	runs, built := testRuns(t)
	b := newWebBackend("1.2.3-test", nil, runs)
	run := openTestRun(t, b)

	sess := built.get(t)
	huge := strings.Repeat("x", maxArtifactSide+1)
	sess.RecordFileChange("/w/generated.go", "", huge, true)
	sess.RecordFileChange("/w/small.go", "before", "after", false)

	arts, err := b.RunArtifacts(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("RunArtifacts: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("got %d artifacts, want both files listed", len(arts))
	}
	big, small := arts[0], arts[1]
	if big.Path != "/w/generated.go" || small.Path != "/w/small.go" {
		t.Fatalf("artifacts = %+v, want them sorted by path", arts)
	}
	if !big.Truncated || big.New != "" {
		t.Errorf("the large change = %+v, want it listed without its content", big)
	}
	if big.NewBytes != len(huge) {
		t.Errorf("NewBytes = %d, want the real size %d", big.NewBytes, len(huge))
	}
	// And the file next to it is unaffected: the budget rations content, it
	// does not switch it off.
	if small.Truncated || small.New != "after" {
		t.Errorf("the small change = %+v, want its content", small)
	}
}

// Every operation carries fields that only differ by name, and this is the
// translation between two structs full of them. A mapping that put the model
// reference where the approval id goes would compile and run.
func TestTheAdapterTranslatesEachOperation(t *testing.T) {
	runs, built := testRuns(t)
	b := newWebBackend("1.2.3-test", nil, runs)
	run := openTestRun(t, b)
	ctx := context.Background()

	if err := b.ApplyRunOp(ctx, run.ID, web.RunOp{Op: "submit", Text: "the message"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	evs, err := runs.Events(run.ID, 0, 0)
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

	var gotArgs string
	built.get(t).Handle("rename", func(args string) error {
		gotArgs = args
		return nil
	})
	if err := b.ApplyRunOp(ctx, run.ID, web.RunOp{
		Op: "command", Name: "rename", Args: "the widget factory",
	}); err != nil {
		t.Fatalf("command: %v", err)
	}
	if gotArgs != "the widget factory" {
		t.Errorf("the handler was given %q, want the argument line", gotArgs)
	}

	// step_mode is the one op whose meaning is inverted on the way through.
	if err := b.ApplyRunOp(ctx, run.ID, web.RunOp{Op: "step_mode", On: false}); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Run(ctx, run.ID); got.Step {
		t.Error("step mode reads as on after it was turned off")
	}

	// resolve carries the decision and who made it, and an approval nobody is
	// waiting on is the ordinary refusal rather than something else.
	err = b.ApplyRunOp(ctx, run.ID, web.RunOp{
		Op: "resolve", ID: "a1", Decision: "once", Label: "web",
	})
	var refusal *web.Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "already decided") {
		t.Errorf("resolve = %v, want a refusal saying the approval was decided", err)
	}

	// switch_model resolves the ref, and a failed switch leaves the record.
	before, _ := b.Run(ctx, run.ID)
	if err := b.ApplyRunOp(ctx, run.ID, web.RunOp{
		Op: "switch_model", Ref: "nowhere/nothing",
	}); err == nil {
		t.Error("switching to a model that does not resolve succeeded")
	}
	if after, _ := b.Run(ctx, run.ID); after.Model != before.Model {
		t.Errorf("model = %q after a failed switch, want %q", after.Model, before.Model)
	}
}

// A session closed underneath the table answers from further in than the
// registry does, and a client keys on the status code: "session closed" as a
// 400 refusal is not something a page can act on.
func TestASessionClosedUnderTheTableIsStillAClosedRun(t *testing.T) {
	runs, built := testRuns(t)
	b := newWebBackend("1.2.3-test", nil, runs)
	run := openTestRun(t, b)

	built.get(t).Close()
	_, err := b.WatchRun(context.Background(), run.ID, web.RunClient{Kind: "web"}, 0)
	if !errors.Is(err, web.ErrRunClosed) {
		t.Fatalf("WatchRun on a closed session = %v, want ErrRunClosed", err)
	}
}

// lastSession remembers the session the test's own Open built. The registry
// hands none out - it is the point of the type that a caller works through the
// id - so this is how a test reaches the conversation to make something happen
// in it.
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
