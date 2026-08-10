package uisession

import (
	"strings"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/tools"
)

// remotePair starts a daemon-shaped server around a real local session and
// attaches to it, so what is under test is the protocol rather than a mock of
// it. The server lives in the test because internal/web imports this package.
func remotePair(t *testing.T) (*Local, *Remote) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	local := New(Config{
		Tools: reg,
		NewAgent: func(confirm agent.ConfirmFunc) *agent.Agent {
			return agent.New(&scriptedClient{tool: "bash"}, reg, 0.3, confirm, "")
		},
		ModelRef: func() string { return "test/model" },
		Ring:     256,
	})
	t.Cleanup(local.Close)

	srv := newTestDaemon(t, local)
	r, err := Dial(srv.base, srv.id, srv.token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return local, r
}

// An attached client sees the conversation as it happens and can answer the
// question blocking it, which is the whole of "the terminal is a client".
func TestRemoteFollowsAndAnswers(t *testing.T) {
	local, r := remotePair(t)

	ch, stop, err := r.Subscribe(Client{ID: "tui", Kind: "tui"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if err := r.Submit("hello", nil); err != nil {
		t.Fatal(err)
	}
	if ev := waitFor(t, ch, KindUserMessage); ev.Text != "hello" {
		t.Fatalf("user message = %q", ev.Text)
	}
	req := waitFor(t, ch, KindApprovalRequest)
	if req.Approval == nil || req.Approval.Tool != "bash" {
		t.Fatalf("approval = %+v", req.Approval)
	}
	if err := r.Resolve(req.ID, DecisionOnce, "tui"); err != nil {
		t.Fatal(err)
	}
	if ev := waitFor(t, ch, KindApprovalResolved); ev.By != "tui" {
		t.Fatalf("resolved by %q, want tui", ev.By)
	}
	if ev := waitFor(t, ch, KindTurnEnd); ev.Error != "" {
		t.Fatalf("turn end = %+v", ev)
	}
	if got := r.Meta().ID; got != local.Meta().ID || got == "" {
		t.Fatalf("remote meta id = %q, local = %q", got, local.Meta().ID)
	}
}

// A client that attaches while a turn is already blocked never saw the request
// event, so the dialog has to come from somewhere else.
func TestRemoteReportsPendingApproval(t *testing.T) {
	local, r := remotePair(t)
	if err := r.Submit("hello", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		if id, req := r.Pending(); req != nil && id != "" {
			if req.Tool != "bash" {
				t.Fatalf("pending = %+v", req)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("the remote never learned about the open approval")
		case <-time.After(5 * time.Millisecond):
		}
	}
	id, _ := local.Pending()
	if remoteID, _ := r.Pending(); remoteID != id {
		t.Fatalf("remote holds approval %q, the session holds %q", remoteID, id)
	}
}

// Interrupting has to reach the agent, not just the client's own view of it.
func TestRemoteInterrupts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	local := New(Config{
		Tools: reg,
		NewAgent: func(confirm agent.ConfirmFunc) *agent.Agent {
			return agent.New(&blockingClient{release: release}, reg, 0.3, confirm, "")
		},
		Ring: 256,
	})
	t.Cleanup(local.Close)
	srv := newTestDaemon(t, local)
	r, err := Dial(srv.base, srv.id, srv.token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)

	ch, stop, err := r.Subscribe(Client{ID: "tui"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := r.Submit("work", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, KindTurnStart)
	r.Interrupt()
	if ev := waitFor(t, ch, KindTurnEnd); !ev.Interrupted {
		t.Fatalf("turn end = %+v, want it marked interrupted", ev)
	}
}

// The socket dropping is ordinary. Losing the middle of the conversation to it
// is not: the follower resumes from the last event it delivered.
func TestRemoteResumesAfterADrop(t *testing.T) {
	local, r := remotePair(t)
	ch, stop, err := r.Subscribe(Client{ID: "tui"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	local.emit(Event{Kind: KindNotice, Text: "before"})
	if ev := waitFor(t, ch, KindNotice); ev.Text != "before" {
		t.Fatalf("first notice = %q", ev.Text)
	}

	// Drop the connection from under the follower and emit into the gap.
	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()
	if conn == nil {
		t.Fatal("not connected")
	}
	_ = conn.Close()
	local.emit(Event{Kind: KindNotice, Text: "during"})
	local.emit(Event{Kind: KindNotice, Text: "after"})

	var seen []string
	deadline := time.After(10 * time.Second)
	for len(seen) < 2 {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream closed")
			}
			// The reconnect itself is announced as a notice; ignore those.
			if ev.Kind == KindNotice && !strings.HasPrefix(ev.Text, "attach:") {
				seen = append(seen, ev.Text)
			}
		case <-deadline:
			t.Fatalf("only got %v after the drop; the gap was not replayed", seen)
		}
	}
	if seen[0] != "during" || seen[1] != "after" {
		t.Fatalf("replayed %v, want [during after]", seen)
	}
}
