package bot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/agent"
)

// injectingRunner stands in for a long implementation turn: it blocks until a
// message is delivered into it, the way *agent.Agent does at a round boundary.
type injectingRunner struct {
	started chan string

	mu       sync.Mutex
	running  bool
	injected []string
	got      chan string
}

func (r *injectingRunner) Run(ctx context.Context, input string, _ agent.Events) (string, error) {
	r.mu.Lock()
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()
	r.started <- input
	select {
	case text := <-r.got:
		return "stopped: " + text, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (r *injectingRunner) Inject(text string) bool {
	r.mu.Lock()
	running := r.running
	if running {
		r.injected = append(r.injected, text)
	}
	r.mu.Unlock()
	if !running {
		return false
	}
	r.got <- text
	return true
}

func newInjectingRunner() *injectingRunner {
	return &injectingRunner{started: make(chan string, 4), got: make(chan string, 4)}
}

func TestAddressedMessageIsDeliveredIntoTheRunningTurn(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	runner := newInjectingRunner()
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rt.Serve(ctx)

	thread := ThreadID("t1")
	ft.in <- Inbound{Kind: "mention", Thread: thread, Author: "u-demetre", Text: "implement #5"}
	if got := <-runner.started; !strings.Contains(got, "implement #5") {
		t.Fatalf("first turn input = %q", got)
	}

	// Written in a language and phrasing no keyword list would carry.
	ft.in <- Inbound{Kind: "mention", Thread: thread, Author: "u-gigovich",
		Text: "@amiran погоди, AIGEM больше не трогаем, пока я не скажу"}

	waitForReplies(t, ft, 1)
	ft.mu.Lock()
	replies := append([]string(nil), ft.replies...)
	ft.mu.Unlock()
	// One reply, from the turn that was already running - the message did not
	// wait for a turn of its own.
	if len(replies) != 1 || !strings.Contains(replies[0], "больше не трогаем") {
		t.Fatalf("replies = %v", replies)
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.injected) != 1 {
		t.Fatalf("injected = %v", runner.injected)
	}
	delivered := runner.injected[0]
	for _, want := range []string{"погоди", "while you are working", "If it tells you to stop"} {
		if !strings.Contains(delivered, want) {
			t.Fatalf("delivery is missing %q:\n%s", want, delivered)
		}
	}
}

// With no turn running there is nothing to inject into, so the message must take
// the ordinary path and get its own turn.
func TestMessageWithNoRunningTurnRunsNormally(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	runner := &fakeRunner{started: make(chan struct{}, 4), release: make(chan struct{})}
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)

	done := make(chan struct{})
	go func() { rt.Serve(context.Background()); close(done) }()

	ft.in <- Inbound{Kind: "mention", Thread: ThreadID("c1"), Text: "stop that"}
	<-runner.started
	close(runner.release)
	ft.Close()
	<-done

	if len(ft.replies) != 1 || ft.replies[0] != "answer:stop that" {
		t.Fatalf("replies = %v", ft.replies)
	}
}

// A thread update is coalesced, not delivered: it carries no text of its own, so
// injecting it would put an empty nudge in front of the model.
func TestThreadUpdateIsNotDelivered(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	runner := newInjectingRunner()
	rt := NewRuntime(ft, func(string) Runner { return runner }, 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rt.Serve(ctx)

	thread := ThreadID("t1")
	ft.in <- Inbound{Kind: "mention", Thread: thread, Text: "implement #5"}
	<-runner.started
	ft.in <- Inbound{Kind: "thread_update", Thread: thread}

	time.Sleep(200 * time.Millisecond)
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.injected) != 0 {
		t.Fatalf("a thread update was delivered into the turn: %v", runner.injected)
	}
}

func TestMidTurnDeliveryNamesTheAuthor(t *testing.T) {
	got := midTurnDelivery("gigovich", "hold off on #5")
	if !strings.Contains(got, "from gigovich") || !strings.Contains(got, "hold off on #5") {
		t.Fatalf("delivery = %q", got)
	}
	// An unresolved author must not leave a dangling "from" in the header line.
	header, _, _ := strings.Cut(midTurnDelivery("", "x"), "\n")
	if strings.Contains(header, "from") {
		t.Fatalf("unnamed delivery should not say 'from': %q", header)
	}
}

func waitForReplies(t *testing.T, ft *fakeTransport, n int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		ft.mu.Lock()
		got := len(ft.replies)
		ft.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected %d replies", n)
}
