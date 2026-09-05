package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/session"
	"github.com/gigovich/aigem/internal/store"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
)

// The daemon's runs: the conversations it has open, the record it keeps of each
// one, and the operations a front-end performs on them.
//
// A run is one uisession.Local and one journal - the same conversation the
// terminal opens, reached over HTTP instead of over a keyboard. What this type
// adds is the table: an id a URL can name, a record that outlives the process,
// and a lifecycle the daemon can shut down as a whole.
//
// Building the session is deliberately not here. Choosing a model, opening a
// credential and loading an environment are the binary's job, and doing them
// here would put the whole of internal/llm and internal/auth behind every test
// of the table. Open is what a caller supplies instead.

var (
	// ErrNoRun is returned for an id the table does not hold.
	ErrNoRun = errors.New("runner: no such run")
	// ErrRunClosed is returned for an operation that needs the live session of a
	// run whose session is gone: it was closed, or it belongs to a daemon that
	// has since restarted. Its timeline is still readable from the journal.
	ErrRunClosed = errors.New("runner: this run is closed")
	// ErrRunsClosed is returned once the registry itself has been shut down.
	ErrRunsClosed = errors.New("runner: the run registry is closed")
	// ErrRunMode is returned for a mode this build does not open runs in.
	ErrRunMode = errors.New("runner: unsupported run mode")
	// ErrTooManyRuns is returned once the daemon is holding as many live
	// conversations as it will.
	ErrTooManyRuns = errors.New("runner: too many conversations are open")
	// ErrNoBlob is returned when nothing was stored beside the journal for that
	// event: the result was short enough to be journalled whole, the event is
	// not a tool result at all, the run never had a turn and so has no journal
	// directory, or the write that would have kept the body failed - in which
	// case the event says so rather than promising one.
	ErrNoBlob = errors.New("runner: no stored body for that event")
)

// maxLiveRuns is how many conversations one daemon holds at once.
//
// A run is not cheap: a tools registry, a model handle, an event ring, and
// whatever the model's context holds. Nothing about the API stops a client
// looping on POST, and without a ceiling that loop is an out-of-memory with no
// backstop. The number is chosen the way the socket cap was - far past what a
// person opens and far short of what hurts - and a run that is closed gives its
// place back.
const maxLiveRuns = 32

// RunStatus is the durable state of a run. It is deliberately coarse: whether
// there is a session to talk to is what a client has to know before it opens a
// socket, and everything finer - running a turn, parked on an approval - is
// live state that no file can be right about.
type RunStatus string

const (
	// RunOpen means the session is live in this daemon.
	RunOpen RunStatus = "open"
	// RunClosed means the record and its journal remain and the session is gone.
	RunClosed RunStatus = "closed"
)

// Run is the durable half of a run: what survives a restart.
//
// SessionID is the id the journal is keyed by. It is stamped from the session
// the run was opened with, so a record has one from the start - but the journal
// itself is only written from the first turn, because a conversation opened and
// walked away from is not worth a file. Reading back a run that never had one
// is an empty timeline, not an error.
type Run struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId,omitempty"`
	Mode      Mode      `json:"mode"`
	Title     string    `json:"title,omitempty"`
	Model     string    `json:"model,omitempty"`
	Root      string    `json:"root,omitempty"`
	Status    RunStatus `json:"status"`
	Created   time.Time `json:"created"`
	// Updated is when this record last changed - opened, renamed, switched
	// model, closed. It is not the last thing that happened in the
	// conversation: a turn does not rewrite the table.
	Updated time.Time `json:"updated"`
}

// RunView is a run as a reader gets it: the record, plus what only the live
// session knows and no file can be right about.
type RunView struct {
	Run
	// Live is whether a session is attached. A closed run answers reads from
	// its journal and refuses everything else.
	Live bool
	// Running is whether a turn is in flight.
	Running bool
	// Waiting is whether the turn is parked on an approval nobody has answered.
	Waiting bool
	// Step is whether the person is asked about every tool call. It is the
	// inverse of the session's auto mode, and it is reported this way round
	// because "step mode" is the toggle a front-end draws.
	Step bool
	// Seq is the last event sequence the session has emitted, so a client can
	// tell whether it is caught up without opening the stream.
	Seq uint64
}

// RunRequest is what a client asked for when it created a run. Everything in it
// is a preference: OpenRun resolves the model and the root, and reports what it
// actually built in Opened.
type RunRequest struct {
	// Mode is the session policy. The zero value is interactive.
	Mode Mode
	// Title names the conversation before its first turn does.
	Title string
	// Model is the reference to open, in the "provider/id" form the wire uses.
	// Empty takes the daemon's default.
	Model string
}

// Opened is what OpenRun built. It is reported rather than assumed, because
// the model a run got is the one the registry could open and not the one the
// request named.
type Opened struct {
	Model string
	Root  string
	// There is deliberately no Title. A conversation's name belongs to the
	// conversation - a SessionStart hook may set it, the first message
	// otherwise does - and a second place to put one would only ever be the
	// same string read from the same session, or a disagreement with nothing to
	// settle it.
	// Release is called once the session has been saved and closed, for
	// whatever the caller allocated alongside it - an environment reference, a
	// worktree. It may be nil, and it is called exactly once per run.
	Release func()
}

// OpenRun builds the session behind a new run.
//
// It runs without the registry's lock held: it dials MCP servers and executes
// the SessionStart hook, either of which can take tens of seconds, and a table
// that could not be read while one run was starting would be a daemon that
// stops answering because somebody opened a chat.
type OpenRun func(ctx context.Context, req RunRequest) (*Session, Opened, error)

// RunsConfig configures a registry.
type RunsConfig struct {
	// Open builds the session behind a new run. It is required.
	Open OpenRun
	// Store persists the table. A nil store keeps the runs in memory alone,
	// which is what a daemon with nowhere to write falls back to: the table
	// still works, it just does not survive the process. Whether a conversation
	// can be opened at all in that state is Open's answer, not this one.
	Store *store.File[[]Run]
	// Notify is called with a run whose record has changed - opened, named by
	// its conversation, switched model, closed.
	//
	// It exists because most of those do not happen during an HTTP request. A
	// run gets its name on the first message, which arrives up a websocket, and
	// a daemon that only announced what its own routes did would leave every
	// other tab showing an untitled run until something else moved.
	//
	// It is called without the registry's lock, and must not block: whatever is
	// on the other end is a fan-out to connected pages, and a slow one would
	// hold up the conversation that caused it.
	Notify func(RunView)
	// Now is the clock, so a test can pin the timestamps it asserts on.
	Now func() time.Time
}

// Runs is the daemon's table of conversations.
type Runs struct {
	open   OpenRun
	notify func(RunView)
	now    func() time.Time

	// once makes Close idempotent, and makes a second caller wait for the first
	// rather than return while the conversations are still being saved.
	once sync.Once
	// opening counts the Creates that are past the closed check and still
	// building a session. Close waits on it, because a session built after the
	// shutdown has walked the table would otherwise be saved and closed after
	// the daemon has torn down the environment its SessionEnd hook runs in.
	opening sync.WaitGroup

	mu   sync.Mutex
	file *store.File[[]Run]
	// byID and order are the table: the map answers a lookup, and the slice
	// keeps the order runs were created in, which is the order a list is served
	// in and the one a client sees as stable.
	byID  map[string]*liveRun
	order []string
	// next is the highest run number handed out. It survives a restart through
	// the table, so a restarted daemon does not reuse an id its journal still
	// holds a timeline for.
	next int
	// pending counts the Creates that are building a session and have no row
	// yet. Without it the ceiling below counts only what has finished arriving,
	// and a client issuing its POSTs in parallel walks straight past it - the
	// window being exactly the MCP dial and the SessionStart hook.
	pending int
	closed  bool
}

// row is a record and its session, read together under the lock and used after
// it: everything about the live half takes the session's own mutex.
type row struct {
	rec  Run
	sess *Session
}

// liveRun is one row: the record, and the session while there is one.
type liveRun struct {
	rec  Run
	sess *Session
	// release is Opened.Release, cleared as it is called so that closing a run
	// twice does not release its environment twice.
	release func()
}

// NewRuns loads the table and returns the registry.
//
// Runs that were open belonged to a daemon that is gone: their sessions did not
// survive the process, and leaving them marked open would offer a client a
// socket onto nothing. They are brought to closed here, and the correction is
// written back, so a daemon that starts and dies again does not have to make it
// a second time.
func NewRuns(cfg RunsConfig) (*Runs, error) {
	if cfg.Open == nil {
		return nil, errors.New("runner: a run registry needs a way to open a session")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	notify := cfg.Notify
	if notify == nil {
		notify = func(RunView) {}
	}
	r := &Runs{
		open: cfg.Open, notify: notify, now: now,
		file: cfg.Store, byID: map[string]*liveRun{},
	}
	if cfg.Store == nil {
		return r, nil
	}
	saved, err := cfg.Store.Load()
	if err != nil {
		return nil, fmt.Errorf("runner: could not read the run table: %w", err)
	}
	stale := false
	for _, rec := range saved {
		if rec.ID == "" || r.byID[rec.ID] != nil {
			// A duplicate id would make one of the two rows unreachable, and a
			// row with no id is unreachable already. Dropping it is a correction
			// the next save records.
			stale = true
			continue
		}
		if rec.Status != RunClosed {
			rec.Status = RunClosed
			rec.Updated = now()
			stale = true
		}
		r.byID[rec.ID] = &liveRun{rec: rec}
		r.order = append(r.order, rec.ID)
		if n := runNumber(rec.ID); n > r.next {
			r.next = n
		}
	}
	if stale {
		r.mu.Lock()
		r.saveLocked()
		r.mu.Unlock()
	}
	return r, nil
}

// runNumber reads the counter back out of an id. An id this daemon did not
// mint - a table edited by hand, a format from another version - counts as
// zero, so it never lowers the next one.
func runNumber(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, runIDPrefix))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

const runIDPrefix = "RUN-"

// Create opens a run.
//
// The id is reserved before the session is built and is not returned to the
// pool if building fails: two runs must never be handed the same id, and an id
// is cheap.
func (r *Runs) Create(ctx context.Context, req RunRequest) (RunView, error) {
	if req.Mode == "" {
		req.Mode = ModeInteractive
	}
	if req.Mode != ModeInteractive {
		// Autonomous runs need a ticket and a dedicated worktree, neither of
		// which exists yet. Opening one anyway would be a session with the
		// autonomous policy and none of what the policy assumes.
		return RunView{}, fmt.Errorf("%w: %q", ErrRunMode, req.Mode)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return RunView{}, ErrRunsClosed
	}
	if n := r.liveLocked() + r.pending; n >= maxLiveRuns {
		r.mu.Unlock()
		return RunView{}, fmt.Errorf("%w: %d are open, and %d is the limit; close one first",
			ErrTooManyRuns, n, maxLiveRuns)
	}
	r.next++
	id := runIDPrefix + strconv.Itoa(r.next)
	// Both registered under the lock, next to the check they belong to: Close
	// sets closed under this lock and then waits, so nothing can join after the
	// wait has started, and the ceiling counts this run from the moment it is
	// allowed rather than from the moment it finishes.
	r.pending++
	r.opening.Add(1)
	r.mu.Unlock()
	// counted is cleared in the same critical section that adds the row, so the
	// two halves of the count never both hold this run. Doing it in a lock of
	// its own - which is what the deferred release does when the run never
	// arrives - let live+pending exceed the ceiling for as long as it took to
	// build a view and rewrite the table, and refused creates while slots were
	// free.
	counted := true
	uncount := func() {
		if counted {
			counted = false
			r.pending--
		}
	}
	defer func() {
		r.mu.Lock()
		uncount()
		r.mu.Unlock()
		r.opening.Done()
	}()

	sess, opened, err := r.open(ctx, req)
	if err != nil {
		// Whatever it allocated before failing is still the caller's to give
		// back. Today's Open returns a zero Opened on every error, but Release
		// is documented as called once per run and this path is the one that
		// would call it never.
		release(opened.Release)
		return RunView{}, err
	}
	if sess == nil || sess.Local == nil {
		release(opened.Release)
		return RunView{}, errors.New("runner: the run was opened without a session")
	}

	meta := sess.Local.Meta()

	r.mu.Lock()
	// Stamped under the lock, so the order runs were created in and the order
	// their timestamps read are the same one. Read before it, two runs opened
	// at once could be listed in an order their Created fields contradict.
	now := r.now()
	rec := Run{
		ID: id, SessionID: meta.ID, Mode: req.Mode,
		Title: meta.Title, Model: opened.Model, Root: opened.Root,
		Status: RunOpen, Created: now, Updated: now,
	}
	if r.closed {
		// The registry shut down while the session was starting. It is not in
		// the table, so nothing else will ever close it.
		r.mu.Unlock()
		closeSession(sess, opened.Release)
		return RunView{}, ErrRunsClosed
	}
	lr := &liveRun{rec: rec, sess: sess, release: opened.Release}
	r.byID[id] = lr
	r.order = append(r.order, id)
	// The row is the count now.
	uncount()
	r.saveLocked()
	r.mu.Unlock()

	v := view(rec, sess)
	r.notify(v)
	return v, nil
}

// List reports every run, oldest first.
func (r *Runs) List() []RunView {
	r.mu.Lock()
	rows := make([]row, 0, len(r.order))
	for _, id := range r.order {
		if lr := r.byID[id]; lr != nil {
			rows = append(rows, row{lr.rec, lr.sess})
		}
	}
	r.mu.Unlock()

	// Outside the lock: every live field below takes the session's own lock,
	// and holding the table's across them would let one busy conversation stall
	// a list of all the others.
	out := make([]RunView, 0, len(rows))
	for _, r := range rows {
		out = append(out, view(r.rec, r.sess))
	}
	return out
}

// Get reports one run.
func (r *Runs) Get(id string) (RunView, error) {
	rec, sess, err := r.row(id)
	if err != nil {
		return RunView{}, err
	}
	return view(rec, sess), nil
}

// Events returns the run's timeline after since, at most limit events.
//
// A live run replays from the session, which falls back to the journal for
// anything the ring no longer holds; a closed one reads the journal directly. A
// gap the journal cannot fill is uisession.ErrTruncated, which a client answers
// by reloading rather than by rendering a hole.
func (r *Runs) Events(id string, since uint64, limit int) ([]uisession.Event, error) {
	rec, sess, err := r.row(id)
	if err != nil {
		return nil, err
	}
	var evs []uisession.Event
	switch {
	case sess != nil:
		evs, err = sess.Local.Replay(since)
	case rec.SessionID != "":
		// The sess == nil arm, so there is no live id to prefer: the record is
		// what sessionID would return.
		evs, err = uisession.ReadJournal(rec.SessionID, since)
		if errors.Is(err, fs.ErrNotExist) {
			// The record outlived its journal - a state directory cleared, a
			// conversation that ended before its first turn was written. An
			// empty timeline is the truth about it.
			evs, err = nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(evs) > limit {
		evs = evs[:limit]
	}
	return evs, nil
}

// Blob returns the whole body of a tool result whose journalled form was
// trimmed, for the event at seq.
//
// It reads the journal's sidecar rather than the session, so a closed run and a
// run whose daemon has since restarted answer it too - which is the point: what
// a timeline shows for an oversized result is its head, and the whole of it is
// only ever on disk. An event that carries no stored body is ErrNoBlob, and a
// client that asked for one it was not promised gets the same answer as one
// that asked for a seq that never existed.
func (r *Runs) Blob(id string, seq uint64) (string, error) {
	rec, sess, err := r.row(id)
	if err != nil {
		return "", err
	}
	var m session.Meta
	if sess != nil && sess.Local != nil {
		m = sess.Local.Meta()
	}
	sid := sessionID(rec, m)
	if sid == "" {
		return "", ErrNoBlob
	}
	body, err := uisession.ReadBlob(sid, seq)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoBlob
	}
	if err != nil {
		return "", err
	}
	return body, nil
}

// Subscribe attaches a client to a live run and returns its event channel plus
// a function to detach. since is the last sequence the client already has.
func (r *Runs) Subscribe(id string, c uisession.Client, since uint64) (
	<-chan uisession.Event, func(), error,
) {
	_, sess, err := r.row(id)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, ErrRunClosed
	}
	return sess.Local.Subscribe(c, since)
}

// Artifacts reports the files the run changed, with the content before and
// after.
func (r *Runs) Artifacts(id string) (map[string]tools.FileChange, error) {
	_, sess, err := r.row(id)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		// The artifacts live with the session, not in the journal. A closed run
		// has none to report rather than an empty set to promise.
		return nil, ErrRunClosed
	}
	return sess.Local.Artifacts(), nil
}

// RunOp is one operation a client performs on a run. Which fields apply is
// determined by Op.
type RunOp struct {
	Op string
	// Text and Images are a submitted message.
	Text   string
	Images []llm.Image
	// ID and Decision answer an approval; By labels who answered, so the other
	// attached clients can show who decided instead of reporting a failure.
	ID       string
	Decision uisession.Decision
	By       string
	// Name and Args are a slash command and its argument line.
	Name string
	Args string
	// On is step mode: with it on, the person is asked about every tool call.
	// It is the inverse of the session's auto mode, and it is expressed this
	// way round because "step mode" is the toggle a person sees.
	On bool
	// Ref is a model reference for switch_model, and Persist saves it as the
	// preference for the next session too.
	Ref     string
	Persist bool
}

// The operations a run takes. They are named here rather than being an open
// string so that a front-end's typo is a refusal with a list, and so that the
// set a transport advertises is the set this file implements.
const (
	OpSubmit      = "submit"
	OpInterrupt   = "interrupt"
	OpResolve     = "resolve"
	OpCommand     = "command"
	OpStepMode    = "step_mode"
	OpSwitchModel = "switch_model"
)

// Apply carries out one operation on a live run.
//
// Everything it can refuse is a client mistake rather than a fault: an approval
// somebody else answered first, a command that does not exist, a model that
// cannot be opened. The caller reports them to the client that asked, and the
// conversation goes on.
func (r *Runs) Apply(id string, op RunOp) error {
	_, sess, err := r.row(id)
	if err != nil {
		return err
	}
	if sess == nil {
		return ErrRunClosed
	}
	l := sess.Local
	switch op.Op {
	case OpSubmit:
		if err := l.Submit(op.Text, op.Images); err != nil {
			return err
		}
		// The first message is where a conversation gets its id and its name.
		r.sync(id, sess)
		return nil
	case OpInterrupt:
		l.Interrupt()
		return nil
	case OpResolve:
		return l.Resolve(op.ID, op.Decision, op.By)
	case OpCommand:
		return l.Command(op.Name, op.Args)
	case OpStepMode:
		l.SetAutoMode(!op.On)
		return nil
	case OpSwitchModel:
		// Refused while a turn is in flight. The switch itself reads the
		// agent's messages to re-estimate the context window, and the turn
		// goroutine is writing them - a data race in internal/agent that this
		// is the daemon's reachable path to. It is also the answer that makes
		// sense on its own: the turn already started against the model being
		// replaced, so switching under it changes nothing about the answer
		// being produced.
		if l.Running() {
			return uisession.ErrBusy
		}
		info, err := l.SwitchModel(op.Ref, op.Persist)
		if err != nil {
			return err
		}
		r.setModel(id, info.Ref())
		return nil
	default:
		return fmt.Errorf("runner: unknown run operation %q", op.Op)
	}
}

// setModel records the model a run switched to. The switch has already
// happened, so a record that cannot be written is a stale row rather than a
// failed operation, and reporting it to the client that asked would say the
// switch did not happen.
func (r *Runs) setModel(id, ref string) {
	r.mu.Lock()
	lr := r.byID[id]
	if lr == nil || lr.rec.Model == ref {
		r.mu.Unlock()
		return
	}
	lr.rec.Model = ref
	lr.rec.Updated = r.now()
	r.saveLocked()
	rec, sess := lr.rec, lr.sess
	r.mu.Unlock()

	r.notify(view(rec, sess))
}

// CloseRun saves the conversation and ends the session, leaving the record and
// its journal behind: a run a person is done with is one they can still read.
//
// Closing a run that is already closed is not an error - two tabs pressing the
// same button is the ordinary case.
func (r *Runs) CloseRun(id string) error {
	r.mu.Lock()
	lr := r.byID[id]
	if lr == nil {
		r.mu.Unlock()
		return ErrNoRun
	}
	sess, rel := lr.sess, lr.release
	// Detached under the lock, so a second caller finds nothing to close rather
	// than racing this one into a double Close.
	lr.sess, lr.release = nil, nil
	r.mu.Unlock()

	if sess == nil {
		return nil
	}
	// Read with no lock of this registry's held. Meta takes the session's own
	// mutex, which a journal read or a journal write holds for as long as the
	// disk takes, and holding the table's across that would stall a list of
	// every other run behind one conversation.
	meta := sess.Local.Meta()

	r.mu.Lock()
	// What the session knows about itself is only true once it has had a turn:
	// the id names the journal, and the title is whatever the conversation
	// called itself. Without copying them back, a closed run is a record that
	// cannot be found again and has no name.
	if meta.ID != "" {
		lr.rec.SessionID = meta.ID
	}
	if meta.Title != "" {
		lr.rec.Title = meta.Title
	}
	lr.rec.Status = RunClosed
	lr.rec.Updated = r.now()
	r.saveLocked()
	rec := lr.rec
	r.mu.Unlock()

	// Announced with the session already detached, so what a page is told
	// matches what it would read back.
	r.notify(view(rec, nil))
	closeSession(sess, rel)
	return nil
}

// Close shuts the registry down: every live session is saved and closed, and
// the table records that none of them survived.
//
// Calling it twice is safe. It is what a daemon's shutdown calls, and it blocks
// until the sessions have finished unwinding, so a caller that returns from
// here can say the conversations are saved rather than that they were asked to
// save.
func (r *Runs) Close() { r.once.Do(r.shutdown) }

func (r *Runs) shutdown() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()

	// A Create that is past the check above is still building a session, and
	// will find the registry closed and close what it built. Waiting for it
	// here is what makes this function's promise true: without it that session
	// is saved and its SessionEnd hook run after the caller has gone on to tear
	// down the environment the hook needs - and after the process is free to
	// exit in the middle of the save.
	//
	// Bounded, because what it waits on is somebody else's code: Open dials a
	// provider and may refresh a credential over the network, and a Ctrl-C must
	// not be able to hang on a server that has stopped answering. Past the
	// bound the shutdown goes on and says so, which is the same trade
	// uisession.Close makes with a turn that will not unwind.
	if !waitFor(&r.opening, openWait) {
		slog.Warn("a run was still being opened when the daemon stopped waiting for it",
			"after", openWait)
	}

	r.mu.Lock()
	rows := make([]*liveRun, 0, len(r.order))
	for _, id := range r.order {
		if lr := r.byID[id]; lr != nil && lr.sess != nil {
			rows = append(rows, lr)
		}
	}
	// Detached under the lock, so a CloseRun racing this one finds nothing left
	// to close rather than closing the same session twice.
	sessions := make([]*Session, len(rows))
	releases := make([]func(), len(rows))
	for i, lr := range rows {
		sessions[i], releases[i] = lr.sess, lr.release
		lr.sess, lr.release = nil, nil
	}
	r.mu.Unlock()

	// Meta outside the lock, for the reason CloseRun gives.
	metas := make([]session.Meta, len(sessions))
	for i, s := range sessions {
		metas[i] = s.Local.Meta()
	}

	r.mu.Lock()
	for i, lr := range rows {
		if metas[i].ID != "" {
			lr.rec.SessionID = metas[i].ID
		}
		if metas[i].Title != "" {
			lr.rec.Title = metas[i].Title
		}
		lr.rec.Status = RunClosed
		lr.rec.Updated = r.now()
	}
	if len(rows) > 0 {
		r.saveLocked()
	}
	r.mu.Unlock()

	// Together rather than one after another. Closing one conversation runs its
	// SessionEnd hook and waits for a turn to unwind, each bounded at five
	// seconds; done in sequence, ten open runs is a Ctrl-C that appears to hang
	// for a minute and a half.
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			closeSession(sessions[i], releases[i])
		}()
	}
	wg.Wait()
}

// openWait bounds how long a shutdown waits for a run that is still being
// opened. It is a backstop rather than a duration anything is expected to take.
const openWait = 30 * time.Second

// waitFor waits on wg for at most d, and reports whether it finished. The
// goroutine outlives a timeout, which is what the bound is for: the process is
// on its way out and work that will not finish should not decide when it gets
// there.
func waitFor(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// liveLocked counts the conversations with a session attached.
func (r *Runs) liveLocked() int {
	n := 0
	for _, lr := range r.byID {
		if lr.sess != nil {
			n++
		}
	}
	return n
}

// sync copies back what a session only knows about itself once it has had a
// turn. It is called after a submit, so that a daemon killed without a chance
// to close its runs leaves records that can still be found and still have a
// name - which closing them is otherwise the only thing that does.
//
// It writes the table only when something actually changed, so the ordinary
// message costs a comparison.
func (r *Runs) sync(id string, sess *Session) {
	meta := sess.Local.Meta()
	r.mu.Lock()
	lr := r.byID[id]
	if lr == nil {
		r.mu.Unlock()
		return
	}
	changed := false
	if meta.ID != "" && lr.rec.SessionID != meta.ID {
		lr.rec.SessionID, changed = meta.ID, true
	}
	if meta.Title != "" && lr.rec.Title != meta.Title {
		lr.rec.Title, changed = meta.Title, true
	}
	if !changed {
		r.mu.Unlock()
		return
	}
	lr.rec.Updated = r.now()
	r.saveLocked()
	rec := lr.rec
	r.mu.Unlock()

	r.notify(view(rec, sess))
}

// row reads one record and its session under the lock, so that everything after
// it works on a consistent pair rather than on a row another goroutine is
// closing.
func (r *Runs) row(id string) (Run, *Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lr := r.byID[id]
	if lr == nil {
		return Run{}, nil, ErrNoRun
	}
	return lr.rec, lr.sess, nil
}

// saveLocked writes the table. It is called with the lock held, so the order
// the rows changed in is the order they reach the file: a save outside it could
// overwrite a newer table with an older one, and File.Save is last-write-wins.
//
// A table that cannot be written is logged and not returned. The run the person
// just started is live either way, and refusing it because the daemon could not
// take a note would lose the conversation to protect the record of it.
func (r *Runs) saveLocked() {
	if r.file == nil {
		return
	}
	table := make([]Run, 0, len(r.order))
	for _, id := range r.order {
		if lr := r.byID[id]; lr != nil {
			table = append(table, lr.rec)
		}
	}
	if err := r.file.Save(table); err != nil {
		slog.Error("the run table could not be written", "path", r.file.Path(), "err", err)
	}
}

// view is the record plus what the live session knows. It takes no lock of the
// registry's, so a caller may call it after releasing one.
func view(rec Run, sess *Session) RunView {
	v := RunView{Run: rec}
	if sess == nil || sess.Local == nil {
		return v
	}
	v.Live = true
	v.Running = sess.Local.Running()
	v.Step = !sess.Local.AutoMode()
	_, pending := sess.Local.Pending()
	v.Waiting = pending != nil
	v.Seq = sess.Local.Seq()
	// One snapshot, not two: a reset landing between them would give a view the
	// new conversation's id over the old one's name.
	m := sess.Local.Meta()
	v.SessionID = sessionID(rec, m)
	// A title a session has not been given is left alone rather than copied
	// over: the record's is what the run was opened with, and blanking it here
	// would lose it.
	if m.Title != "" {
		v.Title = m.Title
	}
	return v
}

// sessionID is which id the journal for this run is keyed by: the live
// session's where it has one, and the record's otherwise. It takes the meta the
// caller has already read, so a caller that needs the title too reads the
// session once.
//
// The two agree for the whole of an ordinary run - the record is stamped from
// the session at creation, and a session is given its id when it is built. They
// come apart only for a session that takes a new identity mid-run, which
// uisession.Reset and uisession.Load do and no daemon path reaches today. It is
// one function because a reader and a timeline disagreeing about which journal
// a run has is the kind of bug that only shows up in that one case.
func sessionID(rec Run, m session.Meta) string {
	if m.ID != "" {
		return m.ID
	}
	return rec.SessionID
}

// closeSession ends the conversation and releases whatever was allocated
// alongside it, in that order: Close runs the SessionEnd hook and waits for a
// turn to unwind, and the environment the hook runs in has to still be there.
//
// It saves after closing, never before. The session persists itself at the end
// of every turn, so the messages are already safe; what a turn does not write
// is metadata changed between turns, and switching model is exactly that - a
// conversation resumed in the terminal would come back on the model it was
// switched away from. Close cancels a running turn and waits for it, so by the
// time this save reads the agent nothing is writing it. Saving first, as this
// did, read those messages while the turn goroutine was still producing them.
func closeSession(sess *Session, rel func()) {
	if sess != nil && sess.Local != nil {
		sess.Local.Close()
		if err := sess.Local.Save(); err != nil {
			slog.Error("a run's conversation could not be saved", "err", err)
		}
	}
	release(rel)
}

// release calls what Opened handed back, if anything. It is a function rather
// than a nil check at each site because Release is documented as running once
// per run, and three of the four sites are error paths.
func release(rel func()) {
	if rel != nil {
		rel()
	}
}
