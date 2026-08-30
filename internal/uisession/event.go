// Package uisession holds the session logic that every interactive aigem
// front-end needs: it runs turns, owns the approval queue and the session tool
// policy, and publishes one ordered event stream that a terminal or a browser
// renders however it likes.
//
// The split is deliberate. A front-end decides how to ask and how to draw;
// nothing else. Everything that decides what happens - whether a tool runs,
// what a turn does, what is persisted - lives here, so a second front-end is a
// renderer rather than a fork of the first one's logic.
package uisession

import (
	"encoding/json"
	"time"

	"github.com/gigovich/aigem/internal/agent"
)

// Kind tags an Event. The values are part of the wire format a remote
// front-end decodes, so they are strings rather than an iota.
type Kind string

const (
	KindUserMessage      Kind = "user_message"
	KindTurnStart        Kind = "turn_start"
	KindTurnEnd          Kind = "turn_end"
	KindContent          Kind = "content"
	KindReasoning        Kind = "reasoning"
	KindAssistantMessage Kind = "assistant_message"
	KindToolBatch        Kind = "tool_batch"
	KindToolStart        Kind = "tool_start"
	KindToolEnd          Kind = "tool_end"
	KindAgentStart       Kind = "agent_start"
	KindAgentEnd         Kind = "agent_end"
	KindSubToolStart     Kind = "sub_tool_start"
	KindSubToolEnd       Kind = "sub_tool_end"
	KindSubNotice        Kind = "sub_notice"
	KindNotice           Kind = "notice"
	KindError            Kind = "error"
	KindUsage            Kind = "usage"
	KindTodo             Kind = "todo"
	KindBudgetExhausted  Kind = "budget_exhausted"
	KindFileChanged      Kind = "file_changed"
	KindApprovalRequest  Kind = "approval_request"
	KindApprovalResolved Kind = "approval_resolved"
	KindSessionMeta      Kind = "session_meta"
	KindPresence         Kind = "presence"
	KindDesync           Kind = "desync"
)

// Call is one tool call inside a batch.
type Call struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Event is one step in a session, in the order it happened. It is a flat struct
// with omitted empties rather than a per-kind union: the same value is written
// to the journal, handed to an in-process front-end, and decoded on the far
// side of a websocket, and a flat struct is the shape that costs nothing in all
// three. Which fields apply is determined by Kind.
type Event struct {
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"`
	Kind Kind      `json:"kind"`

	// ID identifies what the event is about: the tool call for a tool event, the
	// delegated run for an agent event, the request for an approval event. On a
	// nested (sub_*) event it is the nested call's own id, which is unique only
	// within its run - see RunID.
	ID string `json:"id,omitempty"`
	// RunID is the parent task call's id, set on every event belonging to a
	// delegated run so concurrent subagents stay apart.
	RunID string `json:"run_id,omitempty"`
	Agent string `json:"agent,omitempty"`
	Name  string `json:"name,omitempty"`

	// Text is the event's prose: a streamed delta, a notice, an answer, a
	// prompt, a tool result, or the reason a budget stopped the turn.
	Text  string          `json:"text,omitempty"`
	Args  json.RawMessage `json:"args,omitempty"`
	Error string          `json:"error,omitempty"`

	// Bytes is the full size of a tool result whose stored form was trimmed. It
	// is absent on the live event, which carries the result whole.
	Bytes int `json:"bytes,omitempty"`

	// Ctx is the active model's context window, carried on session_meta so a
	// front-end tracks it from the stream instead of asking for it.
	Ctx int `json:"ctx,omitempty"`

	Round  int              `json:"round,omitempty"`
	Calls  []Call           `json:"calls,omitempty"`
	Tokens int              `json:"tokens,omitempty"`
	Todos  []agent.TodoItem `json:"todos,omitempty"`

	// Images counts the images that came with a user message. The bytes are not
	// journalled; a front-end that needs them fetches them separately.
	Images int `json:"images,omitempty"`
	// Injected marks a user message that arrived mid-turn and was appended to
	// the running turn instead of starting a new one.
	Injected bool `json:"injected,omitempty"`
	// Interrupted marks a turn that ended because the user cancelled it, which
	// is not an error and should not be rendered as one.
	Interrupted bool `json:"interrupted,omitempty"`

	Path    string `json:"path,omitempty"`
	Created bool   `json:"created,omitempty"`

	Approval *Approval `json:"approval,omitempty"`
	Decision Decision  `json:"decision,omitempty"`
	// By labels the client that answered an approval, so the others can show who
	// decided instead of reporting a failure.
	By string `json:"by,omitempty"`

	Clients []Client `json:"clients,omitempty"`

	// From is the last seq a dropped subscriber is known to have received.
	// Reconnecting with it recovers everything missed.
	From uint64 `json:"from,omitempty"`
}

// Client describes one attached front-end, for presence.
type Client struct {
	ID    string `json:"id"`
	Kind  string `json:"kind,omitempty"`  // "tui", "web", ...
	Label string `json:"label,omitempty"` // free text, e.g. a device name
}
