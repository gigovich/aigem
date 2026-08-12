package bot

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
)

type fakeTransport struct {
	in      chan Inbound
	mu      sync.Mutex
	replies []string
}

func (f *fakeTransport) Events() <-chan Inbound { return f.in }
func (f *fakeTransport) Reply(_ ThreadID, text string) error {
	f.mu.Lock()
	f.replies = append(f.replies, text)
	f.mu.Unlock()
	return nil
}
func (f *fakeTransport) Post(_, _ string) error { return nil }
func (f *fakeTransport) Close() error           { close(f.in); return nil }

type fakeRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *fakeRunner) Run(_ context.Context, input string, _ agent.Events) (string, error) {
	r.started <- struct{}{}
	<-r.release
	return "answer:" + input, nil
}

func TestRuntimeRepliesPerThread(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	runner := &fakeRunner{started: make(chan struct{}, 4), release: make(chan struct{})}
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("c1"), Text: "hi"}
	<-runner.started
	close(runner.release)
	ft.Close()
	<-done

	if len(ft.replies) != 1 || ft.replies[0] != "answer:hi" {
		t.Fatalf("replies = %v", ft.replies)
	}
}

type fakeThreadTransport struct {
	fakeTransport
	thread string
}

func (f *fakeThreadTransport) ThreadHistory(_ context.Context, _ ThreadID) string { return f.thread }

func TestRuntimeThreadUpdateFeedsFullThread(t *testing.T) {
	ft := &fakeThreadTransport{
		fakeTransport: fakeTransport{in: make(chan Inbound, 4)},
		thread:        "lisa: status?\namiran: ready",
	}
	runner := &fakeRunner{started: make(chan struct{}, 4), release: make(chan struct{})}
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "thread_update", Thread: ThreadID("r1")}
	<-runner.started
	close(runner.release)
	ft.Close()
	<-done

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.replies) != 1 {
		t.Fatalf("replies = %v", ft.replies)
	}
	// The runner echoes its input; the thread_update input must carry the fetched thread and the
	// owner-decides preamble, not the empty inbound text.
	if !strings.Contains(ft.replies[0], "amiran: ready") ||
		!strings.Contains(ft.replies[0], threadUpdatePreamble) {
		t.Fatalf("thread_update input did not include the full thread + preamble: %q", ft.replies[0])
	}
}

func TestRuntimeSingleFlightPerThread(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 8)}
	var live, maxLive int
	var mu sync.Mutex
	gate := make(chan struct{})
	mk := func(string) Runner {
		return runnerFunc(func() {
			mu.Lock()
			live++
			if live > maxLive {
				maxLive = live
			}
			mu.Unlock()
			<-gate
			mu.Lock()
			live--
			mu.Unlock()
		})
	}
	rt := NewRuntime(ft, mk, 8)
	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	th := ThreadID("r1")
	for i := 0; i < 3; i++ {
		ft.in <- Inbound{Kind: "mention", Thread: th, Text: "x"}
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	ft.Close()
	<-done

	if maxLive != 1 {
		t.Fatalf("same-thread runs overlapped: maxLive=%d", maxLive)
	}
}

type runnerFunc func()

func (f runnerFunc) Run(_ context.Context, _ string, _ agent.Events) (string, error) {
	f()
	return "ok", nil
}

func TestIsNoReply(t *testing.T) {
	silent := []string{"NO_REPLY", "no_reply", " NO_REPLY ", "`NO_REPLY`", "**NO_REPLY**", "NO_REPLY."}
	for _, s := range silent {
		if !isNoReply(s) {
			t.Errorf("expected silent: %q", s)
		}
	}
	loud := []string{"", "NO_REPLY, but one thing", "done", "staying quiet"}
	for _, s := range loud {
		if isNoReply(s) {
			t.Errorf("expected posted: %q", s)
		}
	}
}

type staticRunner struct{ answer string }

func (r staticRunner) Run(_ context.Context, _ string, _ agent.Events) (string, error) {
	return r.answer, nil
}

func TestRuntimeSuppressesNoReply(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	rt := NewRuntime(ft, func(string) Runner { return staticRunner{answer: "`NO_REPLY`"} }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("r1"), Text: "fyi"}
	ft.Close()
	<-done

	if len(ft.replies) != 0 {
		t.Fatalf("NO_REPLY answer must not be posted, got %v", ft.replies)
	}
}

// countingThreadTransport serves thread history and counts runner executions via the runner below.
type countingRunner struct {
	started chan struct{}
	release chan struct{}
	runs    atomic.Int32
}

func (r *countingRunner) Run(_ context.Context, _ string, _ agent.Events) (string, error) {
	r.runs.Add(1)
	r.started <- struct{}{}
	<-r.release
	return "ok", nil
}

func TestRuntimeCoalescesThreadUpdates(t *testing.T) {
	ft := &fakeThreadTransport{
		fakeTransport: fakeTransport{in: make(chan Inbound, 8)},
		thread:        "amiran: done",
	}
	runner := &countingRunner{started: make(chan struct{}, 8), release: make(chan struct{}, 8)}
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	up := Inbound{Kind: "thread_update", Thread: ThreadID("r1")}
	ft.in <- up
	<-runner.started // first update is running and holds the thread lock
	// A burst of updates lands while the turn is in flight: they must coalesce.
	for i := 0; i < 5; i++ {
		ft.in <- up
	}
	// Give the burst time to be routed (each would have started its own run before).
	time.Sleep(50 * time.Millisecond)
	runner.release <- struct{}{} // finish the first turn
	<-runner.started             // exactly one coalesced follow-up turn starts
	runner.release <- struct{}{}

	// Drain: no further runs may start.
	time.Sleep(50 * time.Millisecond)
	ft.Close()
	<-done

	if got := runner.runs.Load(); got != 2 {
		t.Fatalf("runs = %d, want 2 (initial + one coalesced)", got)
	}
}

type budgetRunner struct {
	stops int32 // how many turns fire the budget callback before succeeding
	runs  atomic.Int32
}

func (r *budgetRunner) Run(_ context.Context, input string, ev agent.Events) (string, error) {
	n := r.runs.Add(1)
	if n <= atomic.LoadInt32(&r.stops) && ev.OnBudgetExhausted != nil {
		ev.OnBudgetExhausted("model round budget reached (40)")
		return "wrap-up: did X, next Y", nil
	}
	return "answer:" + input, nil
}

func TestRuntimeAutoResumesAfterBudgetStop(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	runner := &budgetRunner{stops: 1}
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)
	rt.resumeDelay = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { rt.Serve(ctx); close(done) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("r1"), Text: "build it"}

	deadline := time.After(2 * time.Second)
	for {
		ft.mu.Lock()
		n := len(ft.replies)
		ft.mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("auto-resume turn never replied")
		case <-time.After(5 * time.Millisecond):
		}
	}
	ft.Close()
	<-done

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.replies) != 2 {
		t.Fatalf("replies = %v, want wrap-up + resumed answer", ft.replies)
	}
	if !strings.Contains(ft.replies[0], "wrap-up") || !strings.Contains(ft.replies[0], budgetResumeNote) {
		t.Fatalf("first reply must carry the wrap-up and the resume note: %q", ft.replies[0])
	}
	if !strings.Contains(ft.replies[1], "stopped at a work limit") {
		t.Fatalf("resume turn must receive the continuation prompt: %q", ft.replies[1])
	}
}

func TestRuntimeAutoResumeCap(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	runner := &budgetRunner{stops: 100} // every turn hits the budget
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)
	rt.resumeDelay = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { rt.Serve(ctx); close(done) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("r1"), Text: "go"}
	deadline := time.After(2 * time.Second)
	for runner.runs.Load() < int32(1+maxAutoResumes) {
		select {
		case <-deadline:
			t.Fatalf("resume chain stalled at %d runs", runner.runs.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond) // a run past the cap would start within this grace window
	ft.Close()
	<-done

	if got := runner.runs.Load(); got != int32(1+maxAutoResumes) {
		t.Fatalf("runs = %d, want %d (initial + capped resumes)", got, 1+maxAutoResumes)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	last := ft.replies[len(ft.replies)-1]
	if !strings.Contains(last, budgetStalledNote) {
		t.Fatalf("final reply must say auto-resumes are exhausted: %q", last)
	}
}

type errorRunner struct {
	err  error
	runs atomic.Int32
}

func (r *errorRunner) Run(_ context.Context, _ string, _ agent.Events) (string, error) {
	r.runs.Add(1)
	return "", r.err
}

func TestRuntimeSanitizesTransientErrorAndResumes(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	runner := &errorRunner{err: errors.New(`responses: status 429: {"error":{"type":"usage_limit_reached","message":"x"}}`)}
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)
	rt.resumeDelay = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { rt.Serve(ctx); close(done) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("r1"), Text: "hi"}

	deadline := time.After(2 * time.Second)
	for runner.runs.Load() < 2 { // the transient failure must schedule a resume turn
		select {
		case <-deadline:
			t.Fatal("transient failure did not schedule a resume")
		case <-time.After(5 * time.Millisecond):
		}
	}
	ft.Close()
	<-done

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.replies) == 0 {
		t.Fatal("expected a sanitized failure notice for a direct mention")
	}
	for _, reply := range ft.replies {
		if strings.Contains(reply, "usage_limit_reached") || strings.Contains(reply, "{") {
			t.Fatalf("raw provider payload leaked into chat: %q", reply)
		}
	}
	if !strings.Contains(ft.replies[0], "status 429") {
		t.Fatalf("notice should carry the short error class: %q", ft.replies[0])
	}
}

// imageTransport serves one image attachment plus a note.
type imageTransport struct {
	*fakeTransport
}

func (it *imageTransport) Attachments(_ context.Context, fileIDs []string) ([]llm.Image, string) {
	imgs := make([]llm.Image, len(fileIDs))
	for i := range imgs {
		imgs[i] = llm.Image{MediaType: "image/png", Data: "aGk="}
	}
	return imgs, "Attachments on this message:\n- shot.png (image/png, 3 B) - attached as an image"
}

// imageRunner records what RunWithImages received and echoes the input.
type imageRunner struct {
	images int
}

func (r *imageRunner) Run(_ context.Context, input string, _ agent.Events) (string, error) {
	return "text:" + input, nil
}

func (r *imageRunner) RunWithImages(_ context.Context, input string, images []llm.Image,
	_ agent.Events) (string, error) {
	r.images = len(images)
	return "multimodal:" + input, nil
}

func TestRuntimeDispatchesAttachmentsToImageRunner(t *testing.T) {
	ft := &imageTransport{fakeTransport: &fakeTransport{in: make(chan Inbound, 4)}}
	runner := &imageRunner{}
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("r1"),
		Text: "what is in the screenshot?", AttachmentIDs: []string{"f1"}}
	ft.Close()
	<-done

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.replies) != 1 || !strings.HasPrefix(ft.replies[0], "multimodal:") {
		t.Fatalf("attachment turn must go through RunWithImages: %v", ft.replies)
	}
	if runner.images != 1 {
		t.Fatalf("images passed = %d, want 1", runner.images)
	}
	if !strings.Contains(ft.replies[0], "shot.png") {
		t.Fatalf("attachment note must be appended to the input: %q", ft.replies[0])
	}
}

func TestErrSummary(t *testing.T) {
	in := errors.New(`responses: status 429: {"error":{"type":"usage_limit_reached"}}`)
	if got := errSummary(in); got != "responses: status 429" {
		t.Fatalf("errSummary = %q", got)
	}
	long := errors.New(strings.Repeat("x", 500))
	if got := errSummary(long); len([]rune(got)) > 170 {
		t.Fatalf("errSummary did not cap length: %d", len(got))
	}
}

// billingTransport journals, so its turns are the ones a model call can be
// billed to.
type billingTransport struct {
	fakeTransport
	mu    sync.Mutex
	spent map[string][]int
}

func (b *billingTransport) TurnEvents(thread ThreadID, _ string) (agent.Events, UsageSink,
	func(string, error)) {
	spend := func(u llm.Usage, _ string) {
		b.mu.Lock()
		b.spent[string(thread)] = append(b.spent[string(thread)], u.InputTokens)
		b.mu.Unlock()
	}
	return agent.Events{}, spend, func(string, error) {}
}

// billingRunner stands in for the model client: it reads the sink off the
// context exactly as the OnCallCtx callback does, on the goroutine that made the
// call. It holds the turn open until released, so both turns are in flight at
// once - which is the only arrangement in which a shared sink would smear.
type billingRunner struct {
	tokens  int
	started chan struct{}
	release chan struct{}
}

func (r *billingRunner) Run(ctx context.Context, input string, _ agent.Events) (string, error) {
	r.started <- struct{}{}
	<-r.release
	spend := UsageFrom(ctx)
	if spend == nil {
		return "", errors.New("the turn context carries no usage sink")
	}
	for range 3 {
		spend(llm.Usage{InputTokens: r.tokens}, "xai/grok-4.3")
	}
	return "answer:" + input, nil
}

// One client serves every thread a bot works on at once, so what a turn cost can
// only be known per call and only from the call's own context. Sampling a total
// around a turn would charge each of these threads for the other.
func TestConcurrentTurnsBillTheirOwnThread(t *testing.T) {
	bt := &billingTransport{
		fakeTransport: fakeTransport{in: make(chan Inbound, 4)},
		spent:         map[string][]int{},
	}
	started, release := make(chan struct{}, 2), make(chan struct{})
	tokens := map[string]int{"c1": 100, "c2": 7}
	rt := NewRuntime(bt, func(key string) Runner {
		return &billingRunner{tokens: tokens[key], started: started, release: release}
	}, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	bt.in <- Inbound{Kind: "mention", Thread: ThreadID("c1"), Text: "hi"}
	bt.in <- Inbound{Kind: "mention", Thread: ThreadID("c2"), Text: "hi"}
	<-started
	<-started
	close(release)
	bt.Close()
	<-done

	bt.mu.Lock()
	defer bt.mu.Unlock()
	for thread, want := range map[string][]int{"c1": {100, 100, 100}, "c2": {7, 7, 7}} {
		if !slices.Equal(bt.spent[thread], want) {
			t.Fatalf("thread %s was billed %v, want %v", thread, bt.spent[thread], want)
		}
	}
}

// A context outlives the turn that built it: a budget stop parks one for
// minutes and the resume runs on that same context. So a turn that has no sink
// has to say so, or its calls land on a row that was closed long ago.
func TestANilSinkClearsTheOneItInherited(t *testing.T) {
	ctx := WithUsage(t.Context(), func(llm.Usage, string) {})
	if UsageFrom(ctx) == nil {
		t.Fatal("the sink was not attached")
	}
	if UsageFrom(WithUsage(ctx, nil)) != nil {
		t.Fatal("a turn with no journal inherited the previous turn's sink")
	}
}

// A transport that does not journal has no turn to bill to, and a bot whose
// calls could not be attributed must still work.
func TestATurnWithoutAJournalStillRuns(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	rt := NewRuntime(ft, func(string) Runner { return sinkProbeRunner{} }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("c1"), Text: "hi"}
	ft.Close()
	<-done

	if len(ft.replies) != 1 || ft.replies[0] != "no sink" {
		t.Fatalf("replies = %v", ft.replies)
	}
}

type sinkProbeRunner struct{}

func (sinkProbeRunner) Run(ctx context.Context, _ string, _ agent.Events) (string, error) {
	if UsageFrom(ctx) != nil {
		return "sink", nil
	}
	return "no sink", nil
}

type eventRunner struct{}

func (eventRunner) Run(_ context.Context, input string, ev agent.Events) (string, error) {
	if ev.OnToolStart != nil {
		ev.OnToolStart("c1", "read_channel", nil)
	}
	if ev.OnToolEnd != nil {
		ev.OnToolEnd("c1", "read_channel", "ok", nil)
	}
	return "answer:" + input, nil
}

func TestRuntimeLogsSteps(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	ft := &fakeTransport{in: make(chan Inbound, 4)}
	rt := NewRuntime(ft, func(string) Runner { return eventRunner{} }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("c1"), Author: "u-x", Text: "hi"}
	ft.Close()
	<-done

	logs := buf.String()
	for _, want := range []string{"inbound", "tool start", "tool end", "reply"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %q in:\n%s", want, logs)
		}
	}
}

func TestRuntimeBusyWhileTurnRuns(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	runner := &fakeRunner{started: make(chan struct{}, 4), release: make(chan struct{})}
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)
	if rt.Busy() {
		t.Fatal("an idle runtime must not report busy")
	}

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("c1"), Text: "hi"}
	<-runner.started
	if !rt.Busy() {
		t.Fatal("a runtime with a turn in flight must report busy, or cron will double up on it")
	}
	close(runner.release)
	ft.Close()
	<-done

	// The count must return to zero, or the busy gate would silence cron forever.
	deadline := time.Now().Add(2 * time.Second)
	for rt.Busy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if rt.Busy() {
		t.Fatal("Busy should clear once the turn finished")
	}
}

func TestRuntimeOnAddressedFiresForMentionAndDMOnly(t *testing.T) {
	ft := &fakeThreadTransport{
		fakeTransport: fakeTransport{in: make(chan Inbound, 8)},
		thread:        "kate: готова взять DOAML-7",
	}
	var calls atomic.Int64
	rt := NewRuntime(ft, func(string) Runner { return &echoRunner{} }, 4)
	rt.SetOnAddressed(func() { calls.Add(1) })

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("c1"), Text: "статус?"}
	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("r"), Text: "@amiran"}
	ft.in <- Inbound{Kind: "thread_update", Thread: ThreadID("r3")}
	ft.Close()
	<-done

	if got := calls.Load(); got != 2 {
		t.Fatalf("onAddressed called %d times, want 2 (the dm and the mention, not the thread update)", got)
	}
}

type echoRunner struct{}

func (echoRunner) Run(_ context.Context, input string, _ agent.Events) (string, error) {
	return "ok:" + input, nil
}

// The commit's claim is that timer-driven runs are as visible as chat turns; without a tag an
// operator cannot tell a scheduled run from silence.
func TestCronEventsTagRunsWithTheJobID(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	ev := CronEvents(slog.Default(), "work-heartbeat")
	ev.OnToolStart("c1", "read_file", nil)
	ev.OnToolEnd("c1", "read_file", "", nil)
	ev.OnToolEnd("c2", "bash", "", errRefused{})
	out := buf.String()
	if !strings.Contains(out, "cron:work-heartbeat") {
		t.Fatalf("cron events must carry the job id, got %q", out)
	}
	if !strings.Contains(out, "tool=read_file") || !strings.Contains(out, "level=ERROR") {
		t.Fatalf("cron events must log tool steps and failures, got %q", out)
	}
}

// A message addressed inside a thread must carry that thread. Without it a bare "@bot" in a thread
// the bot itself started reads as a greeting out of nowhere - which is exactly what happened live:
// the manager answered "Hi, how can I help?" to a reply to its own escalation.
func TestRuntimeMentionInThreadCarriesTheThread(t *testing.T) {
	ft := &fakeThreadTransport{
		fakeTransport: fakeTransport{in: make(chan Inbound, 4)},
		thread:        "lisa: нужно решение по #32\ngigovich: препятствий для #32 не вижу",
	}
	seen := make(chan string, 4)
	rt := NewRuntime(ft, func(string) Runner { return &captureRunner{inputs: seen} }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()
	ft.in <- Inbound{Kind: "mention",
		Thread: ThreadID("root1"), Text: "@lisa"}
	got := <-seen
	ft.Close()
	<-done

	if !strings.Contains(got, "препятствий для #32 не вижу") {
		t.Fatalf("the mention's input lost the thread it arrived in: %q", got)
	}
	if !strings.Contains(got, "@lisa") {
		t.Fatalf("the mention's own text is missing: %q", got)
	}
}

// A mention that opens its own thread has nothing to prepend; repeating the single message would
// just duplicate it.
func TestRuntimeMentionOpeningAThreadIsNotDuplicated(t *testing.T) {
	ft := &fakeThreadTransport{
		fakeTransport: fakeTransport{in: make(chan Inbound, 4)},
		thread:        "gigovich: @lisa посмотри #32",
	}
	seen := make(chan string, 4)
	rt := NewRuntime(ft, func(string) Runner { return &captureRunner{inputs: seen} }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()
	ft.in <- Inbound{Kind: "mention",
		Thread: ThreadID("root1"), Text: "@lisa посмотри #32"}
	got := <-seen
	ft.Close()
	<-done

	if strings.Contains(got, "Thread:") {
		t.Fatalf("a one-message thread should not be prefixed: %q", got)
	}
}

type captureRunner struct{ inputs chan string }

func (c *captureRunner) Run(_ context.Context, input string, _ agent.Events) (string, error) {
	c.inputs <- input
	return "NO_REPLY", nil
}
