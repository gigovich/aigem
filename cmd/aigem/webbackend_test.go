package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

	// An attached image travels with the message, in the shape the model APIs
	// take. It is a field the wire has and nothing else would notice missing.
	if err := b.ApplyRunOp(ctx, run.ID, web.RunOp{
		Op: "submit", Text: "what is this",
		Images: []web.Image{{MediaType: "image/png", Data: "aGVsbG8="}},
	}); err != nil {
		t.Fatalf("submit with an image: %v", err)
	}
	evs, err = runs.Events(run.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var withImage bool
	for _, ev := range evs {
		if ev.Kind == uisession.KindUserMessage && ev.Text == "what is this" && ev.Images == 1 {
			withImage = true
		}
	}
	if !withImage {
		t.Errorf("the message did not carry its image: %+v", evs)
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

// A run that touched a hundred files is a real run; a hundred files' worth of
// content in one document is not a page anyone can render. Every file is still
// listed, with its real size, whether or not its content came along.
func TestTheArtifactBudgetIsSpentAcrossTheWholeResponse(t *testing.T) {
	runs, built := testRuns(t)
	b := newWebBackend("1.2.3-test", nil, runs)
	run := openTestRun(t, b)

	// Each is well inside the per-change cap, and together they are several
	// times the response budget.
	const each = maxArtifactSide / 2
	files := (maxArtifactBody / each) + 8
	body := strings.Repeat("y", each)
	sess := built.get(t)
	for i := range files {
		sess.RecordFileChange(fmt.Sprintf("/w/%03d.go", i), "", body, true)
	}

	arts, err := b.RunArtifacts(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("RunArtifacts: %v", err)
	}
	if len(arts) != files {
		t.Fatalf("got %d artifacts, want all %d listed", len(arts), files)
	}
	total, truncated := 0, 0
	for _, a := range arts {
		total += len(a.Old) + len(a.New)
		if a.Truncated {
			truncated++
			if a.Old != "" || a.New != "" {
				t.Errorf("%s is marked truncated and still carries content", a.Path)
			}
		}
		if a.NewBytes != each {
			t.Errorf("%s reports %d bytes, want the real %d", a.Path, a.NewBytes, each)
		}
	}
	if total > maxArtifactBody {
		t.Errorf("the response carries %d bytes of content, and the budget is %d",
			total, maxArtifactBody)
	}
	if truncated == 0 {
		t.Error("nothing was truncated; the budget was never reached")
	}
}

// The label is who answered an approval, and it is what the other clients are
// shown instead of a failure. A translation that dropped it would make every
// answer anonymous.
func TestTheAdapterCarriesWhoAnsweredAnApproval(t *testing.T) {
	cwd := t.TempDir()
	env, _, err := runner.Load(context.Background(), runner.Options{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.Close)

	model := newAskingModel(t)
	var built *uisession.Local
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(_ context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			reg, err := env.NewTools()
			if err != nil {
				return nil, runner.Opened{}, err
			}
			s := runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Backend: llm.NewRef(llm.New(model.URL, "m")),
			})
			built = s.Local
			return s, runner.Opened{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	b := newWebBackend("1.2.3-test", nil, runs)
	run := openTestRun(t, b)
	stream, err := b.WatchRun(context.Background(), run.ID, web.RunClient{Kind: "web"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := b.ApplyRunOp(context.Background(), run.ID, web.RunOp{
		Op: "submit", Text: "write it",
	}); err != nil {
		t.Fatal(err)
	}

	// Wait for the approval the tool call parks on, then answer it as a named
	// client.
	var id string
	deadline := time.Now().Add(30 * time.Second)
	for id == "" && time.Now().Before(deadline) {
		id, _ = built.Pending()
		time.Sleep(5 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("the turn never parked on an approval")
	}
	if err := b.ApplyRunOp(context.Background(), run.ID, web.RunOp{
		Op: "resolve", ID: id, Decision: "deny", Label: "the kitchen tablet",
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	for ev := range stream.Events() {
		var decoded uisession.Event
		if err := json.Unmarshal(ev.Data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Kind != uisession.KindApprovalResolved {
			continue
		}
		if decoded.By != "the kitchen tablet" {
			t.Errorf("the answer is attributed to %q, want the client that gave it", decoded.By)
		}
		if decoded.Decision != uisession.DecisionDeny {
			t.Errorf("decision = %q, want the one that was sent", decoded.Decision)
		}
		return
	}
	t.Fatal("no approval_resolved event arrived")
}

// newAskingModel answers with a tool call that has to be approved, so a test can
// reach the approval queue.
func newAskingModel(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1",`+
			`"type":"function","function":{"name":"write_file",`+
			`"arguments":"{\"path\":\"notes.md\",\"content\":\"hi\"}"}}]},`+
			`"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Who is watching is carried through the adapter into the session, and comes
// back to every client on the presence event. Swapping the two fields, or
// dropping them, makes every tab anonymous - and presence is what tells
// "thinking" apart from "waiting for somebody who walked away".
func TestTheAdapterCarriesWhoIsWatching(t *testing.T) {
	runs, _ := testRuns(t)
	b := newWebBackend("1.2.3-test", nil, runs)
	run := openTestRun(t, b)

	stream, err := b.WatchRun(context.Background(), run.ID,
		web.RunClient{Kind: "phone", Label: "the kitchen one"}, 0)
	if err != nil {
		t.Fatalf("WatchRun: %v", err)
	}
	defer stream.Close()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				t.Fatal("the stream ended before presence arrived")
			}
			var decoded uisession.Event
			if err := json.Unmarshal(ev.Data, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Kind != uisession.KindPresence {
				continue
			}
			if len(decoded.Clients) != 1 {
				t.Fatalf("presence lists %d clients, want the one that attached", len(decoded.Clients))
			}
			got := decoded.Clients[0]
			if got.Kind != "phone" || got.Label != "the kitchen one" {
				t.Fatalf("presence shows %+v, want the kind and label that were given", got)
			}
			return
		case <-deadline:
			t.Fatal("presence never arrived")
		}
	}
}

// A resuming client says where it got to, and the adapter has to hand that on:
// a second attachment that replayed from zero would redraw the whole
// conversation over one the page already has.
func TestTheAdapterResumesFromTheCursorItWasGiven(t *testing.T) {
	runs, _ := testRuns(t)
	b := newWebBackend("1.2.3-test", nil, runs)
	run := openTestRun(t, b)
	ctx := context.Background()

	// One attachment, detached again, so the run has a timeline to resume into.
	first, err := b.WatchRun(ctx, run.ID, web.RunClient{Kind: "web"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var last uint64
	for ev := range first.Events() {
		last = ev.Seq
		if last >= 1 {
			break
		}
	}
	first.Close()
	if last == 0 {
		t.Fatal("nothing was emitted to resume from")
	}

	resumed, err := b.WatchRun(ctx, run.ID, web.RunClient{Kind: "web"}, last)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	select {
	case ev, ok := <-resumed.Events():
		if !ok {
			t.Fatal("the resumed stream closed at once")
		}
		if ev.Seq <= last {
			t.Errorf("resuming after %d replayed event %d", last, ev.Seq)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing arrived on the resumed stream")
	}
}
