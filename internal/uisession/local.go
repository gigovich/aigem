package uisession

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
)

// Session is what a front-end drives. Local runs the agent in this process;
// a remote implementation speaks the same events over a websocket, which is
// what lets a terminal attach to a session a browser started.
type Session interface {
	Subscribe(c Client, since uint64) (<-chan Event, func(), error)
	Replay(since uint64) ([]Event, error)

	Submit(text string, images []llm.Image) error
	Interrupt()
	Command(name, args string) error
	Resolve(id string, d Decision, by string) error
}

var _ Session = (*Local)(nil)

var (
	// ErrTruncated means the requested history is no longer held and the client
	// should start from scratch rather than render a hole.
	ErrTruncated = errors.New("event history truncated")
	// ErrClosed is returned once the session has been closed.
	ErrClosed = errors.New("session closed")
	// ErrUnknownCommand is returned by Command for a name nothing handles.
	ErrUnknownCommand = errors.New("unknown command")
	// ErrBusy is returned when a turn is already running and the caller asked
	// for one that cannot be folded into it.
	ErrBusy = errors.New("a turn is already running")
)

// Session tool policy values, set by an "Always"/"Forbid" answer and consulted
// for the rest of the session.
const (
	policyAllow = "allow"
	policyDeny  = "deny"
)

// defaultRing bounds the in-memory history a late subscriber can catch up on.
// It is generous because the cost is one struct per event and the alternative -
// a client that reconnects into a hole - is much worse. The on-disk journal
// replaces it as the source of truth.
const defaultRing = 8192

// CommandFunc handles one named command. It runs with no lock held.
type CommandFunc func(args string) error

// Config describes a session. NewAgent is a factory rather than a ready agent
// because the confirmation function belongs to the session, not to its caller,
// and because switching model has to build a new agent around the same policy.
type Config struct {
	Tools    *tools.Registry
	NewAgent func(confirm agent.ConfirmFunc) *agent.Agent
	// AutoMode approves anything a tool can undo without asking. A destructive
	// command still asks.
	AutoMode bool
	// Ring bounds the retained event history; zero picks a default.
	Ring int
}

// Local is a session whose agent runs in this process.
type Local struct {
	mu    sync.Mutex
	tools *tools.Registry
	ag    *agent.Agent

	autoMode   bool
	toolPolicy map[string]string

	seq     uint64
	ring    []Event
	ringCap int

	subs   map[string]*subscriber
	subSeq uint64

	active      *pending
	queue       []*pending
	approvalSeq uint64

	artifacts map[string]tools.FileChange

	commands map[string]CommandFunc

	running bool
	cancel  context.CancelFunc

	done   chan struct{}
	closed bool
}

// New builds a session. The registry's path approver and file-change hook are
// taken over here: a front-end that also set them would be answering questions
// the session is meant to own.
func New(cfg Config) *Local {
	ring := cfg.Ring
	if ring <= 0 {
		ring = defaultRing
	}
	l := &Local{
		tools:      cfg.Tools,
		autoMode:   cfg.AutoMode,
		toolPolicy: map[string]string{},
		ringCap:    ring,
		subs:       map[string]*subscriber{},
		artifacts:  map[string]tools.FileChange{},
		commands:   map[string]CommandFunc{},
		done:       make(chan struct{}),
	}
	if cfg.NewAgent != nil {
		l.ag = cfg.NewAgent(l.confirmTool)
	}
	if l.tools != nil {
		// Persisted grants are enabled here and nowhere else: an unattended run
		// must not inherit a directory a human approved for the same directory.
		l.tools.SetPathGrants(true)
		l.tools.SetPathApprover(l.approvePath)
		l.tools.OnFileChange(l.recordFileChange)
	}
	return l
}

// Agent exposes the running agent. It exists for the front-end work that has
// not moved here yet; new code should prefer a method on the session.
func (l *Local) Agent() *agent.Agent { return l.ag }

// Handle registers a command handler, letting a front-end contribute commands
// without this package importing it.
func (l *Local) Handle(name string, fn CommandFunc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.commands[name] = fn
}

// Command runs a named command.
func (l *Local) Command(name, args string) error {
	l.mu.Lock()
	fn, ok := l.commands[name]
	l.mu.Unlock()
	if !ok {
		return ErrUnknownCommand
	}
	return fn(args)
}

// Close ends the session: the running turn is cancelled, every parked approval
// is refused so no tool call is left waiting forever, and every subscriber's
// channel is closed.
func (l *Local) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	if l.cancel != nil {
		l.cancel()
	}
	l.failPendingLocked()
	subs := l.subs
	l.subs = map[string]*subscriber{}
	l.mu.Unlock()

	close(l.done)
	for _, s := range subs {
		s.stop()
	}
}

func (l *Local) nextApprovalID() string {
	l.approvalSeq++
	return "ap-" + strconv.FormatUint(l.approvalSeq, 10)
}

// ---- event fan-out ----

// emit publishes an event to the journal ring and every subscriber.
func (l *Local) emit(ev Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emitLocked(ev)
}

// emitLocked assigns the sequence number and fans out. Numbering and fan-out
// happen under one lock so no subscriber can see events in an order the journal
// disagrees with.
func (l *Local) emitLocked(ev Event) {
	l.seq++
	ev.Seq = l.seq
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	l.ring = append(l.ring, ev)
	if len(l.ring) > l.ringCap {
		l.ring = l.ring[len(l.ring)-l.ringCap:]
	}
	for _, s := range l.subs {
		s.push(ev)
	}
}

// firstSeqLocked is the oldest sequence number still retained, or 0 when
// nothing has been emitted.
func (l *Local) firstSeqLocked() uint64 {
	if len(l.ring) == 0 {
		return 0
	}
	return l.ring[0].Seq
}

// Replay returns the retained events after since, oldest first. It reports
// ErrTruncated when the gap has already been evicted, which a client answers by
// reloading rather than by rendering a hole.
func (l *Local) Replay(since uint64) ([]Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.replayLocked(since)
}

func (l *Local) replayLocked(since uint64) ([]Event, error) {
	first := l.firstSeqLocked()
	if first > since+1 {
		return nil, ErrTruncated
	}
	out := make([]Event, 0, len(l.ring))
	for _, ev := range l.ring {
		if ev.Seq > since {
			out = append(out, ev)
		}
	}
	return out, nil
}

// Subscribe attaches a client and returns its event channel plus a function to
// detach. The backlog after since is loaded before the subscriber is registered
// for live events, so there is no window in which an event is neither replayed
// nor delivered - the gap a separate Replay-then-Subscribe would leave open.
func (l *Local) Subscribe(c Client, since uint64) (<-chan Event, func(), error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, nil, ErrClosed
	}
	backlog, err := l.replayLocked(since)
	if err != nil {
		l.mu.Unlock()
		return nil, nil, err
	}
	l.subSeq++
	if c.ID == "" {
		c.ID = "c-" + strconv.FormatUint(l.subSeq, 10)
	}
	s := newSubscriber(c, l.ringCap, backlog)
	l.subs[c.ID] = s
	l.emitLocked(l.presenceLocked())
	l.mu.Unlock()

	go s.run()
	var once sync.Once
	return s.out, func() { once.Do(func() { l.unsubscribe(c.ID) }) }, nil
}

func (l *Local) unsubscribe(id string) {
	l.mu.Lock()
	s := l.subs[id]
	if s == nil {
		l.mu.Unlock()
		return
	}
	delete(l.subs, id)
	if !l.closed {
		l.emitLocked(l.presenceLocked())
	}
	l.mu.Unlock()
	s.stop()
}

// presenceLocked builds the current attendance. It matters more than it looks:
// an unanswered approval blocks the turn, and without knowing whether anyone is
// watching, a front-end cannot tell "thinking" from "waiting for someone who
// walked away".
func (l *Local) presenceLocked() Event {
	clients := make([]Client, 0, len(l.subs))
	for _, s := range l.subs {
		clients = append(clients, s.client)
	}
	return Event{Kind: KindPresence, Clients: clients}
}

// ---- turns ----

// Submit sends a user message. During a running turn the text is injected into
// it rather than queued or refused: on a phone the answer to "it is still
// working" is usually another sentence, not a wait.
func (l *Local) Submit(text string, images []llm.Image) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return ErrClosed
	}
	if l.ag == nil {
		l.mu.Unlock()
		return errors.New("session has no agent")
	}
	if l.running {
		ag := l.ag
		l.mu.Unlock()
		if l.injectInto(ag, text) {
			l.emit(Event{Kind: KindUserMessage, Text: text, Images: len(images), Injected: true})
			return nil
		}
		// The turn ended between the check and the injection; fall through and
		// start a new one.
		l.mu.Lock()
		switch {
		case l.closed:
			l.mu.Unlock()
			return ErrClosed
		case l.running:
			l.mu.Unlock()
			return ErrBusy
		}
	}
	l.startLocked(text, len(images), func(ctx context.Context, ev agent.Events) (string, error) {
		return l.ag.RunWithImages(ctx, text, images, ev)
	})
	l.mu.Unlock()
	return nil
}

// injectWindow bounds how long Submit waits for a just-started turn to become
// injectable. The agent marks itself running from the turn's own goroutine, so
// a message sent immediately after a turn starts can arrive before that flag is
// set - a race a person cannot see and should not be told about.
const injectWindow = 100 * time.Millisecond

func (l *Local) injectInto(ag *agent.Agent, text string) bool {
	deadline := time.Now().Add(injectWindow)
	for {
		if ag.Inject(text) {
			return true
		}
		// The turn is over: the caller starts a fresh one rather than waiting.
		if !l.Running() || time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// Run starts a turn from something other than a typed message - a skill, an MCP
// prompt, a compaction - with display standing in for the user line.
func (l *Local) Run(display string, run func(context.Context, agent.Events) (string, error)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if l.running {
		return ErrBusy
	}
	l.startLocked(display, 0, run)
	return nil
}

func (l *Local) startLocked(display string, images int,
	run func(context.Context, agent.Events) (string, error)) {
	l.running = true
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.emitLocked(Event{Kind: KindUserMessage, Text: display, Images: images})
	l.emitLocked(Event{Kind: KindTurnStart})
	ev := l.agentEvents()
	go func() {
		defer cancel()
		answer, err := run(ctx, ev)
		l.finishTurn(answer, err)
	}()
}

func (l *Local) finishTurn(answer string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running = false
	l.cancel = nil
	out := Event{Kind: KindTurnEnd, Text: answer}
	switch {
	case errors.Is(err, context.Canceled):
		out.Interrupted = true
	case err != nil:
		out.Error = err.Error()
	}
	l.emitLocked(out)
}

// Interrupt cancels the running turn. It is a no-op when nothing is running.
func (l *Local) Interrupt() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancel != nil {
		l.cancel()
	}
}

// Running reports whether a turn is in progress.
func (l *Local) Running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

func (l *Local) recordFileChange(c tools.FileChange) {
	l.mu.Lock()
	// Keep the content as it was before this session first touched the file, so
	// the diff shown at the end is the session's whole effect rather than the
	// last edit's.
	if prev, ok := l.artifacts[c.Path]; ok {
		c.Old, c.Created = prev.Old, prev.Created
	}
	l.artifacts[c.Path] = c
	l.emitLocked(Event{Kind: KindFileChanged, Path: c.Path, Created: c.Created})
	l.mu.Unlock()
}

// Artifacts returns the files this session changed, with the content before and
// after, keyed by path.
func (l *Local) Artifacts() map[string]tools.FileChange {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]tools.FileChange, len(l.artifacts))
	for k, v := range l.artifacts {
		out[k] = v
	}
	return out
}

// agentEvents bridges the agent's callbacks onto the event stream.
func (l *Local) agentEvents() agent.Events {
	return agent.Events{
		OnContent:          func(d string) { l.emit(Event{Kind: KindContent, Text: d}) },
		OnReasoning:        func(d string) { l.emit(Event{Kind: KindReasoning, Text: d}) },
		OnAssistantMessage: func(c string) { l.emit(Event{Kind: KindAssistantMessage, Text: c}) },
		OnNotice:           func(t string) { l.emit(Event{Kind: KindNotice, Text: t}) },
		OnUsage:            func(n int) { l.emit(Event{Kind: KindUsage, Tokens: n}) },
		OnTodoUpdate:       func(td []agent.TodoItem) { l.emit(Event{Kind: KindTodo, Todos: td}) },
		OnBudgetExhausted:  func(r string) { l.emit(Event{Kind: KindBudgetExhausted, Text: r}) },
		OnToolBatch: func(round int, calls []agent.ToolCallRef) {
			out := make([]Call, len(calls))
			for i, c := range calls {
				out[i] = Call{ID: c.ID, Name: c.Name}
			}
			l.emit(Event{Kind: KindToolBatch, Round: round, Calls: out})
		},
		OnToolStart: func(id, name string, args json.RawMessage) {
			l.emit(Event{Kind: KindToolStart, ID: id, Name: name, Args: args})
		},
		OnToolEnd: func(id, name, result string, err error) {
			l.emit(Event{Kind: KindToolEnd, ID: id, Name: name, Text: result, Error: errText(err)})
		},
		OnAgentStart: func(id, name, prompt string) {
			l.emit(Event{Kind: KindAgentStart, ID: id, Agent: name, Text: prompt})
		},
		OnAgentEnd: func(id, result string, err error) {
			l.emit(Event{Kind: KindAgentEnd, ID: id, Text: result, Error: errText(err)})
		},
		OnSubToolStart: func(runID, name, callID, tool string, args json.RawMessage) {
			l.emit(Event{Kind: KindSubToolStart, RunID: runID, Agent: name,
				ID: callID, Name: tool, Args: args})
		},
		OnSubToolEnd: func(runID, name, callID, tool, result string, err error) {
			l.emit(Event{Kind: KindSubToolEnd, RunID: runID, Agent: name,
				ID: callID, Name: tool, Text: result, Error: errText(err)})
		},
		OnSubNotice: func(runID, name, text string) {
			l.emit(Event{Kind: KindSubNotice, RunID: runID, Agent: name, Text: text})
		},
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
