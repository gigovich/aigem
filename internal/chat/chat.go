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
