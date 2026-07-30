// Package agent runs the chat/tool loop against the model.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
)

const (
	// maxParallelTools caps how many tool calls from one assistant message run
	// concurrently (the model can batch independent calls, e.g. several task
	// delegations, to run them in parallel).
	maxParallelTools = 6
	// maxStopBlocks caps how many times a Stop hook may force more work in one
	// turn, so a misbehaving hook cannot loop the agent forever.
	maxStopBlocks = 3
	// maxGuardTrips bounds how many times a turn may re-attempt an identical
	// file edit that already failed before the turn is forced to a close. The
	// first failure runs normally; each later identical retry is refused (a trip);
	// after this many trips the model is thrashing, so we stop tool use entirely.
	maxGuardTrips = 2
)

// deterministicEditTools are the file-mutating tools whose result is a pure
// function of their arguments: an identical call that errored once cannot
// succeed on retry, so the repeat guard refuses it. bash is deliberately
// excluded - a command may fail transiently and legitimately be retried.
var deterministicEditTools = map[string]bool{"edit_file": true, "write_file": true}

// streamer is the subset of *llm.Client the agent needs (lets tests inject a fake).
type streamer interface {
	Stream(ctx context.Context, messages []llm.Message, tools []llm.Tool, temperature float64,
		onEvent func(llm.StreamEvent)) (llm.Message, error)
}

// ConfirmFunc asks the user to approve a tool call. Return false to deny.
type ConfirmFunc func(toolName string, args json.RawMessage) bool

// Events surfaces what happens during a Run so a UI can render it. Any callback
// may be nil.
type Events struct {
	OnContent   func(delta string)
	OnReasoning func(delta string)
	// OnAssistantMessage fires when an assistant message is complete and the loop
	// will run another step (it has tool calls, or the evaluator/Stop hook is
	// pushing it onward). The UI commits this intermediate text to the timeline
	// and clears its live preview, so a premature or mid-turn answer does not
	// linger as the streaming tail beneath later tool output.
	OnAssistantMessage func(content string)
	OnToolStart        func(name string, args json.RawMessage)
	OnToolEnd          func(name, result string, err error)
	OnNotice           func(text string)
	// OnBudgetExhausted fires when the turn is stopped by a runaway-protection
	// budget (model rounds, tool calls, wall clock). The turn still returns a
	// normal answer (a wrap-up when possible); the callback lets an unattended
	// front-end schedule a continuation instead of silently going idle.
	OnBudgetExhausted func(reason string)
	// OnUsage reports the estimated tokens currently held in context.
	OnUsage func(tokens int)
	// OnTodoUpdate reports the model's working plan after each todo_write call.
	OnTodoUpdate func(todos []TodoItem)
	// OnAgent* bracket a delegated subagent run; id is the parent task tool
	// call's ID, used to group the run's nested activity in the UI.
	OnAgentStart func(id, agent, prompt string)
	OnAgentEnd   func(id, result string, err error)
	// OnSub* surface a delegated subagent's nested tool activity, tagged with
	// the same parent-call id so concurrent runs stay grouped.
	OnSubToolStart func(id, agent, tool string, args json.RawMessage)
	OnSubToolEnd   func(id, agent, tool, result string, err error)
	OnSubNotice    func(id, agent, text string)
}

type Agent struct {
	client   streamer
	tools    *tools.Registry
	temp     float64
	confirm  ConfirmFunc
	messages []llm.Message
	callSeq  atomic.Uint64 // fallback group ids when a tool call has no ID

	hooks *hooks.Runner
	// subagentType, when set, marks this agent as a delegated subagent: it fires
	// SubagentStop (not Stop) and skips the user-turn-only events (reset,
	// UserPromptSubmit) that belong to the top-level agent.
	subagentType string

	mu sync.Mutex
	// running marks a turn in progress, and injected holds messages that arrived
	// during it. See Inject.
	running  bool
	injected []string
	// Turn-scoped tool policy set by an active skill (allowed-tools /
	// disallowed-tools); both are reset at the start of each user turn.
	approved   map[string]bool
	disallowed map[string]bool
	// Conditional skills (paths globs) surfaced once a matching file is touched.
	watch     []*skill.Skill
	activated map[string]bool
	// Skill-scoped hooks active for this turn, owned per agent so a subagent's
	// skill hooks never leak into a sibling or the parent. Reset each user turn.
	skillHooks map[string][]hooks.Matcher
	// todos is the model-maintained working plan, surfaced in the UI and used to
	// arm the autonomous evaluator. Replaced wholesale by the todo_write tool.
	todos []TodoItem
	// failedEdits maps the signature of a file-editing tool call that errored this
	// turn to its target path, so an identical retry (which cannot succeed - the
	// file is unchanged) is refused instead of re-run. The path lets a later
	// successful edit of the same file clear stale entries. A local model
	// otherwise re-emits the same broken edit_file forever; see runToolCall.
	// guardTrips counts how many such retries were refused this turn, used to
	// force the turn to a close.
	failedEdits map[string]string
	guardTrips  int

	// budget bounds one user turn when a front-end opts in (non-interactive/bot).
	budget TurnBudget

	// Compaction policy and state (all guarded by mu).
	compact    CompactConfig
	sessionID  string         // names pre-compaction backups
	compactSeq int            // backup counter within this session
	tokCache   map[string]int // accurate per-text token counts (content immutable)
}

// WatchSkills registers paths-conditional skills; when a tool touches a matching
// file, a one-time hint is appended to the tool result so the model can invoke
// the now-relevant skill.
func (a *Agent) WatchSkills(skills []*skill.Skill) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.watch = skills
}

// WatchedSkills returns the path-gated skills currently armed.
func (a *Agent) WatchedSkills() []*skill.Skill {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*skill.Skill(nil), a.watch...)
}

// SetHooks attaches a hook runner (nil disables hooks).
func (a *Agent) SetHooks(r *hooks.Runner) { a.hooks = r }

// SetTurnBudget configures per-turn runaway protection. A zero budget keeps the
// existing unbounded interactive behavior.
func (a *Agent) SetTurnBudget(b TurnBudget) { a.budget = b }

// fireHooks runs the event's hooks (if any) and surfaces any system message or
// non-blocking notices to the UI, returning the aggregated decision.
func (a *Agent) fireHooks(ctx context.Context, event string, in hooks.Input, ev Events) hooks.Decision {
	if a.hooks == nil {
		return hooks.Decision{Continue: true}
	}
	a.mu.Lock()
	scoped := a.skillHooks
	a.mu.Unlock()
	d := a.hooks.RunScoped(ctx, event, in, scoped)
	for _, n := range d.Notices {
		notice(ev, n)
	}
	if d.SystemMessage != "" {
		notice(ev, d.SystemMessage)
	}
	return d
}

type hooksKey struct{}

// WithHooks attaches the hook runner to ctx so delegated subagents inherit it.
func WithHooks(ctx context.Context, r *hooks.Runner) context.Context {
	return context.WithValue(ctx, hooksKey{}, r)
}

// HooksFrom returns the hook runner attached to ctx, or nil.
func HooksFrom(ctx context.Context) *hooks.Runner {
	r, _ := ctx.Value(hooksKey{}).(*hooks.Runner)
	return r
}

func New(client streamer, reg *tools.Registry, temp float64, confirm ConfirmFunc, systemPrompt string) *Agent {
	if systemPrompt == "" {
		systemPrompt = "You are aigem, a concise coding assistant running in a terminal."
	}
	return &Agent{
		client:     client,
		tools:      reg,
		temp:       temp,
		confirm:    confirm,
		approved:   map[string]bool{},
		disallowed: map[string]bool{},
		activated:  map[string]bool{},
		skillHooks: map[string][]hooks.Matcher{},
		messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt},
		},
	}
}

// AppendSystem appends text to the system prompt so a capability enabled
// mid-session (e.g. web search configured via /agents) is advertised to the
// model on the next turn. A no-op for empty text or a missing system message.
func (a *Agent) AppendSystem(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.messages) > 0 && a.messages[0].Role == llm.RoleSystem {
		a.messages[0].Content += "\n\n" + text
	}
}

// Messages returns a copy of the current conversation for persistence.
func (a *Agent) Messages() []llm.Message {
	out := make([]llm.Message, len(a.messages))
	copy(out, a.messages)
	return out
}

// SetMessages replaces the conversation, e.g. when resuming a saved session.
// The agent's current system prompt is kept, so a resumed session always runs
// on the live prompt rather than whatever was saved.
func (a *Agent) SetMessages(msgs []llm.Message) {
	var sys llm.Message
	if len(a.messages) > 0 && a.messages[0].Role == llm.RoleSystem {
		sys = a.messages[0]
	}
	if len(msgs) > 0 && msgs[0].Role == llm.RoleSystem {
		msgs = msgs[1:]
	}
	a.messages = append([]llm.Message{sys}, msgs...)
}

// SetSystem replaces the system prompt (messages[0]) in place, e.g. when /new
// rebuilds it from freshly re-read project instructions. Any other conversation
// messages are preserved; with none (the post-Reset state) it just swaps the prompt.
func (a *Agent) SetSystem(prompt string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.messages) > 0 && a.messages[0].Role == llm.RoleSystem {
		a.messages[0].Content = prompt
		return
	}
	a.messages = append([]llm.Message{{Role: llm.RoleSystem, Content: prompt}}, a.messages...)
}

// Reset clears the conversation back to just the system prompt and drops the
// working plan and per-session state, as if the agent had just started. The
// live system prompt (including mid-session additions like web-search guidance)
// is kept.
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	var sys llm.Message
	if len(a.messages) > 0 && a.messages[0].Role == llm.RoleSystem {
		sys = a.messages[0]
	}
	a.messages = []llm.Message{sys}
	a.todos = nil
	a.sessionID = ""
	a.compactSeq = 0
	a.tokCache = nil
}

// ContextTokens estimates how many tokens the conversation currently holds
// (roughly four characters per token).
func (a *Agent) ContextTokens() int {
	chars := 0
	images := 0
	for _, m := range a.messages {
		chars += len(m.Content) + len(m.Name)
		images += len(m.Images)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return chars/4 + images*ImageTokenEstimate
}

// Inject hands text to the turn currently running, as a user message the model
// reads before its next round. It reports whether a turn was there to take it,
// so a caller with no turn in flight can run the text as an ordinary one.
//
// This is what makes a running turn interruptible without the runtime having to
// guess what an interruption looks like: a message that arrives mid-turn is put
// in front of the model, which decides whether it stops the work, changes it, or
// means nothing - in whatever language it was written. The cost is that it lands
// at the next round boundary, so a long tool call delays it.
func (a *Agent) Inject(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		return false
	}
	a.injected = append(a.injected, text)
	return true
}

func (a *Agent) takeInjected() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.injected
	a.injected = nil
	return out
}

func (a *Agent) hasInjected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.injected) > 0
}

// Run processes one user turn, looping through tool calls until the model
// produces a final answer. It returns the final assistant text.
func (a *Agent) Run(ctx context.Context, userInput string, ev Events) (string, error) {
	return a.RunWithImages(ctx, userInput, nil, ev)
}

// RunWithImages processes one user turn with optional image attachments.
func (a *Agent) RunWithImages(ctx context.Context, userInput string, images []llm.Image, ev Events) (string, error) {
	budget := a.budget
	budgetCtx := false
	if budget.MaxDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget.MaxDuration)
		defer cancel()
		budgetCtx = true
	}
	// Anything injected but not yet answered stays queued for this turn rather
	// than being cleared: a message that arrived in the instant the previous turn
	// ended was accepted on the promise that the model would see it.
	a.mu.Lock()
	a.running = true
	leftover := len(a.injected)
	a.mu.Unlock()
	if leftover > 0 {
		notice(ev, "carrying a message that arrived as the last turn ended")
	}
	defer func() {
		a.mu.Lock()
		a.running = false
		stranded := len(a.injected)
		a.mu.Unlock()
		if stranded > 0 {
			notice(ev, "a message arrived as this turn ended; it will be read at the start of the next one")
		}
	}()
	a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: userInput, Images: images})
	defs := a.tools.Definitions()
	a.mu.Lock()
	a.approved, a.disallowed, a.activated = map[string]bool{}, map[string]bool{}, map[string]bool{}
	a.failedEdits, a.guardTrips = map[string]string{}, 0
	// Only the top-level agent resets skill hooks per turn; a subagent keeps the
	// hooks seeded for it (e.g. a forked skill's own hooks) for its single run.
	if a.subagentType == "" {
		a.skillHooks = map[string][]hooks.Matcher{}
	}
	a.mu.Unlock()
	// A new top-level turn starts fresh: a fully-completed plan from the previous
	// task is cleared so the sidebar does not carry it over.
	if a.subagentType == "" && a.clearTodosIfComplete() && ev.OnTodoUpdate != nil {
		ev.OnTodoUpdate(a.Todos())
	}
	// UserPromptSubmit fires only for the top-level agent (a real user prompt).
	if a.subagentType == "" && a.hooks != nil {
		dec := a.fireHooks(ctx, hooks.EventUserPromptSubmit, hooks.Input{Prompt: userInput}, ev)
		if !dec.Continue {
			return firstNonEmpty(dec.StopReason, dec.Reason), nil
		}
		if dec.Block {
			return dec.Reason, nil
		}
		if dec.Context != "" {
			a.messages[len(a.messages)-1].Content += "\n\n" + dec.Context
		}
	}
	a.reportUsage(ev)

	stopBlocks := 0
	autoContinues := 0
	lastPlanSig := "" // plan state at the last auto-continue, to detect no progress
	modelRounds, toolCalls := 0, 0
	repeatedTools := map[string]int{}
	for {
		if err := ctx.Err(); err != nil {
			if budgetCtx && err == context.DeadlineExceeded {
				return budgetExhausted(ev, "wall-clock turn timeout reached"), nil
			}
			return "", err
		}
		// Loop top is the only safe place to add a message: the previous round's
		// tool results are already appended, so tool_use/tool_result stay paired.
		for _, text := range a.takeInjected() {
			a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: text})
			notice(ev, "message delivered mid-turn")
		}
		if budget.MaxModelRounds > 0 && modelRounds >= budget.MaxModelRounds {
			// Loop top: the previous iteration's tool results are already appended, so the
			// tool_use/tool_result pairing is clean and we can ask for a wrap-up summary instead
			// of a bare "budget exhausted" line. The tool-call/repeat checks below cannot do this
			// (they fire before results are appended).
			return a.budgetStop(ctx, ev, fmt.Sprintf("model round budget reached (%d)", budget.MaxModelRounds)), nil
		}
		// A clean turn boundary: all pending tool results from the previous
		// iteration are appended, so compaction never splits a tool_use pair.
		a.maybeCompact(ctx, ev)
		modelRounds++
		assistant, err := a.call(ctx, defs, ev)
		if err != nil {
			if budgetCtx && ctx.Err() == context.DeadlineExceeded {
				return budgetExhausted(ev, "wall-clock turn timeout reached"), nil
			}
			return "", err
		}
		a.messages = append(a.messages, assistant)
		a.reportUsage(ev)

		if assistant.FinishReason == "repetition" {
			notice(ev, "detected repeating output - forcing an answer")
			return a.forceAnswer(ctx, ev)
		}
		if assistant.FinishReason == "length" {
			notice(ev, "response hit the token cap (see --max-tokens); it may be truncated")
		}

		if len(assistant.ToolCalls) == 0 {
			// A message delivered while this round was in flight must not be lost to
			// the turn ending: the caller was told it was taken, and it queues no
			// turn of its own. Keep looping so the model answers it.
			if a.hasInjected() {
				continue
			}
			// Autonomous evaluator: when an open plan remains and the model stopped
			// without asking the user, a controller pass decides whether to push it
			// to keep working. Only the top-level agent self-continues, and only up
			// to maxAutoContinue times per turn. The plan is re-evaluated only when
			// it changed since the last push - an unchanged plan means no progress,
			// so stop instead of paying for another classification.
			if a.subagentType == "" && autoContinues < maxAutoContinue && a.hasOpenPlan() {
				if sig := summarizePlan(a.Todos()); sig != lastPlanSig {
					intent, evalErr := a.evaluate(ctx, assistant.Content)
					if evalErr != nil {
						msg := "autonomous evaluator unavailable; stopping with open plan: " + evalErr.Error()
						notice(ev, msg)
						assistant.Content = appendStatusLine(assistant.Content, "Evaluator unavailable; stopping with open plan.")
						a.messages[len(a.messages)-1].Content = assistant.Content
					} else {
						switch intent {
						case intentContinue:
							autoContinues++
							lastPlanSig = sig
							stepDone(ev, assistant.Content)
							notice(ev, "plan has open steps - continuing")
							a.messages = append(a.messages,
								llm.Message{Role: llm.RoleUser, Content: a.continueNudge()})
							continue
						case intentDone:
							// The model reported the task done; reconcile the plan so the
							// sidebar does not show stale open steps it forgot to close.
							if a.completeOpenTodos() && ev.OnTodoUpdate != nil {
								ev.OnTodoUpdate(a.Todos())
							}
						}
					}
				}
			}
			event, in := hooks.EventStop, hooks.Input{}
			if a.subagentType != "" {
				event, in.AgentType = hooks.EventSubagentStop, a.subagentType
			}
			dec := a.fireHooks(ctx, event, in, ev)
			if dec.Block && dec.Reason != "" {
				if stopBlocks < maxStopBlocks {
					stopBlocks++
					stepDone(ev, assistant.Content)
					a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: dec.Reason})
					continue
				}
				notice(ev, fmt.Sprintf("%s hook still blocking after %d attempts - stopping anyway",
					event, maxStopBlocks))
			}
			return assistant.Content, nil
		}

		if budget.MaxToolCalls > 0 && toolCalls+len(assistant.ToolCalls) > budget.MaxToolCalls {
			return budgetExhausted(ev, fmt.Sprintf("tool-call budget exhausted (%d)", budget.MaxToolCalls)), nil
		}
		if budget.MaxRepeatedToolCalls > 0 {
			for _, tc := range assistant.ToolCalls {
				sig := tc.Function.Name + "\x00" + tc.Function.Arguments
				repeatedTools[sig]++
				if repeatedTools[sig] > budget.MaxRepeatedToolCalls {
					return budgetExhausted(ev, "repeated tool-call budget exhausted for "+tc.Function.Name), nil
				}
			}
		}
		toolCalls += len(assistant.ToolCalls)

		// Commit this step's streamed text to the timeline before its tool calls
		// render, so it sits above them (as a reloaded session would) instead of
		// lingering as the live tail beneath later tool output.
		stepDone(ev, assistant.Content)
		results := a.runToolCalls(ctx, assistant.ToolCalls, ev)
		for i, tc := range assistant.ToolCalls {
			a.messages = append(a.messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    results[i],
			})
		}
		a.reportUsage(ev)

		// The model is re-attempting identical edits that already failed and the
		// refusals are not redirecting it. Stop tool use and force a final answer
		// so the turn ends instead of looping to the context limit or timeout.
		a.mu.Lock()
		trips := a.guardTrips
		a.mu.Unlock()
		if trips >= maxGuardTrips {
			notice(ev, "repeated identical failing edits - forcing an answer")
			return a.forceAnswer(ctx, ev)
		}
	}
}

// call streams one assistant turn with the given tool definitions, relaying
// content and reasoning deltas to the UI.
func (a *Agent) call(ctx context.Context, defs []llm.Tool, ev Events) (llm.Message, error) {
	a.fitContext(ctx, ev)
	return a.client.Stream(ctx, a.messages, defs, a.temp, func(e llm.StreamEvent) {
		if e.Content != "" && ev.OnContent != nil {
			ev.OnContent(e.Content)
		}
		if e.Reasoning != "" && ev.OnReasoning != nil {
			ev.OnReasoning(e.Reasoning)
		}
	})
}

// forceAnswer asks the model for a final answer with tools disabled, so a loop
// or step-limit ends with text rather than an error.
func (a *Agent) forceAnswer(ctx context.Context, ev Events) (string, error) {
	a.messages = append(a.messages, llm.Message{
		Role:    llm.RoleUser,
		Content: "Stop calling tools. Using only what you have already gathered, give your best final answer now.",
	})
	assistant, err := a.call(ctx, nil, ev)
	if err != nil {
		return "", err
	}
	a.messages = append(a.messages, assistant)
	return assistant.Content, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func notice(ev Events, text string) {
	if ev.OnNotice != nil {
		ev.OnNotice(text)
	}
}

func budgetExhaustedMsg(reason string) string {
	return "Budget exhausted: " + reason + ". I stopped this turn before it could loop indefinitely."
}

func budgetExhausted(ev Events, reason string) string {
	notice(ev, "budget exhausted: "+reason+"; stopping this turn before it can loop indefinitely.")
	if ev.OnBudgetExhausted != nil {
		ev.OnBudgetExhausted(reason)
	}
	return budgetExhaustedMsg(reason)
}

// budgetStop ends a turn that hit a runaway-protection limit by asking the model for a short
// wrap-up - what it accomplished and the concrete next steps - so the stop is a usable checkpoint
// the work can resume from, not a bare "budget exhausted" line. Tools are disabled for this call so
// it cannot loop further. Falls back to the canned message if the wrap-up call fails or is empty
// (e.g. no time left, or a provider error); the wrap-up prompt is dropped in that case so history
// is not left with an unanswered instruction. Only safe to call at a clean turn boundary (no
// pending tool_use awaiting its result).
func (a *Agent) budgetStop(ctx context.Context, ev Events, reason string) string {
	notice(ev, "budget reached: "+reason+"; wrapping up this turn.")
	if ev.OnBudgetExhausted != nil {
		ev.OnBudgetExhausted(reason)
	}
	a.messages = append(a.messages, llm.Message{
		Role: llm.RoleUser,
		Content: "You have reached this turn's work limit (" + reason + "). Stop calling tools and " +
			"do not start new work. Using only what you already have, briefly summarize what you " +
			"changed or accomplished so far and the concrete next steps that remain, so the work " +
			"can resume cleanly.",
	})
	assistant, err := a.call(ctx, nil, ev)
	if err != nil || strings.TrimSpace(assistant.Content) == "" {
		a.messages = a.messages[:len(a.messages)-1] // drop the unanswered wrap-up prompt
		return budgetExhaustedMsg(reason)
	}
	a.messages = append(a.messages, assistant)
	return assistant.Content
}

func appendStatusLine(content, status string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return "[status: " + status + "]"
	}
	return content + "\n\n[status: " + status + "]"
}

// stepDone signals that the current assistant message is final for this step and
// the loop will continue, so the UI can commit its streamed text and reset the
// live preview before the next step's content or tool output arrives.
func stepDone(ev Events, content string) {
	if ev.OnAssistantMessage != nil {
		ev.OnAssistantMessage(content)
	}
}

func (a *Agent) reportUsage(ev Events) {
	if ev.OnUsage != nil {
		ev.OnUsage(a.ContextTokens())
	}
}

// evSink forwards a subagent's nested events to the parent Events callbacks,
// tagging each with the parent task call's id so the UI can group concurrent
// runs.
type evSink struct {
	ev     Events
	callID string
}

func (s evSink) AgentStart(agent, prompt string) {
	if s.ev.OnAgentStart != nil {
		s.ev.OnAgentStart(s.callID, agent, prompt)
	}
}

func (s evSink) AgentEnd(result string, err error) {
	if s.ev.OnAgentEnd != nil {
		s.ev.OnAgentEnd(s.callID, result, err)
	}
}

func (s evSink) SubToolStart(agent, tool string, args json.RawMessage) {
	if s.ev.OnSubToolStart != nil {
		s.ev.OnSubToolStart(s.callID, agent, tool, args)
	}
}

func (s evSink) SubToolEnd(agent, tool, result string, err error) {
	if s.ev.OnSubToolEnd != nil {
		s.ev.OnSubToolEnd(s.callID, agent, tool, result, err)
	}
}

func (s evSink) SubNotice(agent, text string) {
	if s.ev.OnSubNotice != nil {
		s.ev.OnSubNotice(s.callID, agent, text)
	}
}

// Activation lets an active skill adjust the turn's tool policy: Approve
// pre-approves confirm-gated tools (no prompt), Disallow blocks tools, AddHooks
// registers the skill's hooks. All reset on the next user turn.
type Activation interface {
	Approve(tools []string)
	Disallow(tools []string)
	AddHooks(cfg map[string][]hooks.Matcher)
}

type activationKey struct{}

// WithActivation attaches an Activation to ctx for the skill tool.
func WithActivation(ctx context.Context, a Activation) context.Context {
	return context.WithValue(ctx, activationKey{}, a)
}

// ActivationFrom returns the Activation attached to ctx, or nil.
func ActivationFrom(ctx context.Context) Activation {
	a, _ := ctx.Value(activationKey{}).(Activation)
	return a
}

type agentActivation struct{ a *Agent }

func (x agentActivation) Approve(toolNames []string) {
	x.a.mu.Lock()
	defer x.a.mu.Unlock()
	for _, t := range toolNames {
		x.a.approved[normalizeTool(t)] = true
	}
}

func (x agentActivation) Disallow(toolNames []string) {
	x.a.mu.Lock()
	defer x.a.mu.Unlock()
	for _, t := range toolNames {
		x.a.disallowed[normalizeTool(t)] = true
	}
}

// AddHooks merges a skill's hooks into this agent's turn-scoped set. It publishes
// a fresh map (copy-on-write) so a concurrent fireHooks reader is never racing a
// mutation of the live map.
func (x agentActivation) AddHooks(cfg map[string][]hooks.Matcher) {
	if len(cfg) == 0 {
		return
	}
	x.a.mu.Lock()
	defer x.a.mu.Unlock()
	merged := make(map[string][]hooks.Matcher, len(x.a.skillHooks)+len(cfg))
	for event, ms := range x.a.skillHooks {
		merged[event] = ms
	}
	for event, ms := range cfg {
		merged[event] = append(append([]hooks.Matcher{}, merged[event]...), ms...)
	}
	x.a.skillHooks = merged
}

// claudeToolNames maps Claude tool names (used in skill allowed-tools) to the
// closest aigem tool, so Claude-authored skills work unchanged.
var claudeToolNames = map[string]string{
	"read": "read_file", "write": "write_file", "edit": "edit_file",
	"bash": "bash", "grep": "grep", "glob": "fuzzy_find", "ls": "list_dir",
	"task": "task", "skill": "skill",
}

// normalizeTool lowercases a tool reference, drops any "(args)" pattern, and
// maps Claude names onto aigem's. aigem-native names pass through.
func normalizeTool(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if i := strings.IndexByte(name, '('); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	if mapped, ok := claudeToolNames[name]; ok {
		return mapped
	}
	return name
}

// runToolCalls executes a batch of tool calls from one assistant message. A
// single call runs inline; multiple run concurrently (bounded), since the model
// is expected to batch only independent calls. Results keep the input order.
func (a *Agent) runToolCalls(ctx context.Context, calls []llm.ToolCall, ev Events) []string {
	results := make([]string, len(calls))
	if len(calls) == 1 {
		results[0] = a.runToolCall(ctx, calls[0], ev)
		return results
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelTools)
	for i, tc := range calls {
		wg.Add(1)
		go func(i int, tc llm.ToolCall) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = a.runToolCall(ctx, tc, ev)
		}(i, tc)
	}
	wg.Wait()
	return results
}

func (a *Agent) runToolCall(ctx context.Context, tc llm.ToolCall, ev Events) string {
	name := tc.Function.Name
	rawArgs := json.RawMessage(tc.Function.Arguments)
	if name != TaskToolName && ev.OnToolStart != nil {
		ev.OnToolStart(name, rawArgs)
	}

	tool, ok := a.tools.Get(name)
	if !ok {
		err := fmt.Errorf("unknown tool %q", name)
		if ev.OnToolEnd != nil {
			ev.OnToolEnd(name, "", err)
		}
		return "error: " + err.Error()
	}

	a.mu.Lock()
	disallowed, preApproved := a.disallowed[name], a.approved[name]
	a.mu.Unlock()
	if disallowed {
		res := "error: tool " + name + " is disallowed by the active skill"
		if ev.OnToolEnd != nil {
			ev.OnToolEnd(name, res, nil)
		}
		return res
	}

	// Repeat guard: a file edit identical to one that already failed this turn
	// cannot succeed (the file is unchanged), so refuse it instead of re-running.
	// This breaks the local model's habit of re-emitting the same broken
	// edit_file forever. The guidance steers it to re-copy old_string exactly or
	// fall back to write_file. editPath lets a later successful edit to the same
	// file clear these records, since changing the file can make a prior failure
	// valid again.
	var editSig, editPath string
	if deterministicEditTools[name] {
		editSig = name + "\x00" + string(rawArgs)
		var pa struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(rawArgs, &pa)
		editPath = pa.Path
		a.mu.Lock()
		_, failed := a.failedEdits[editSig]
		if failed {
			a.guardTrips++
		}
		a.mu.Unlock()
		if failed {
			res := "error: this exact " + name + " call already failed above with the " +
				"same arguments; re-running it cannot succeed. Re-read the file and copy " +
				"old_string EXACTLY as it appears (every space, tab, and newline must match), " +
				"or use write_file with the file's full new contents instead."
			if ev.OnToolEnd != nil {
				ev.OnToolEnd(name, res, nil)
			}
			return res
		}
	}

	pre := a.fireHooks(ctx, hooks.EventPreToolUse, hooks.Input{ToolName: name, ToolInput: rawArgs}, ev)
	if pre.Block {
		res := "error: tool call blocked by a PreToolUse hook: " + pre.Reason
		if ev.OnToolEnd != nil {
			ev.OnToolEnd(name, res, nil)
		}
		return res
	}
	if len(pre.UpdatedInput) > 0 {
		rawArgs = pre.UpdatedInput
	}
	needConfirm := tool.NeedsConfirm() && !preApproved
	if pre.Allow {
		needConfirm = false
	}
	if pre.Ask {
		needConfirm = true
	}
	if needConfirm && a.confirm != nil && !a.confirm(name, rawArgs) {
		const denied = "error: user denied this tool call"
		if ev.OnToolEnd != nil {
			ev.OnToolEnd(name, denied, nil)
		}
		return denied
	}

	groupID := tc.ID
	if groupID == "" {
		groupID = "call-" + strconv.FormatUint(a.callSeq.Add(1), 10)
	}
	ctx = WithSink(ctx, evSink{ev: ev, callID: groupID})
	ctx = WithActivation(ctx, agentActivation{a})
	ctx = WithHooks(ctx, a.hooks)
	result, err := tool.Run(ctx, rawArgs)
	if deterministicEditTools[name] {
		a.mu.Lock()
		if err != nil {
			a.failedEdits[editSig] = editPath
		} else if editPath != "" {
			// A successful edit changed this file, so earlier failures against the
			// same path may now match. Drop them so a legitimate retry is not
			// refused (the guard only exists to stop futile identical repeats).
			for sig, p := range a.failedEdits {
				if p == editPath {
					delete(a.failedEdits, sig)
				}
			}
		}
		a.mu.Unlock()
	}
	if err != nil {
		result = "error: " + err.Error()
	}
	if pre.Context != "" {
		result += "\n\n" + pre.Context
	}
	if err == nil {
		if hint := a.pathActivation(rawArgs); hint != "" {
			result += "\n\n" + hint
			notice(ev, hint)
		}
	}
	// PostToolUse fires after the tool runs, on success or error, so hooks can
	// react to failures too (tool_response carries the error text).
	post := a.fireHooks(ctx, hooks.EventPostToolUse,
		hooks.Input{ToolName: name, ToolInput: rawArgs, ToolResponse: result}, ev)
	if post.UpdatedOutput != nil {
		result = *post.UpdatedOutput
	}
	if post.Context != "" {
		result += "\n\n" + post.Context
	}
	if post.Block && post.Reason != "" {
		result += "\n\n[PostToolUse hook]: " + post.Reason
	}
	result = clipToolResult(result)
	if name == TodoToolName && ev.OnTodoUpdate != nil {
		ev.OnTodoUpdate(a.Todos())
	}
	if name != TaskToolName && ev.OnToolEnd != nil {
		ev.OnToolEnd(name, result, err)
	}
	return result
}

// maxToolResultBytes bounds any single tool result spliced into the
// conversation. A tool can otherwise return far more than the context window
// holds - an unbounded bash dump, a grep over generated files - producing a
// request the model server rejects outright. Oversized results are truncated
// with a marker; the model can re-run more narrowly or read a specific file.
const maxToolResultBytes = 48 * 1024

func clipToolResult(s string) string {
	if len(s) <= maxToolResultBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxToolResultBytes], "") +
		fmt.Sprintf("\n... [tool output truncated at %d KB - narrow the command or read a specific file]",
			maxToolResultBytes/1024)
}

// pathActivation surfaces any paths-conditional skill the tool's path argument
// matches (once per turn), returning a hint to append to the tool result.
func (a *Agent) pathActivation(rawArgs json.RawMessage) string {
	var x struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(rawArgs, &x) != nil || x.Path == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var hit []string
	for _, s := range a.watch {
		if !a.activated[s.Name] && s.Matches(x.Path) {
			a.activated[s.Name] = true
			hit = append(hit, s.Name)
		}
	}
	if len(hit) == 0 {
		return ""
	}
	return "[skill now relevant for this path: " + strings.Join(hit, ", ") +
		". Invoke it with the skill tool if it applies.]"
}
