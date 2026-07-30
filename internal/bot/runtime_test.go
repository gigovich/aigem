package bot

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
func (f *fakeTransport) Reply(_ ThreadRef, text string) error {
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

	ft.in <- Inbound{Kind: "dm", Channel: "c1", Thread: ThreadRef{ChannelID: "c1"}, Text: "hi"}
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

func (f *fakeThreadTransport) ThreadHistory(_ context.Context, _, _ string) string { return f.thread }

func TestRuntimeThreadUpdateFeedsFullThread(t *testing.T) {
	ft := &fakeThreadTransport{
		fakeTransport: fakeTransport{in: make(chan Inbound, 4)},
		thread:        "lisa: status?\namiran: ready",
	}
	runner := &fakeRunner{started: make(chan struct{}, 4), release: make(chan struct{})}
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "thread_update", Channel: "c1", Thread: ThreadRef{ChannelID: "c1", RootID: "r1"}}
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

	th := ThreadRef{ChannelID: "c1", RootID: "r1"}
	for i := 0; i < 3; i++ {
		ft.in <- Inbound{Kind: "mention", Channel: "c1", Thread: th, Text: "x"}
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

type historyTransport struct {
	*fakeTransport
	block string
}

func (h *historyTransport) History(_ context.Context, _ string) string { return h.block }

type typingTransport struct {
	*fakeTransport
	count int32
}

func (tt *typingTransport) Typing(_ ThreadRef) error {
	atomic.AddInt32(&tt.count, 1)
	return nil
}

func TestRuntimeSignalsTyping(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	tt := &typingTransport{fakeTransport: ft}
	runner := &fakeRunner{started: make(chan struct{}, 4), release: make(chan struct{})}
	rt := NewRuntime(tt, func(string) Runner { return runner }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "dm", Channel: "c1", Thread: ThreadRef{ChannelID: "c1"}, Text: "hi"}
	<-runner.started
	close(runner.release)
	ft.Close()
	<-done

	if atomic.LoadInt32(&tt.count) < 1 {
		t.Fatal("expected at least one typing signal during the run")
	}
}

func TestRuntimePrefixesHistory(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	ht := &historyTransport{fakeTransport: ft, block: "alice: earlier note"}
	runner := &fakeRunner{started: make(chan struct{}, 4), release: make(chan struct{})}
	rt := NewRuntime(ht, func(string) Runner { return runner }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "mention", Channel: "c1", Thread: ThreadRef{ChannelID: "c1", RootID: "p1"}, Text: "summarize"}
	<-runner.started
	close(runner.release)
	ft.Close()
	<-done

	if len(ft.replies) != 1 ||
		!strings.Contains(ft.replies[0], "alice: earlier note") ||
		!strings.Contains(ft.replies[0], "summarize") {
		t.Fatalf("history not prefixed: %v", ft.replies)
	}
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

	ft.in <- Inbound{Kind: "mention", Channel: "c1", Thread: ThreadRef{ChannelID: "c1", RootID: "r1"}, Text: "fyi"}
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

	up := Inbound{Kind: "thread_update", Channel: "c1", Thread: ThreadRef{ChannelID: "c1", RootID: "r1"}}
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

	ft.in <- Inbound{Kind: "mention", Channel: "c1", Thread: ThreadRef{ChannelID: "c1", RootID: "r1"}, Text: "build it"}

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

	ft.in <- Inbound{Kind: "mention", Channel: "c1", Thread: ThreadRef{ChannelID: "c1", RootID: "r1"}, Text: "go"}
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

	ft.in <- Inbound{Kind: "mention", Channel: "c1", Thread: ThreadRef{ChannelID: "c1", RootID: "r1"}, Text: "hi"}

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

	ft.in <- Inbound{Kind: "mention", Channel: "c1", Thread: ThreadRef{ChannelID: "c1", RootID: "r1"},
		Text: "what is in the screenshot?", FileIDs: []string{"f1"}}
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

type eventRunner struct{}

func (eventRunner) Run(_ context.Context, input string, ev agent.Events) (string, error) {
	if ev.OnToolStart != nil {
		ev.OnToolStart("read_channel", nil)
	}
	if ev.OnToolEnd != nil {
		ev.OnToolEnd("read_channel", "ok", nil)
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

	ft.in <- Inbound{Kind: "dm", Channel: "c1", Thread: ThreadRef{ChannelID: "c1"}, Author: "u-x", Text: "hi"}
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

	ft.in <- Inbound{Kind: "dm", Channel: "c1", Thread: ThreadRef{ChannelID: "c1"}, Text: "hi"}
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

	ft.in <- Inbound{Kind: "dm", Channel: "c1", Thread: ThreadRef{ChannelID: "c1"}, Text: "статус?"}
	ft.in <- Inbound{Kind: "mention", Channel: "c2", Thread: ThreadRef{ChannelID: "c2", RootID: "r"}, Text: "@amiran"}
	ft.in <- Inbound{Kind: "thread_update", Channel: "c3", Thread: ThreadRef{ChannelID: "c3", RootID: "r3"}}
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

	ev := CronEvents("work-heartbeat")
	ev.OnToolStart("read_file", nil)
	ev.OnToolEnd("read_file", "", nil)
	ev.OnToolEnd("bash", "", errRefused{})
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
	ft.in <- Inbound{Kind: "mention", Channel: "c1",
		Thread: ThreadRef{ChannelID: "c1", RootID: "root1"}, Text: "@lisa"}
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
	ft.in <- Inbound{Kind: "mention", Channel: "c1",
		Thread: ThreadRef{ChannelID: "c1", RootID: "root1"}, Text: "@lisa посмотри #32"}
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
