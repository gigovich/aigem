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
	"strings"
	"time"
	"unicode/utf8"
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
// see Store.recomputeState.
const (
	// StateNeedsYou means a bot asked the operator for something and is waiting.
	// This is the only state the UI spends its accent on.
	StateNeedsYou = "needs_you"
	// StateWorking means a turn is running in this thread right now.
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
// existence of conversations it is not allowed to see.
var ErrNoSuchThread = errors.New("chat: no such thread")

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

// Say describes a message being written. AwaitReply marks the thread as needing
// the operator, which is what puts it at the top of the inbox with the accent
// marker. It is explicit rather than inferred: guessing from a trailing question
// mark is how a bot ends up shouting for attention it does not need.
type Say struct {
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

// Usage is what a turn spent, accumulated across its model calls. Uncounted is
// how many calls the provider reported no numbers for, so a total can never
// quietly understate the spend.
type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	CachedTokens int `json:"cached_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	Calls        int `json:"calls,omitempty"`
	Uncounted    int `json:"uncounted,omitempty"`
}

// IsZero reports whether nothing was recorded, so a turn that never reached the
// provider renders as no cost rather than as a row of zeroes.
func (u Usage) IsZero() bool { return u == Usage{} }

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
	Seq    uint64      `json:"seq"`
	Stream string      `json:"stream"`
	Thread string      `json:"thread,omitempty"`
	Msg    *Message    `json:"msg,omitempty"`
	Thr    *ThreadView `json:"thr,omitempty"`
	// Event carries a uisession.Event as the JSON the browser already decodes.
	// It is raw here so this package does not depend on uisession.
	Event []byte `json:"event,omitempty"`
	// From is the resume point on a desync frame.
	From uint64 `json:"from,omitempty"`
}

// Frame streams.
const (
	StreamMessage = "message"
	StreamThread  = "thread"
	StreamEvent   = "event"
	StreamDesync  = "desync"
)

// MarshalJSON keeps a Frame's Event as the JSON it already is. Without it the
// []byte would be base64-encoded, and the browser would have to decode a string
// to find the event it was sent.
func (f Frame) MarshalJSON() ([]byte, error) {
	type alias Frame
	return json.Marshal(struct {
		alias
		Event json.RawMessage `json:"event,omitempty"`
	}{alias: alias(f), Event: json.RawMessage(f.Event)})
}

// UnmarshalJSON is the inverse, so a Frame survives a round trip through the
// wire in tests and in any Go client.
func (f *Frame) UnmarshalJSON(b []byte) error {
	type alias Frame
	var raw struct {
		alias
		Event json.RawMessage `json:"event,omitempty"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*f = Frame(raw.alias)
	f.Event = raw.Event
	return nil
}

// preview bounds the denormalised tail kept on a thread row.
const previewChars = 240

// preview trims a body to what an inbox row can show. It cuts on a rune
// boundary, because a preview ending in half a character is a bug you only see
// in the one language that has them.
func preview(body string) string {
	body = strings.TrimSpace(body)
	if len(body) <= previewChars {
		return body
	}
	cut := body[:previewChars]
	// Drop a rune the cut landed inside. A preview ending in half a character is
	// a bug you only ever see in the one language that has them.
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size != 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut)
}
