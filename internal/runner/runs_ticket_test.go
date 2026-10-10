package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
)

// reply is one scripted model answer. It gets the turn's context, so a reply can wait for an
// interrupt the way a slow model would.
type reply func(context.Context) (llm.Message, error)

// scripted is a model that answers from a queue, and says "nothing more" once it is empty.
type scripted struct {
	mu      sync.Mutex
	replies []reply
}

func (s *scripted) then(r ...reply) {
	s.mu.Lock()
	s.replies = append(s.replies, r...)
	s.mu.Unlock()
}

func (s *scripted) Stream(ctx context.Context, _ []llm.Message, _ []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	s.mu.Lock()
	next := say("nothing more")
	if len(s.replies) > 0 {
		next, s.replies = s.replies[0], s.replies[1:]
	}
	s.mu.Unlock()
	return next(ctx)
}

func (*scripted) Tokenize(_ context.Context, text string) (int, error) { return len(text) / 4, nil }
func (*scripted) Endpoint() string                                     { return "test" }

func (*scripted) Model() llm.ModelInfo { return llm.ModelInfo{Provider: "test", ID: "model"} }

func say(text string) reply {
	return func(context.Context) (llm.Message, error) {
		return llm.Message{Role: llm.RoleAssistant, Content: text}, nil
	}
}

var callSeq atomic.Int64

func call(name, args string) reply {
	return func(context.Context) (llm.Message, error) {
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: fmt.Sprintf("call-%d", callSeq.Add(1)), Type: "function",
			Function: llm.FunctionCall{Name: name, Arguments: args},
		}}}, nil
	}
}

// scriptedOpen builds real sessions against s, rooted the way the daemon roots them.
func scriptedOpen(s *scripted, dir string) OpenRun {
	return func(_ context.Context, req RunRequest) (*Session, Opened, error) {
		root := req.Root(dir)
		reg, err := tools.NewRegistry(root)
		if err != nil {
			return nil, Opened{}, err
		}
		sess := NewSession(Spec{Mode: req.Mode, Profile: req.Profile, Tools: reg, Backend: llm.NewRef(s),
			Title: req.Title})
		return sess, Opened{Model: "test/model", Root: root}, nil
	}
}

func nextTurn(t *testing.T, turns <-chan uisession.Event) uisession.Event {
	t.Helper()
	select {
	case ev := <-turns:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no turn event arrived")
		return uisession.Event{}
	}
}

type probe struct{ ran atomic.Bool }

func (*probe) Name() string            { return "probe" }
func (*probe) Description() string     { return "a test probe" }
func (*probe) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (*probe) NeedsConfirm() bool      { return false }
func (p *probe) Run(context.Context, json.RawMessage) (string, error) {
	p.ran.Store(true)
	return "ok", nil
}

func TestATicketRunIsAutonomousRootedAtItsWorktreeAndReportsItsTurns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &scripted{}
	s.then(call("probe", `{}`), say("finished"))
	runs, err := NewRuns(RunsConfig{Open: scriptedOpen(s, t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	wt := t.TempDir()
	p := &probe{}
	turns := make(chan uisession.Event, 8)
	v, err := runs.Create(context.Background(), RunRequest{
		Mode: ModeAutonomous, TicketID: "TCK-1", Worktree: wt, Branch: "aigem/TCK-1",
		Tools: []tools.Tool{p}, OnTurn: func(ev uisession.Event) { turns <- ev },
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Mode != ModeAutonomous || v.TicketID != "TCK-1" || v.Worktree != wt || v.Branch != "aigem/TCK-1" ||
		v.Root != wt {
		t.Fatalf("run = %+v", v)
	}
	if err := runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if ev := nextTurn(t, turns); ev.Kind != uisession.KindTurnStart {
		t.Fatalf("first event = %s, want turn_start", ev.Kind)
	}
	if ev := nextTurn(t, turns); ev.Kind != uisession.KindTurnEnd || ev.Text != "finished" {
		t.Fatalf("second event = %s %q, want turn_end with the last message", ev.Kind, ev.Text)
	}
	if !p.ran.Load() {
		t.Error("the extra tool was not offered past the autonomous tool subset")
	}
}

func TestARunIsRootedAtItsWorktreeWhenItHasOne(t *testing.T) {
	if got := (RunRequest{}).Root("/p"); got != "/p" {
		t.Errorf("a plain run = %q, want the project", got)
	}
	wt := "/p/.aigem/worktrees/TCK-1"
	if got := (RunRequest{Worktree: wt}).Root("/p"); got != wt {
		t.Errorf("a ticket run = %q, want its worktree", got)
	}
	if got := (RunRequest{Dir: "/p/api"}).Root("/p"); got != "/p/api" {
		t.Errorf("a run with a dir = %q, want its dir", got)
	}
}

func TestALostReplayStandsInForTheTurnEnd(t *testing.T) {
	evs := []uisession.Event{
		{Seq: 1, Kind: uisession.KindTurnStart}, {Seq: 2, Kind: uisession.KindContent},
		{Seq: 3, Kind: uisession.KindTurnEnd, Text: "bye"},
	}
	if got := turnEvents(evs, nil, false); len(got) != 2 || got[1].Text != "bye" {
		t.Errorf("turn events = %+v", got)
	}
	if got := turnEvents(nil, uisession.ErrTruncated, true); len(got) != 0 {
		t.Errorf("a lost replay mid-turn = %+v, want nothing until the turn ends", got)
	}
	got := turnEvents(nil, uisession.ErrTruncated, false)
	if len(got) != 1 || got[0].Kind != uisession.KindTurnEnd ||
		!strings.Contains(got[0].Error, "could not be read back") {
		t.Errorf("a lost replay after the turn = %+v, want a turn_end with the error", got)
	}
}

func TestAnAutonomousRunNeedsATicketAndAWorktreeOrOnlyReads(t *testing.T) {
	runs, err := NewRuns(RunsConfig{Open: scriptedOpen(&scripted{}, t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)
	for _, req := range []RunRequest{
		{Mode: ModeAutonomous, TicketID: "TCK-1"},
		{Mode: ModeAutonomous, Worktree: t.TempDir()},
		{Mode: ModeAutonomous, TicketID: "TCK-1", Dir: t.TempDir()},
		{Mode: ModeAutonomous, TicketID: "TCK-1", Dir: t.TempDir(), Profile: "shell"},
		{Mode: ModeAutonomous, TicketID: "TCK-1", Profile: readOnlyProfile},
		{Mode: ModeAutonomous, TicketID: "TCK-1", Worktree: t.TempDir(), Profile: "nope"},
		{Mode: ModeAutonomous, TicketID: "TCK-1", Worktree: t.TempDir(), Profile: "shell"},
		{Mode: ModeAutonomous, TicketID: "TCK-1", Worktree: t.TempDir(), Profile: "dangerous-shell"},
		{Profile: readOnlyProfile},
	} {
		if _, err := runs.Create(context.Background(), req); !errors.Is(err, ErrRunMode) {
			t.Errorf("Create(%+v) = %v, want ErrRunMode", req, err)
		}
	}
}

func TestAReadOnlyRunIsRootedAtItsDirAndCannotWrite(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &scripted{}
	s.then(call("write_file", `{"path":"x.txt","content":"x"}`), call("probe", `{}`), say("read it"))
	runs, err := NewRuns(RunsConfig{Open: scriptedOpen(s, t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	dir := t.TempDir()
	p := &probe{}
	turns := make(chan uisession.Event, 8)
	v, err := runs.Create(context.Background(), RunRequest{
		Mode: ModeAutonomous, Profile: readOnlyProfile, TicketID: "TCK-1", Dir: dir,
		Tools: []tools.Tool{p}, OnTurn: func(ev uisession.Event) { turns <- ev },
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Root != dir || v.Worktree != "" || v.TicketID != "TCK-1" {
		t.Fatalf("run = %+v", v)
	}
	if err := runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: "plan"}); err != nil {
		t.Fatal(err)
	}
	nextTurn(t, turns)
	if ev := nextTurn(t, turns); ev.Kind != uisession.KindTurnEnd {
		t.Fatalf("second event = %s, want turn_end", ev.Kind)
	}
	if pathExists(filepath.Join(dir, "x.txt")) {
		t.Error("a read-only run wrote a file")
	}
	if !p.ran.Load() {
		t.Error("the extra tool was not offered past the read-only subset")
	}
}

func TestStopEndsTheSessionAndKeepsTheRecord(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var mu sync.Mutex
	var told []RunView
	runs, err := NewRuns(RunsConfig{
		Open: scriptedOpen(&scripted{}, t.TempDir()),
		Notify: func(v RunView) {
			mu.Lock()
			told = append(told, v)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)
	v, err := runs.Create(context.Background(), RunRequest{Title: "stop me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runs.Stop(v.ID); err != nil {
		t.Fatal(err)
	}
	got, err := runs.Get(v.ID)
	if err != nil || got.Live || got.Status != RunClosed {
		t.Fatalf("after Stop = %+v, %v, want a closed record", got, err)
	}
	mu.Lock()
	last := told[len(told)-1]
	mu.Unlock()
	if last.ID != v.ID || last.Live || last.Status != RunClosed {
		t.Errorf("last announcement = %+v, want the closed run", last)
	}
	if err := runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: "x"}); !errors.Is(err, ErrRunClosed) {
		t.Errorf("submit after Stop = %v, want ErrRunClosed", err)
	}
	if err := runs.Stop(v.ID); !errors.Is(err, ErrRunClosed) {
		t.Errorf("second Stop = %v, want ErrRunClosed", err)
	}
	if err := runs.Stop("RUN-99"); !errors.Is(err, ErrNoRun) {
		t.Errorf("Stop of an unknown run = %v, want ErrNoRun", err)
	}
}
