package uisession

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/session"
	"github.com/gigovich/aigem/internal/tools"
)

// Session is what a front-end drives. Local runs the agent in this process,
// and is the only implementation today; the interface stays because a
// front-end that is not in this process could implement it too, without
// changing what a front-end is allowed to do.
// Session is the conversation surface, and deliberately only that. It holds
// what would work the same whether the agent runs here or is driven from
// elsewhere. Everything a front-end can do to a conversation is here;
// everything it can ask *about* one arrives as an event, so a non-local
// implementation would not be a chain of round trips.
//
// What is not here is what cannot cross: running a turn from a closure - a
// skill, an MCP prompt, a compaction - needs the agent itself, so a non-local
// implementation would need some other path for those.
type Session interface {
	Subscribe(c Client, since uint64) (<-chan Event, func(), error)
	Replay(since uint64) ([]Event, error)

	Submit(text string, images []llm.Image) error
	Interrupt()
	Command(name, args string) error
	Resolve(id string, d Decision, by string) error

	// Meta identifies the conversation. Pending is the approval blocking it, if
	// any: a front-end that attaches mid-turn has to draw a dialog whose request
	// event it never saw.
	Meta() session.Meta
	Pending() (string, *Approval)
	Close()
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

// closeWait bounds how long Close waits for a running turn to unwind. A turn
// that will not - a tool blocked on something no cancellation reaches - must
// not hold the process on its way out, and the caller of Close is usually a
// defer on the exit path with a terminal already torn down and nothing left to
// explain the pause. Giving up is the race Close used to lose every time; a
// bound only leaves it to the cases that were going to hang anyway.
//
// A variable so a test can shrink it: the behaviour that matters only appears
// once a turn outlives it, and a test that waits five real seconds is a test
// nobody runs.
var closeWait = 5 * time.Second

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

	// Hooks is told the session id as conversations start and end, so a hook can
	// attribute what it sees.
	Hooks *hooks.Runner
	// Title names the conversation before its first turn does; a SessionStart
	// hook is the only thing that supplies one.
	Title string
	// ModelRef reports the model reference to record with a saved session, so it
	// can be restored on resume. The model can change during a session, which is
	// why this is a function rather than a value.
	ModelRef func() string
	// RebuildSystem reassembles the system prompt. It is called when a fresh
	// conversation starts, so edits to the project instruction files take effect
	// without a restart.
	RebuildSystem func() string

	// Models resolves a model reference; Backend is the shared handle every
	// caller streams through, so switching model swaps what is inside it rather
	// than replacing the handle.
	Models  *llm.Registry
	Backend *llm.Ref
	// MaxTokens caps one response. CtxSize is the context window to fall back on
	// for a model that declares none.
	MaxTokens int
	CtxSize   int
	Compact   agent.CompactConfig
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

	// Conversation identity. The id is assigned on the first turn; see
	// beginLocked.
	id    string
	title string
	start time.Time

	hooks         *hooks.Runner
	modelRef      func() string
	rebuildSystem func() string

	models     *llm.Registry
	backend    *llm.Ref
	maxTokens  int
	defaultCtx int
	ctxSize    int
	compact    agent.CompactConfig

	running bool
	cancel  context.CancelFunc
	// turns counts the goroutines running a turn, so Close can wait for one to
	// unwind. Not guarded by mu: Close waits on it after releasing the lock,
	// which is the whole point - finishTurn takes mu.
	turns sync.WaitGroup

	journal *journal

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

		title:         cfg.Title,
		hooks:         cfg.Hooks,
		modelRef:      cfg.ModelRef,
		rebuildSystem: cfg.RebuildSystem,

		models:     cfg.Models,
		backend:    cfg.Backend,
		maxTokens:  cfg.MaxTokens,
		defaultCtx: cfg.CtxSize,
		ctxSize:    cfg.CtxSize,
		compact:    cfg.Compact,
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
//
// It then waits for the cancelled turn to unwind, because a turn is saved after
// the event that says it ended - so a caller that closed on seeing that event
// would otherwise race a write it has no way to know about. The wait is bounded
// by closeWait and then abandoned: a turn parked on something no cancellation
// reaches must not hold the process on its way out.
func (l *Local) Close() {
	l.mu.Lock()
	first := !l.closed
	if first {
		l.closed = true
		if l.cancel != nil {
			l.cancel()
		}
		l.failPendingLocked()
		l.journal.close()
	}
	subs := l.subs
	l.subs = map[string]*subscriber{}
	l.mu.Unlock()

	if first {
		close(l.done)
	}
	for _, s := range subs {
		s.stop()
	}
	// Every caller waits, not just the first. A second Close returning while the
	// first is still waiting would hand its caller the guarantee this exists to
	// give without the wait that makes it true.
	//
	// The wait comes after the cancel, after failPendingLocked and after
	// close(l.done), so the turn is unwinding rather than parked on an approval
	// nobody is left to answer. What is being waited for is the save at the end
	// of it.
	l.waitForTurns()
}

// waitForTurns blocks until every running turn has finished, or closeWait has
// passed. The goroutine outlives a timeout, which is what the bound is for: the
// process is on its way out and a turn that will not unwind should not decide
// when it gets there.
func (l *Local) waitForTurns() {
	done := make(chan struct{})
	go func() {
		l.turns.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeWait):
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
	// Subscribers get the event whole; what is kept is trimmed, so an oversized
	// tool result does not sit in memory or ship on every reconnect.
	kept := l.journalled(ev)
	l.journal.append(kept)
	l.ring = append(l.ring, kept)
	if len(l.ring) > l.ringCap {
		l.ring = l.ring[len(l.ring)-l.ringCap:]
	}
	for _, s := range l.subs {
		s.Push(ev)
	}
}

// Seq is the sequence number of the most recent event, which is what a client
// passes back as since to resume from.
func (l *Local) Seq() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
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
		// Beyond what is held in memory, the journal is the record. Only a session
		// that never reached its first turn has none.
		if l.id != "" {
			if evs, err := readJournal(l.id, since); err == nil {
				return evs, nil
			}
		}
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
	s := newSubscriber(c, l.ringCap, backlog, 0)
	prev := l.subs[c.ID]
	l.subs[c.ID] = s
	l.emitLocked(l.presenceLocked())
	l.mu.Unlock()
	// Replacing an id without stopping the old one would leave its pump parked
	// forever on a channel nothing closes.
	prev.stop()

	go s.Run()
	var once sync.Once
	return s.Out(), func() { once.Do(func() { l.unsubscribe(c.ID) }) }, nil
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
	l.startLocked(text, submitTitle(text, len(images)), len(images),
		func(ctx context.Context, ev agent.Events) (string, error) {
			return l.ag.RunWithImages(ctx, text, images, ev)
		})
	l.mu.Unlock()
	return nil
}

// submitTitle names a conversation after its first message. A message that is
// only images has no text to name it after, and "(untitled)" says less than
// what was actually sent.
func submitTitle(text string, images int) string {
	if strings.TrimSpace(text) == "" && images > 0 {
		if images == 1 {
			return "1 image"
		}
		return strconv.Itoa(images) + " images"
	}
	return session.Title(text)
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
// prompt, a compaction - with display standing in for the user line and title
// naming the conversation if this is its first turn.
func (l *Local) Run(display, title string, run func(context.Context, agent.Events) (string, error)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if l.running {
		return ErrBusy
	}
	l.startLocked(display, title, 0, run)
	return nil
}

func (l *Local) startLocked(display, title string, images int,
	run func(context.Context, agent.Events) (string, error)) {
	l.beginLocked(title)
	l.running = true
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.emitLocked(Event{Kind: KindUserMessage, Text: display, Images: images})
	l.emitLocked(Event{Kind: KindTurnStart})
	ev := l.agentEvents()
	// Tracked so Close can wait for it. The turn outlives the event that says it
	// ended - finishTurn emits KindTurnEnd and only then saves - so a caller that
	// closed on seeing that event would otherwise race a write it has no way to
	// know about. A test whose t.TempDir is being removed is the visible half of
	// that; a process tearing down its state directory is the other.
	l.turns.Add(1)
	go func() {
		defer l.turns.Done()
		defer cancel()
		answer, err := run(ctx, ev)
		l.finishTurn(answer, err)
	}()
}

func (l *Local) finishTurn(answer string, err error) {
	l.mu.Lock()
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
	l.mu.Unlock()

	// Persist at the end of every turn rather than only when a front-end
	// remembers to. A conversation this process is holding has no one to save
	// it on the way out if the process dies, and a turn that took a minute to
	// produce should not be lost to a crash in the next one.
	if saveErr := l.Save(); saveErr != nil {
		l.Notice("could not save session: " + saveErr.Error())
	}
}

// Notice publishes a line of prose to the session. It is how something outside
// the agent - a provider retry, a failed save - says so where the conversation
// can show it.
func (l *Local) Notice(text string) {
	l.emit(Event{Kind: KindNotice, Text: text})
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

// RecordFileChange notes a change the session did not make through a tool. It
// exists for tests and for a front-end that edited a file itself; the tool
// registry reports its own.
func (l *Local) RecordFileChange(path, old, updated string, created bool) {
	l.recordFileChange(tools.FileChange{Path: path, Old: old, New: updated, Created: created})
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

// agentEvents bridges the agent's callbacks onto this session's event stream.
func (l *Local) agentEvents() agent.Events { return Bridge(l.emit) }
