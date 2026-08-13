// Package chat is the conversation store the bot fleet and its browser UI share.
//
// A thread is one task or one conversation with an explicit set of
// participants: the operator and one or more bots. There are no channels and no
// rooms, so participation is the whole authorization boundary - a bot reads and
// writes exactly the threads it is in, and there is no second authority to
// disagree with about that.
//
// The package knows nothing about bots, agents or models. It stores what was
// said, what an agent did while saying it, and who was there.
package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Limits on what one message may carry. Attachments were always bounded; text,
// mentions and the attachment count were not, and every one of them is written
// inside the transaction that holds the single writer.
const (
	MaxBodyBytes      = 256 << 10
	MaxMentions       = 32
	MaxAttachments    = 8
	MaxTitleChars     = 200
	MaxEventBytes     = 64 << 10
	MaxEventBlobBytes = 4 << 20
	// MaxArtifactBytes bounds one side of a stored diff. A file past it is
	// recorded as a path with no content rather than dropped: that a turn wrote
	// a 40MB generated file is the useful half of the fact, and the diff of one
	// is not something a browser was going to render.
	//
	// 128KiB rather than something generous, because this is the product of two
	// limits: both sides of MaxTurnArtifacts files, all of it written through the
	// single writer and held for the retention window.
	MaxArtifactBytes = 128 << 10
	// MaxTurnArtifacts bounds how many distinct files one turn records. A turn
	// that rewrites a whole tree is real, and every path it touched would
	// otherwise be a row and two file bodies on the single writer.
	MaxTurnArtifacts = 200
	// MaxPathChars bounds a recorded path. Far above any real one: it is a
	// backstop against a pathological name, not a display width.
	MaxPathChars = 1024
)

// Actor kinds. An actor id is "<kind>:<name>", so it is readable in a log and
// sortable by kind without a join.
const (
	KindHuman = "human"
	KindBot   = "bot"
)

// Operator is the single human. Multi-user is deliberately out of scope; the
// actors table has the shape to grow more later, and nothing else assumes it.
const Operator = "human:operator"

// Thread states. They are derived from what is in the store, never guessed:
// the resting ones by Store.restingState, and the live one by the effectiveState
// expression the queries share.
const (
	// StateNeedsYou means a bot asked the operator for something and is waiting.
	// This is the only state the UI spends its accent on.
	StateNeedsYou = "needs_you"
	// StateWorking means a turn is running in this thread right now. It is never
	// stored - see liveTurn - only computed at read time.
	StateWorking = "working"
	// StateWaiting means the operator spoke last and no bot has answered yet.
	StateWaiting = "waiting"
	// StateIdle means nothing is outstanding.
	StateIdle = "idle"
)

// Message kinds.
const (
	MsgMessage = "message"
	// MsgSystem records something the store did rather than someone said: a
	// participant added or removed. It is in the transcript because a
	// conversation whose membership changed silently cannot be audited.
	MsgSystem = "system"
	// MsgHandoff is one bot formally passing work to another.
	MsgHandoff = "handoff"
)

// ErrNoSuchThread is returned when a thread id names nothing - and also when it
// names a thread the caller is not a participant in. The two are deliberately
// indistinguishable: a bot that could tell them apart could probe for the
// existence of conversations it is not allowed to see. For the same reason it
// carries no thread id.
var ErrNoSuchThread = errors.New("chat: no such thread")

// ErrInvalid wraps every refusal that is the caller's fault rather than a
// missing thing. It exists so the HTTP layer's whole error mapping is two
// lines - ErrInvalid is a 400, ErrNoSuchThread is a 404 - instead of a string
// match against a growing list of messages.
var ErrInvalid = errors.New("chat: invalid request")

// ErrNoSuchTurn is returned when a turn id names nothing the caller may write
// to. Losing a turn's spend accounting silently is worse than an error.
var ErrNoSuchTurn = errors.New("chat: no such turn")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Actor is a participant identity: the operator, or a bot in the fleet.
type Actor struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Name    string    `json:"name"`
	Role    string    `json:"role,omitempty"`
	Present bool      `json:"present"`
	Created time.Time `json:"created"`
}

// Fleet states, as the roster reports them. They are decided here rather than
// by each renderer, for the same reason thread states are: two clients drawing
// one screen from the same response must not each derive the word, and a state
// added here would otherwise be invisible in one of them until its bundle was
// rebuilt.
const (
	// FleetWorking is a turn with no end - the same fact the inbox reads.
	FleetWorking = "working"
	// FleetIdle is up with nothing open.
	FleetIdle = "idle"
	// FleetStopped is configured and not running: either the daemon says so, or
	// no daemon claims it and the store's own presence flag agrees.
	FleetStopped = "stopped"
)

// FleetMember is one identity on the fleet screen: what the store knows about
// it, plus what only the process running the bots can say.
type FleetMember struct {
	Actor
	// State is what this bot is doing, in one word: see the constants above. It
	// is empty for an actor that is not a bot, because "what is the operator
	// doing" is not a question this roster answers.
	State string `json:"state,omitempty"`
	// Threads is how many unarchived threads this actor takes part in.
	Threads int `json:"threads"`
	// Working is true while this actor has a turn open anywhere. It is read
	// from the turns table, exactly as the inbox reads its own working flag, so
	// the roster and the inbox cannot disagree about what a bot is doing.
	Working bool `json:"working"`
	// Live is what the daemon running the fleet knows and the store cannot.
	//
	// It is absent rather than zeroed when nobody reported one: a daemon
	// serving this store without running the bots - and every actor that is not
	// a bot - knows none of this, and a row of confident zeroes claiming a
	// stopped bot with no heartbeat is worse than an empty one.
	Live *LiveBot `json:"live,omitempty"`
}

// LiveBot is a bot's operational state inside the process running the fleet.
type LiveBot struct {
	// Running is false for a configured bot that could not be started and is
	// being retried - the state an operator would otherwise have to read
	// journalctl to discover.
	Running bool   `json:"running"`
	Model   string `json:"model,omitempty"`
	// Heartbeat is the wake-up interval currently in force ("30m"), and Tier is
	// how far the idle backoff has walked from the working cadence.
	Heartbeat string `json:"heartbeat,omitempty"`
	Tier      int    `json:"tier"`
	// NextJob is the scheduled job due soonest, and NextRun when. A bot with
	// nothing scheduled reports neither rather than a zero time.
	NextJob string     `json:"next_job,omitempty"`
	NextRun *time.Time `json:"next_run,omitempty"`
}

// BotActor builds the actor id for a bot name. Ids are built in one place, and
// carry their kind, so a bot named "operator" cannot collide with the human.
func BotActor(name string) string { return KindBot + ":" + strings.TrimSpace(name) }

// ActorName splits an actor id back into its kind and name.
func ActorName(id string) (kind, name string) {
	k, n, ok := strings.Cut(id, ":")
	if !ok {
		return "", id
	}
	return k, n
}

// Thread is a conversation. LastText is a preview, not the message: the inbox
// draws a line per thread and must not fetch a transcript to do it.
type Thread struct {
	ID         string    `json:"id"`
	Title      string    `json:"title,omitempty"`
	Created    time.Time `json:"created"`
	CreatedBy  string    `json:"created_by"`
	LastSeq    uint64    `json:"last_seq"`
	LastAt     time.Time `json:"last_at"`
	LastAuthor string    `json:"last_author,omitempty"`
	LastText   string    `json:"last_text,omitempty"`
	State      string    `json:"state"`
	Archived   bool      `json:"archived,omitempty"`
}

// ThreadView is a thread as the inbox needs it: the thread plus the three
// things a row shows that are not on the thread itself.
type ThreadView struct {
	Thread
	Participants []string `json:"participants"`
	Unread       int      `json:"unread"`
	// Working is true while some bot has an open turn here. It is read from the
	// turns table rather than from a transient ping, so it survives a reload and
	// is still right after a crash the moment the fleet restarts.
	Working bool `json:"working"`
}

// Message is one thing someone said.
type Message struct {
	Seq      uint64    `json:"seq"`
	Thread   string    `json:"thread"`
	Author   string    `json:"author"`
	Body     string    `json:"body"`
	Kind     string    `json:"kind"`
	Mentions []string  `json:"mentions,omitempty"`
	Await    bool      `json:"await,omitempty"`
	Created  time.Time `json:"created"`
	// Attachments are ids, resolved through the attachment routes.
	Attachments []string `json:"attachments,omitempty"`
	// Turn is the run that produced this message, or zero for one said outside
	// any run - which is every message the operator writes.
	//
	// It is recorded rather than inferred from the sequence. Messages and events
	// share one cursor, so a reader could bracket a turn between its start and
	// its end, except that a bot may post several times inside one turn and a
	// turn killed with the process never wrote an end at all.
	Turn uint64 `json:"turn,omitempty"`
}

// Draft describes a message being written. AwaitReply marks the thread as
// needing the operator, which is what puts it at the top of the inbox with the
// accent marker. It is explicit rather than inferred: guessing from a trailing
// question mark is how a bot ends up shouting for attention it does not need.
//
// Kind may be MsgMessage or MsgHandoff. MsgSystem is reserved for the store.
type Draft struct {
	Author      string
	Body        string
	Kind        string
	Mentions    []string
	AwaitReply  bool
	Attachments []string
	// TurnSeq files this message under the run that produced it. It must be a
	// turn the author owns in this thread; zero means the message was not said
	// inside one.
	TurnSeq uint64
}

// Turn is one bot's run inside a thread, and what it cost. A turn row exists
// from the moment work starts, so "amiran is working" is a fact in the store
// rather than a ping that lies after a crash.
type Turn struct {
	Seq     uint64    `json:"seq"`
	Thread  string    `json:"thread"`
	Actor   string    `json:"actor"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`
	Usage   Usage     `json:"usage,omitzero"`
	Model   string    `json:"model,omitempty"`
	Error   string    `json:"error,omitempty"`

	// What the turn did, counted as it happened. The collapsed summary line is
	// drawn for every bot message on screen, and deriving it from the timeline
	// would mean shipping a thread's whole history to render a hundred one-line
	// summaries.
	Steps int `json:"steps,omitempty"`
	Tools int `json:"tools,omitempty"`
	Files int `json:"files,omitempty"`
	// Plan is the working plan as this turn last left it, as the JSON the
	// browser already decodes. Empty on a turn that never wrote one; the panel
	// shows the newest turn that did.
	Plan json.RawMessage `json:"plan,omitempty"`
}

// Artifact is one file a turn changed. Old is the content as it stood before
// the turn first touched it, so a turn that edited a file five times still
// shows one diff of its whole effect.
//
// Old and New are carried only when a path is asked for by name: the list is
// opened far more often than any one diff is read, and a turn that rewrote a
// large tree would otherwise ship all of it to draw a filename.
type Artifact struct {
	Turn    uint64 `json:"turn"`
	Path    string `json:"path"`
	Created bool   `json:"created"`
	// Truncated says the content was too large to keep, so the path is all there
	// is. Said out loud rather than served as an empty diff, which reads as a
	// file that was emptied.
	Truncated bool      `json:"truncated,omitempty"`
	Old       string    `json:"old,omitempty"`
	New       string    `json:"new,omitempty"`
	Changed   time.Time `json:"changed"`
}

// Usage is what a turn spent, accumulated across its model calls.
//
// Calls counts every call, and Uncounted says how many of those the provider
// reported no numbers for - so a total can never quietly understate the spend
// by dropping them. llm.UsageReport, which the bot log carries, excludes them
// from its own Calls instead, so the two surfaces disagree by design.
type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	CachedTokens int `json:"cached_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	Calls        int `json:"calls,omitempty"`
	Uncounted    int `json:"uncounted,omitempty"`
}

// IsZero is what the `omitzero` tag on Turn.Usage calls: a turn that never
// reached the provider renders as no cost rather than as a row of zeroes. It
// looks unused, and is not.
func (u Usage) IsZero() bool { return u == Usage{} }

// Add sums two usages field by field, so that the places which total usage
// have one summation between them rather than a copy each.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + o.InputTokens,
		CachedTokens: u.CachedTokens + o.CachedTokens,
		OutputTokens: u.OutputTokens + o.OutputTokens,
		Calls:        u.Calls + o.Calls,
		Uncounted:    u.Uncounted + o.Uncounted,
	}
}

// Page is a bounded slice of a stream, plus where to resume it.
//
// It exists so that a route which truncates has to say so. A bare JSON array
// that stopped at its limit is indistinguishable from the end of the
// conversation, and a reader built on one loses history without ever being
// told there was any. Cursor is what to pass back as the paging parameter -
// before for messages, since for a timeline - and is only set when More is.
type Page[T any] struct {
	Items  []T    `json:"items"`
	Cursor uint64 `json:"cursor,omitempty"`
	More   bool   `json:"more,omitempty"`
}

// Attachment is a file on a message.
type Attachment struct {
	ID       string    `json:"id"`
	Thread   string    `json:"thread"`
	Message  uint64    `json:"message,omitempty"`
	Filename string    `json:"filename"`
	Mime     string    `json:"mime"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	Created  time.Time `json:"created"`
}

// Frame is one element of the ordered stream every reader follows. Messages,
// timeline events and thread changes share a single sequence so a client that
// reconnects with one number sees everything that happened, in the order it
// happened.
type Frame struct {
	Seq    uint64 `json:"seq"`
	Stream string `json:"stream"`
	// ThreadID is set on every frame. A thread frame with no Thread on it is a
	// tombstone: the conversation is gone and there is nothing left to describe.
	ThreadID string      `json:"thread,omitempty"`
	Message  *Message    `json:"msg,omitempty"`
	Thread   *ThreadView `json:"thr,omitempty"`
	// Event carries a uisession.Event as the JSON the browser already decodes.
	// It is raw here so this package does not depend on uisession, and it is
	// json.RawMessage rather than []byte so the type itself states that the
	// bytes are already-encoded JSON - and so it survives a round trip without
	// being base64'd into a string the browser would have to decode.
	Event json.RawMessage `json:"event,omitempty"`
	// Turn is the run an event frame belongs to. It is on the frame rather than
	// inside the payload because the payload is a uisession.Event, which knows
	// nothing about threads or runs - and without it a live step cannot be filed
	// under the collapsed trace it belongs to, only under "the latest one",
	// which is wrong the moment two bots work in a thread at once.
	Turn uint64 `json:"turn,omitempty"`
	// From is the resume point on a desync frame.
	From uint64 `json:"from,omitempty"`
	// To is who may see this frame: the thread's participants at the moment it
	// was written. Entitlement is decided here, inside the transaction that
	// already has the participants in hand, rather than by the hub asking the
	// database once per frame per subscriber on a path that holds the writer.
	//
	// It never goes on the wire. It is routing, not content.
	To []string `json:"-"`
}

// visibleTo reports whether an actor may see this frame.
func (f Frame) visibleTo(actor string) bool {
	return slices.Contains(f.To, actor)
}

// Frame streams.
const (
	StreamMessage = "message"
	StreamThread  = "thread"
	StreamEvent   = "event"
	// StreamDesync means the reader fell behind and frames were dropped: set
	// your cursor to From and reconnect.
	StreamDesync = "desync"
	// StreamTruncated means the reader is still in order, but its backlog did
	// not fit in one page: ask for the rest from From. It is a separate stream
	// from desync because the two call for opposite reactions, and a client
	// that confused them would throw away the backlog it had just been given.
	StreamTruncated = "truncated"
)

// previewChars bounds the denormalised tail kept on a thread row.
const previewChars = 240

// preview trims a body to what an inbox row can show.
//
// It counts runes, not bytes. Bounding by bytes would show a Georgian or
// Japanese row a third of what an English one gets, for no reason the reader
// could see - and it would cut inside a character, which is a bug you only ever
// meet in the languages that have them.
func preview(body string) string {
	body = strings.TrimSpace(body)
	// Ranging a string yields the byte offset of each rune start, so n counts
	// runes while i stays the offset to cut at.
	n := 0
	for i := range body {
		if n == previewChars {
			return strings.TrimSpace(body[:i])
		}
		n++
	}
	return body
}
