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
)

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
// SessionID is the id the journal is keyed by, and it is empty until the
// session's first turn - a conversation opened and walked away from is not
// worth a file. A record that never got one has no timeline to read back.
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
// is a preference: Open resolves the model and the root, and reports what it
// actually built in RunOpen.
type RunRequest struct {
	// Mode is the session policy. The zero value is interactive.
	Mode Mode
	// Title names the conversation before its first turn does.
	Title string
	// Model is the reference to open, in the "provider/id" form the wire uses.
	// Empty takes the daemon's default.
	Model string
	// Root is the directory the run works in. Empty takes the daemon's.
	Root string
}

// Opened is what OpenRun built. It is reported rather than assumed, because
// the model a run got is the one the registry could open and not the one the
// request named.
type Opened struct {
	Model string
	Root  string
	Title string
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
	// which is what a daemon that could not find its state directory falls back
	// to: it can still hold a conversation, it just forgets it happened.
	Store *store.File[[]Run]
	// Now is the clock, so a test can pin the timestamps it asserts on.
	Now func() time.Time
}

// Runs is the daemon's table of conversations.
type Runs struct {
	open OpenRun
	now  func() time.Time

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
	next   int
	closed bool
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
	r := &Runs{open: cfg.Open, now: now, file: cfg.Store, byID: map[string]*liveRun{}}
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
	r.next++
	id := runIDPrefix + strconv.Itoa(r.next)
	r.mu.Unlock()

	sess, opened, err := r.open(ctx, req)
	if err != nil {
		return RunView{}, err
	}
	if sess == nil || sess.Local == nil {
		release(opened.Release)
		return RunView{}, errors.New("runner: the run was opened without a session")
	}

	now := r.now()
	rec := Run{
		ID: id, SessionID: sess.Local.Meta().ID, Mode: req.Mode,
		Title: opened.Title, Model: opened.Model, Root: opened.Root,
		Status: RunOpen, Created: now, Updated: now,
	}

	r.mu.Lock()
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
	r.saveLocked()
	r.mu.Unlock()

	return view(rec, sess), nil
}

// List reports every run, oldest first.
func (r *Runs) List() []RunView {
	r.mu.Lock()
	rows := make([]struct {
		rec  Run
		sess *Session
	}, 0, len(r.order))
	for _, id := range r.order {
		if lr := r.byID[id]; lr != nil {
			rows = append(rows, struct {
				rec  Run
				sess *Session
			}{lr.rec, lr.sess})
		}
	}
	r.mu.Unlock()

	// Outside the lock: every live field below takes the session's own lock,
	// and holding the table's across them would let one busy conversation stall
	// a list of all the others.
	out := make([]RunView, 0, len(rows))
	for _, row := range rows {
		out = append(out, view(row.rec, row.sess))
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
		return l.Submit(op.Text, op.Images)
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
	defer r.mu.Unlock()
	lr := r.byID[id]
	if lr == nil || lr.rec.Model == ref {
		return
	}
	lr.rec.Model = ref
	lr.rec.Updated = r.now()
	r.saveLocked()
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
	if sess != nil {
		// The id is only known once the session has had a turn, so it is read
		// here rather than at creation: without it the journal cannot be found
		// again after a restart.
		if sid := sess.Local.Meta().ID; sid != "" {
			lr.rec.SessionID = sid
		}
		lr.rec.Status = RunClosed
		lr.rec.Updated = r.now()
		r.saveLocked()
	}
	r.mu.Unlock()

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
func (r *Runs) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	var sessions []*Session
	var releases []func()
	changed := false
	for _, id := range r.order {
		lr := r.byID[id]
		if lr == nil || lr.sess == nil {
			continue
		}
		if sid := lr.sess.Local.Meta().ID; sid != "" {
			lr.rec.SessionID = sid
		}
		lr.rec.Status = RunClosed
		lr.rec.Updated = r.now()
		changed = true
		sessions = append(sessions, lr.sess)
		releases = append(releases, lr.release)
		// Detached under the lock, so a CloseRun racing this one finds nothing
		// left to close rather than closing the same session twice.
		lr.sess, lr.release = nil, nil
	}
	if changed {
		r.saveLocked()
	}
	r.mu.Unlock()

	for i := range sessions {
		closeSession(sessions[i], releases[i])
	}
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
	// A session names itself on its first turn, so the record written at
	// creation is behind until then. A title it has not been given is left
	// alone rather than copied over: the record's is what the run was opened
	// with, and blanking it here would lose it.
	m := sess.Local.Meta()
	if m.ID != "" {
		v.SessionID = m.ID
	}
	if m.Title != "" {
		v.Title = m.Title
	}
	return v
}

// closeSession saves the conversation, ends it, and releases whatever was
// allocated alongside it - in that order, because Close runs the SessionEnd
// hook and waits for a turn to unwind, and the environment the hook runs in has
// to still be there.
func closeSession(sess *Session, rel func()) {
	if sess != nil && sess.Local != nil {
		if err := sess.Local.Save(); err != nil {
			slog.Error("a run's conversation could not be saved", "err", err)
		}
		sess.Local.Close()
	}
	release(rel)
}

func release(rel func()) {
	if rel != nil {
		rel()
	}
}
