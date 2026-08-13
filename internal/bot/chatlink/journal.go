package chatlink

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/chat"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/uisession"
)

// usageFlushEvery bounds how far a turn's recorded spend lags what it has
// actually spent.
//
// The sink fires on the goroutine that made the model call, with the provider's
// response still open, so a write there is paid for by a connection that is not
// yet back in the pool - and it queues behind the single writer the whole fleet
// shares. Per call it bought nothing: AddUsage publishes no frame, so no reader
// is watching the number climb. Batching keeps the write off that path without
// letting a long turn's row sit at zero for the whole run.
//
// The cost is that a process killed mid-turn loses up to this many calls of
// spend rather than at most one. That turn is stamped "interrupted by a
// restart" at the next startup, so its number was never one to bill against.
const usageFlushEvery = 16

// TurnEvents opens a turn and returns the sinks that record its steps and its
// spend.
//
// This is what puts a bot's work in front of the operator. Until now those
// steps reached only the log: the model's answer was the whole of what a human
// could see, which is all Mattermost was ever able to show.
//
// The turn row exists for the whole run, so "amiran is working" is a fact
// anyone can read rather than a ping that keeps having to be re-sent - and one
// that would still be claiming to be true some seconds after the process died.
func (t *Transport) TurnEvents(thread bot.ThreadID, _ string) (agent.Events, bot.UsageSink,
	func(string, error)) {
	ctx := context.Background()
	turn, err := t.store.BeginTurn(ctx, string(thread), t.self)
	if err != nil {
		// A turn whose timeline cannot be recorded still runs. Losing the trace
		// is worse than nothing, but far better than refusing to work.
		t.logger().Warn("could not open a turn", "thread", thread, "err", err)
		return agent.Events{}, nil, func(string, error) {}
	}
	t.openTurn(string(thread), turn)
	emit := func(ev uisession.Event) { t.record(ctx, string(thread), turn, ev) }
	// The turn's spend, accumulated between flushes. Parallel subagents run on
	// the same client, so several calls finish at once.
	var (
		mu      sync.Mutex
		pending chat.Usage
		model   string
	)
	flush := func() {
		mu.Lock()
		u, m := pending, model
		pending = chat.Usage{}
		mu.Unlock()
		if u.IsZero() {
			return
		}
		if err := t.store.AddUsage(ctx, t.self, turn, u, m); err != nil {
			t.logger().Warn("could not record what the turn cost", "thread", thread, "err", err)
		}
	}
	spend := func(u llm.Usage, m string) {
		call := chat.Usage{
			InputTokens:  u.InputTokens,
			CachedTokens: u.CachedTokens,
			OutputTokens: u.OutputTokens,
			Calls:        1,
		}
		if u.IsZero() {
			call.Uncounted = 1
		}
		mu.Lock()
		pending = pending.Add(call)
		if m != "" {
			model = m
		}
		due := pending.Calls >= usageFlushEvery
		mu.Unlock()
		if due {
			flush()
		}
	}
	done := func(answer string, runErr error) {
		flush()
		// The bracket goes first, and has to: it carries the turn's final answer,
		// which reaches the timeline nowhere else, and the counters it moves are
		// only writable while the turn is open.
		//
		// Which means a reader cannot treat this event as "the row is now
		// closed" - EndTurn is a second transaction behind the fleet's single
		// writer, and a refetch racing it reads a turn that is still running.
		// The signal for that is the thread frame EndTurn publishes from inside
		// its own transaction, and that is what the browser keys on.
		emit(uisession.Event{Kind: uisession.KindTurnEnd, Text: answer})
		msg := ""
		if runErr != nil {
			msg = runErr.Error()
		}
		if err := t.store.EndTurn(ctx, t.self, turn, msg); err != nil {
			t.logger().Warn("could not close a turn", "thread", thread, "err", err)
		}
		// Last: the answer is posted before this runs, and it is stamped with the
		// turn it came out of.
		t.closeTurn(string(thread), turn)
	}
	emit(uisession.Event{Kind: uisession.KindTurnStart})
	return uisession.Bridge(emit), spend, done
}

// FileChanged records a file the bot wrote during a turn: the diff for the
// thread panel, and a step in the timeline so the trace says where in the run
// it happened.
//
// A change made outside any turn - a scheduled job with no thread - has nothing
// to file under and is dropped. It reached the log, as it did before.
func (t *Transport) FileChanged(thread bot.ThreadID, a chat.Artifact) {
	turn := t.turnOf(string(thread))
	if turn == 0 {
		return
	}
	// Cleaned here so the row and the timeline step carry the same string. The
	// store cleans its own copy either way, and two spellings of one path is how
	// the panel and the summary line come to disagree about how many files there
	// were.
	a.Path = chat.ReadablePath(a.Path)
	ctx := context.Background()
	stored, err := t.store.PutArtifact(ctx, t.self, string(thread), turn, a)
	if err != nil {
		t.logger().Warn("could not record a changed file", "path", a.Path, "err", err)
		return
	}
	// Only for a file the store actually kept. A step announcing a diff the
	// panel has no row for is what makes the two counts disagree: the summary
	// line counts these events, and it would climb past the cap the list stops
	// at.
	if !stored {
		return
	}
	t.record(ctx, string(thread), turn,
		uisession.Event{Kind: uisession.KindFileChanged, Path: a.Path, Created: a.Created})
}

// record writes one event of a turn to the thread's timeline.
func (t *Transport) record(ctx context.Context, thread string, turn uint64, ev uisession.Event) {
	if skipInTimeline(ev.Kind) {
		return
	}
	payload, blob := split(ev)
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	step, tool := contributes(ev)
	var plan json.RawMessage
	if ev.Kind == uisession.KindTodo {
		// Stored on the turn so the plan panel does not have to replay a
		// timeline to draw six lines. Marshalled here rather than in the store,
		// which deliberately never looks inside a payload.
		//
		// A plan too large for the column is dropped and the step is kept: the
		// store refuses the whole record if the plan is oversized, and losing a
		// timeline step over an unreasonable todo list is the wrong trade.
		if p, err := json.Marshal(ev.Todos); err == nil && len(p) <= chat.MaxEventBytes {
			plan = p
		}
	}
	if _, err := t.store.AppendEvent(ctx, chat.EventRecord{
		Thread: thread, Actor: t.self, TurnSeq: turn,
		Kind: string(ev.Kind), Payload: body, Blob: blob,
		Step: step, Tool: tool, Plan: plan,
	}); err != nil {
		t.logger().Warn("could not record a turn step", "kind", ev.Kind, "err", err)
	}
}

// planTool is the tool that writes the working plan. It is named here for the
// same reason the browser names it: its calls are rendered as a plan, not as
// tool cards, so counting them as tools promises the reader a card that the
// timeline deliberately does not draw.
const planTool = "todo_write"

// contributes says what an event adds to a turn's summary line.
//
// The store is told rather than asked to work it out: which kinds are steps is
// the event vocabulary's business, and internal/chat is deliberately built to
// know nothing about it.
//
// A step is an event that becomes a row when the trace is expanded, and the
// list is a whitelist rather than "everything except the brackets". Counting by
// exception put `usage` (twice per model round), `tool_batch` (once) and the
// ends that merge into the row their start opened into the total - so an eight
// round turn drawing fourteen rows announced itself as forty-five steps. A
// summary the reader can disprove by opening it is worse than none.
//
// A nested subagent call is a step and a tool: a run the operator can watch
// working is work, whoever did it. The plan write is a step but not a tool -
// it happened and it is shown, but it is shown in the plan panel, and "6 tools"
// over a trace holding five cards is the same small lie in the other column.
func contributes(ev uisession.Event) (step, tool bool) {
	switch ev.Kind {
	case uisession.KindToolStart, uisession.KindSubToolStart:
		return true, ev.Name != planTool
	case uisession.KindAssistantMessage:
		// Only when it says something. The agent fires this before every tool
		// batch, and on a round that produced tool calls and no prose the text
		// is empty and the renderer draws nothing - so counting it announced
		// two steps per round for a turn that took one.
		return strings.TrimSpace(ev.Text) != "", false
	case uisession.KindUserMessage, uisession.KindAgentStart, uisession.KindNotice,
		uisession.KindError, uisession.KindBudgetExhausted:
		return true, false
	default:
		// turn_start and turn_end - the run's own brackets, and the answer the
		// second carries is drawn as the message this trace hangs under, not
		// twice; the *_end events that complete the row their start opened;
		// sub_notice, which the renderer drops; and usage, tool_batch, todo and
		// file_changed, which the panels read and which draw no row here.
		return false, false
	}
}

// skipInTimeline drops the per-delta events.
//
// uisession.Bridge fires OnContent and OnReasoning once per streamed chunk, and
// its journal is a buffered file that can take that. Here every event is a
// transaction on the single writer the whole fleet queues behind, plus a
// thread frame and a fan-out to every subscriber - so one turn's streaming
// would stall every other bot's message.
//
// Nothing is lost that a reader needs: the assistant's finished text arrives as
// assistant_message, and the answer arrives again on turn_end. What the browser
// gives up is the token-by-token caret, which a turn nobody is watching live
// does not need.
func skipInTimeline(kind uisession.Kind) bool {
	return kind == uisession.KindContent || kind == uisession.KindReasoning
}

// split prepares an event for storage: an oversized tool result is replaced by
// its head and the whole body is stored beside it, to be fetched when someone
// expands the call.
//
// The threshold is the store's, and the policy is the same one uisession
// applies to a session's own journal. It is duplicated rather than shared
// because deciding what may be trimmed needs to know what the event means -
// which field is the result and which are the render flags - and that knowledge
// belongs with whoever produces the event, not with the store.
func split(ev uisession.Event) (uisession.Event, []byte) {
	if ev.Kind != uisession.KindToolEnd && ev.Kind != uisession.KindSubToolEnd {
		return ev, nil
	}
	if len(ev.Text) <= chat.BlobThreshold {
		return ev, nil
	}
	blob := []byte(ev.Text)
	ev.Bytes = len(ev.Text)
	ev.Blob = true
	ev.Text = ev.Text[:chat.BlobThreshold]
	return ev, blob
}
