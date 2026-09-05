package web

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Backend is the agent side of the daemon, as the web package sees it.
//
// It is declared here and implemented in cmd/aigem rather than the other way
// round, so that internal/web never imports internal/runner - and with it
// uisession, tools, mcp, hooks, skill and llm. What the router serves is then
// testable against a fake, with no model, no MCP server and no agent behind it.
//
// A method here returns transport types this package can marshal and never a
// live object, and it names a model or a run the way the wire names it, because
// resolving that name is the backend's job and not the router's.
type Backend interface {
	// Meta describes the daemon to a signed-in page. It is the daemon's own
	// state rather than the request's, and it may change while the daemon runs,
	// so a page reads it again after anything that could have moved it.
	Meta(ctx context.Context) (Meta, error)

	// Runs reports every conversation this daemon knows about, oldest first,
	// including the ones whose sessions did not survive a restart.
	Runs(ctx context.Context) ([]Run, error)
	// OpenRun starts a conversation and returns the record for it.
	OpenRun(ctx context.Context, req NewRun) (Run, error)
	// Run reports one conversation, or ErrNoRun.
	Run(ctx context.Context, id string) (Run, error)
	// CloseRun saves a conversation and ends its session, leaving the record
	// and the timeline behind. Closing one that is already closed is not an
	// error: two tabs pressing the same button is the ordinary case.
	CloseRun(ctx context.Context, id string) error
	// RunEvents returns the timeline after since, at most limit events, oldest
	// first. A gap that can no longer be filled is ErrHistoryGone.
	RunEvents(ctx context.Context, id string, since uint64, limit int) ([]RunEvent, error)
	// WatchRun attaches a client to a live run, resuming after since. The
	// caller owns the stream and closes it.
	WatchRun(ctx context.Context, id string, c RunClient, since uint64) (RunStream, error)
	// RunArtifacts reports the files the run changed.
	RunArtifacts(ctx context.Context, id string) ([]Artifact, error)
	// ApplyRunOp carries out one client operation. A refusal the client should
	// read is a *Refusal.
	ApplyRunOp(ctx context.Context, id string, op RunOp) error
}

// Meta is what a page needs to know about the daemon it is talking to. The UI
// state is deliberately absent: which assets this Server carries is the
// Server's own answer, and asking the backend for it would let a daemon built
// without a UI claim to have one.
type Meta struct {
	// Version is the running binary's version, as `aigem version` prints it.
	Version string `json:"version"`
	// DefaultModel is the model reference a new session gets when none is
	// named, in the "provider/id" form the wire uses. Empty when nothing is
	// authenticated, which is a state the page has to be able to show.
	DefaultModel string `json:"defaultModel"`
}

// ErrNoBackend is returned by New when Config carries no Backend. A daemon
// without one could still serve the page, and the page would then fail one
// request at a time against a router with nothing behind it; refusing at
// construction reports the wiring mistake where it was made.
var ErrNoBackend = errors.New("web: no backend was given to serve the API with")

// A run is one conversation: the same session a terminal opens, reached over
// HTTP. The methods below are the whole of what the router does to one, and
// each takes and returns transport shapes - a run is a struct of strings here,
// never a live session, and an event crosses as the bytes that go on the wire.
//
// Errors carry meaning: the sentinels below decide the status code, and a
// *Refusal is a message meant for the person at the browser. Anything else is
// the daemon's own fault and is answered generically.

// ErrNoRun is returned for an id no run answers to.
var ErrNoRun = errors.New("web: no such run")

// ErrRunClosed is returned for an operation that needs a run's live session
// when it has none - it was closed, or it belongs to a daemon that has since
// restarted. Its timeline is still readable.
var ErrRunClosed = errors.New("web: this run has no live session")

// ErrHistoryGone is returned when a client asks to resume from a point the run
// no longer holds. It is not an error the client can retry: it reloads.
var ErrHistoryGone = errors.New("web: the run's history no longer reaches that point")

// ErrBusy is returned when the daemon is already holding as much as it will.
//
// It is the one refusal on this API that a client should retry rather than
// correct: nothing about the request is wrong, and closing something makes room.
// It is answered the way the socket cap already answers - a 503 with a
// Retry-After - so a page has one shape for "not now" rather than two.
var ErrBusy = errors.New("web: the daemon is at capacity")

// Refusal is an error whose text is meant to be shown.
//
// It is how a backend says "this is the client's mistake, and the reason is
// worth reading" - an approval somebody else answered first, a command that
// does not exist, a model that cannot be opened. Every other error is the
// daemon's own fault, and the router answers those without a detail, because a
// message assembled somewhere inside the agent is not a sentence anyone at a
// browser can act on.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

// Refuse builds a Refusal from an error, for a backend turning one it has
// classified as the client's.
func Refuse(err error) error {
	if err == nil {
		return nil
	}
	return &Refusal{Reason: err.Error()}
}

// Run is one conversation as the wire describes it. The first block is what
// outlives the daemon; the second is what only a live session knows, and is
// absent from a closed run rather than stale.
type Run struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId,omitempty"`
	Mode      string    `json:"mode"`
	Title     string    `json:"title,omitempty"`
	Model     string    `json:"model,omitempty"`
	Root      string    `json:"root,omitempty"`
	Status    string    `json:"status"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`

	// Live is whether a session is attached. A page reads this rather than the
	// status string before it opens a socket.
	Live bool `json:"live"`
	// Running is whether a turn is in flight, and Waiting whether it is parked
	// on an approval nobody has answered.
	Running bool `json:"running,omitempty"`
	Waiting bool `json:"waiting,omitempty"`
	// Step is whether the person is asked about every tool call, which is the
	// state the front-end's step-mode toggle draws.
	Step bool `json:"step,omitempty"`
	// Seq is the last event sequence the run has emitted, so a list tells a
	// client whether it is caught up without opening the stream.
	Seq uint64 `json:"seq,omitempty"`
}

// NewRun is what a client asked for when it created a run. Everything in it is
// a preference the backend resolves: the run reports what it actually got.
type NewRun struct {
	// Mode is the session policy. Empty means interactive, which is the only
	// one this phase opens.
	Mode string `json:"mode,omitempty"`
	// Title names the conversation before its first turn does.
	Title string `json:"title,omitempty"`
	// Model is a reference in the "provider/id" form the wire uses; empty takes
	// the daemon's default.
	Model string `json:"model,omitempty"`
	// There is deliberately no Root here. A run works in the directory the
	// daemon was started in, and the Root a record reports is that directory.
	// Letting a request name one would be the API's first way to reach outside
	// what the operator pointed the daemon at, and it would have to arrive with
	// the project model that decides which directories are allowed - which is
	// phase 2. A field that is on the wire, documented, and ignored is a worse
	// answer than no field.
}

// RunEvent is one step of a run's timeline on its way to a client: the event as
// the session encoded it, and nothing else.
//
// This package neither decodes it nor describes it. The event vocabulary
// belongs to the session, a second copy of it here would be a second thing to
// keep in step, and the transport's whole job is to deliver the bytes in order.
// The sequence a client resumes from is inside those bytes, where the session
// wrote it; carrying it alongside as well would be a second copy of that too,
// and one nothing here reads.
type RunEvent = json.RawMessage

// RunClient identifies an attached front-end, for the presence the other
// clients of the same run are shown.
type RunClient struct {
	Kind  string
	Label string
}

// RunStream is one client's attachment to a live run.
//
// Events is closed when the run ends; Close detaches and may be called more
// than once, which is what lets a handler defer it and still close early.
type RunStream interface {
	Events() <-chan RunEvent
	Close()
}

// RunOp is one operation a client performs on a run. Which fields apply is
// determined by Op, and the router validates the name before the backend sees
// it: see runOps.
type RunOp struct {
	Op string `json:"op"`
	// Text and Images are a submitted message. An image is base64 with the
	// media type, as the model APIs take them.
	Text   string  `json:"text,omitempty"`
	Images []Image `json:"images,omitempty"`
	// ID and Decision answer an approval; Label says who answered, so the other
	// clients can show who decided instead of reporting a failure.
	ID       string `json:"id,omitempty"`
	Decision string `json:"decision,omitempty"`
	Label    string `json:"label,omitempty"`
	// Name and Args are a slash command and its argument line.
	Name string `json:"name,omitempty"`
	Args string `json:"args,omitempty"`
	// On is step mode: with it on, the person is asked about every tool call.
	On bool `json:"on,omitempty"`
	// Ref is the model to switch to, and Persist saves it as the preference for
	// the next session too.
	Ref     string `json:"ref,omitempty"`
	Persist bool   `json:"persist,omitempty"`
}

// Image is a picture attached to a message, in the shape the model APIs take.
type Image struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// Artifact is one file a run changed, with the content on both sides so the
// page can render the diff without reading the working tree itself.
//
// The content is not promised. A run that appended a line to a very large file
// holds both versions of it, and a route that serialised every one of them
// would be several copies of an unbounded amount of memory per request. Past
// the backend's budget the sides are left out and Truncated says so, with the
// real sizes still reported - a page can then say how big the change is and
// offer to fetch it, rather than being handed a diff it cannot draw.
type Artifact struct {
	Path    string `json:"path"`
	Old     string `json:"old,omitempty"`
	New     string `json:"new,omitempty"`
	Created bool   `json:"created,omitempty"`
	// OldBytes and NewBytes are the true sizes of the two sides, whether or not
	// the content came with them.
	OldBytes int `json:"oldBytes,omitempty"`
	NewBytes int `json:"newBytes,omitempty"`
	// Truncated marks an entry whose content was left out.
	Truncated bool `json:"truncated,omitempty"`
}
