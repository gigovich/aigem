package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
)

// SubagentDef declares a specialized agent: its own system prompt and the
// subset of tools it may use. Definitions are data, so new agents can be added
// in code or loaded from disk without touching the loop.
type SubagentDef struct {
	Name        string
	Description string
	Prompt      string
	Tools       []string
}

// SubagentRegistry holds the available subagent definitions.
type SubagentRegistry struct {
	defs  map[string]SubagentDef
	order []string
}

func NewSubagentRegistry() *SubagentRegistry {
	return &SubagentRegistry{defs: map[string]SubagentDef{}}
}

// Add registers (or replaces) a definition.
func (r *SubagentRegistry) Add(d SubagentDef) {
	if _, ok := r.defs[d.Name]; !ok {
		r.order = append(r.order, d.Name)
	}
	r.defs[d.Name] = d
}

func (r *SubagentRegistry) Get(name string) (SubagentDef, bool) {
	d, ok := r.defs[name]
	return d, ok
}

// List returns the definitions in registration order.
func (r *SubagentRegistry) List() []SubagentDef {
	out := make([]SubagentDef, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.defs[n])
	}
	return out
}

// Names returns the registered subagent names in order.
func (r *SubagentRegistry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Common tool sets for built-in agents.
var (
	readOnlyTools = []string{"read_file", "list_dir", "grep", "fuzzy_find"}
	editTools     = []string{"read_file", "write_file", "edit_file", "list_dir", "bash", "grep", "fuzzy_find"}
)

// DefaultSubagents returns the four built-in agents.
func DefaultSubagents() *SubagentRegistry {
	r := NewSubagentRegistry()
	r.Add(SubagentDef{
		Name:        "scout",
		Description: "Fast read-only codebase recon. Use to locate code and report findings; cannot edit.",
		Tools:       readOnlyTools,
		Prompt: `You are scout, a fast read-only reconnaissance agent. Explore the codebase with
read_file, list_dir, grep, and fuzzy_find to answer the request. You CANNOT modify files or run
commands. Be thorough but concise: report concrete findings with path:line references and a short
summary. Do not speculate about code you have not read.`,
	})
	r.Add(SubagentDef{
		Name:        "code-writer",
		Description: "Implement a focused change from a clear instruction or plan, then verify it.",
		Tools:       editTools,
		Prompt: `You are code-writer, an implementation agent. You are given a concrete, self-contained
task. Read the relevant code first, then make the smallest correct change that satisfies it, matching
the existing naming, structure, and idioms. Run the language's formatter, linter, and tests with bash
and read the output - do not assume success. Report what you changed (path:line) and what you verified.
Do not expand scope beyond the task.`,
	})
	r.Add(SubagentDef{
		Name:        "simplifier",
		Description: "Simplify or refactor existing code for clarity while preserving behavior.",
		Tools:       editTools,
		Prompt: `You are simplifier. Improve the clarity, consistency, and maintainability of the code in
scope WITHOUT changing its behavior. Prefer small, safe edits: remove dead code, reduce duplication,
clarify names, and follow the project's idioms. Never weaken tests or change public behavior. After
editing, run the formatter and tests with bash to prove nothing broke. Report the simplifications made.`,
	})
	r.Add(SubagentDef{
		Name:        "reviewer",
		Description: "Independent review for correctness, quality, and security; reports issues, no edits.",
		Tools:       append([]string{"bash"}, readOnlyTools...),
		Prompt: `You are reviewer, an independent code reviewer. Examine the code in scope for correctness,
logic errors, edge cases, security issues, and adherence to the project's conventions. You may read
files and run read-only checks (tests, linters) with bash, but you must NOT modify any files. Report
concrete issues ranked by severity, each with a path:line reference and a suggested fix. If the code is
sound, say so plainly.`,
	})
	return r
}

// LoadSubagentsInto reads *.md agent definitions from dir and adds them to r,
// overriding built-ins with the same name. A missing dir is not an error;
// valid files are always loaded, and any skipped file is reported in the
// returned (joined) error so the caller can warn without failing startup.
func LoadSubagentsInto(r *SubagentRegistry, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var skipped []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			skipped = append(skipped, fmt.Errorf("read %s: %w", e.Name(), err))
			continue
		}
		def, ok := parseSubagent(string(data))
		if !ok {
			skipped = append(skipped, fmt.Errorf("%s: missing or malformed frontmatter", e.Name()))
			continue
		}
		if def.Name == "" {
			def.Name = strings.TrimSuffix(e.Name(), ".md")
		}
		r.Add(def)
	}
	return errors.Join(skipped...)
}

// parseSubagent reads a markdown file with a simple `key: value` frontmatter
// block delimited by `---`, where the body after it is the system prompt.
// Recognized keys: name, description, tools (comma-separated).
func parseSubagent(s string) (SubagentDef, bool) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return SubagentDef{}, false
	}
	rest := s[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return SubagentDef{}, false
	}
	front := rest[:end]
	body := strings.TrimPrefix(rest[end+len("\n---"):], "\n")

	def := SubagentDef{Prompt: strings.TrimSpace(body)}
	for _, line := range strings.Split(front, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "name":
			def.Name = v
		case "description":
			def.Description = v
		case "tools":
			for _, t := range strings.Split(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					def.Tools = append(def.Tools, t)
				}
			}
		}
	}
	return def, true
}

// DelegationPrompt is the system-prompt block describing when to hand work to a
// subagent. It is appended by the front-end that registers the task tool, the
// way the skill and search blocks are, rather than living in the base prompt:
// there it disappeared behind a user's custom SYSTEM.md while the tool itself
// stayed registered, leaving the model a capability nothing explained. Building
// it from the registry also keeps custom agents from being invisible here.
//
// It deliberately carries no "do not over-delegate" rule. One was tried and
// measured (evals/): it never moved delegation precision, which the suite
// already scored at 100% without it, while on the built-in prompt it cut
// delegation on an explicit "I want an independent review" to zero. The base
// prompt already says to answer small things directly, and a second copy of
// that advice here only tips the balance.
//
// The batching rule likewise says how to delegate several pieces, not that
// several targets oblige you to delegate at all. Measured on three small
// packages, one subagent per package cost about 2k tokens MORE than reading
// them directly - each summary is nearly as long as the file behind it - so a
// model that declined was right, and the rule that ordered it to is not.
func DelegationPrompt(r *SubagentRegistry) string {
	if r == nil || len(r.Names()) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`# Delegation and parallelism

You can hand a self-contained piece of work to a specialized agent with the task tool. It runs in
its own context and reports back a summary, so everything it read stays out of this conversation.

- Delegate work that is heavy or self-contained: a sweep whose intermediate output you do not
  need, an implementation task you can state completely, a review of code you just wrote.
- Delegate whenever the user ASKS for something an agent below exists for - an independent
  review, a second opinion, a look at each of several things. There the separate context IS what
  was asked for, so the size of the job does not matter.
- A sub-agent cannot see this conversation and cannot delegate further, and you get back only its
  final text. Give it a complete, standalone prompt.
- The agents available to you:
`)
	for _, d := range r.List() {
		fmt.Fprintf(&b, "  - %s: %s\n", d.Name, d.Description)
	}
	b.WriteString(`- Independent tool calls you put in a SINGLE response run in parallel; calls in separate
  responses run one after another. That is true of every tool, not just task, so to run work
  concurrently you MUST emit all the calls in one response.
- Once you have decided to delegate several independent pieces, emit one task call PER piece, all
  together in your VERY NEXT single response - never one, wait for it, then the next.
- When the user asks for work to be done "in parallel", the decision is already made for you: one
  task call per target, in one response.`)
	return b.String()
}

// ---- delegation tool ----

// TaskToolName is the delegation tool's name, exported so front-ends can
// recognize and specially render its (grouped) activity.
const TaskToolName = "task"

// taskTool lets the main agent delegate a self-contained task to a subagent,
// which runs in its own context and returns a summary.
type taskTool struct {
	client  streamer
	tools   *tools.Registry
	temp    float64
	confirm ConfirmFunc
	agents  *SubagentRegistry
	project string // project conventions appended to every subagent's prompt
}

// NewTaskTool builds the delegation tool. Register it into reg so the main
// agent can call it. project is the discovered project-convention block (may be
// empty) shared with every subagent.
func NewTaskTool(client streamer, reg *tools.Registry, temp float64, confirm ConfirmFunc,
	agents *SubagentRegistry, project string) tools.Tool {
	return &taskTool{client: client, tools: reg, temp: temp, confirm: confirm, agents: agents, project: project}
}

func (t *taskTool) Name() string       { return TaskToolName }
func (t *taskTool) NeedsConfirm() bool { return false }

func (t *taskTool) Description() string {
	var b strings.Builder
	b.WriteString("Delegate a self-contained task to a specialized agent that works in its own " +
		"context and returns a summary. The sub-agent does NOT see this conversation, so write a " +
		"complete, standalone prompt. To run several INDEPENDENT sub-tasks in parallel, emit " +
		"multiple task calls in a SINGLE response - they execute concurrently; calls in separate " +
		"responses run sequentially. If the user asks for parallel work or names multiple targets " +
		"(e.g. both services), you MUST issue one task call per target together in one response, " +
		"not one at a time. Available agents:\n")
	for _, d := range t.agents.List() {
		fmt.Fprintf(&b, "- %s: %s\n", d.Name, d.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (t *taskTool) Schema() json.RawMessage {
	names := t.agents.Names()
	enum, _ := json.Marshal(names)
	return json.RawMessage(fmt.Sprintf(`{
		"type":"object",
		"properties":{
			"agent_type":{"type":"string","enum":%s,"description":"Which specialized agent to run."},
			"prompt":{"type":"string","description":"The complete, self-contained task for the sub-agent."}
		},
		"required":["agent_type","prompt"]
	}`, enum))
}

func (t *taskTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		AgentType string `json:"agent_type"`
		Prompt    string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	def, ok := t.agents.Get(a.AgentType)
	if !ok {
		return "", fmt.Errorf("unknown agent_type %q; available: %s",
			a.AgentType, strings.Join(t.agents.Names(), ", "))
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return "", fmt.Errorf("prompt is required: describe the task for the %s agent", def.Name)
	}

	toolNames := def.Tools
	if len(toolNames) == 0 {
		toolNames = readOnlyTools
	}
	// excluding TaskToolName prevents a subagent from delegating further;
	// excluding TodoToolName keeps a subagent from mutating the parent's plan
	// (the plan tool is bound to the top-level agent).
	subReg := t.tools.Subset(excluding(excluding(toolNames, TaskToolName), TodoToolName))
	prompt := def.Prompt
	if t.project != "" {
		prompt += "\n\n" + t.project
	}
	sub := New(t.client, subReg, t.temp, t.attributedConfirm(def.Name), prompt)
	if r := HooksFrom(ctx); r != nil {
		sub.SetHooks(r)
		sub.subagentType = def.Name
	}

	sink := SinkFrom(ctx)
	ev := Events{}
	if sink != nil {
		sink.AgentStart(def.Name, a.Prompt)
		ev.OnToolStart = func(name string, a json.RawMessage) { sink.SubToolStart(def.Name, name, a) }
		ev.OnToolEnd = func(name, result string, err error) { sink.SubToolEnd(def.Name, name, result, err) }
		ev.OnNotice = func(text string) { sink.SubNotice(def.Name, text) }
	}

	// A subagent's deltas are shown nowhere and its partial answer is dropped on
	// error, so a transient provider failure mid-stream should cost a retry, not
	// the whole delegation.
	answer, err := sub.Run(llm.WithRetryAfterEmit(ctx), a.Prompt, ev)
	if sink != nil {
		sink.AgentEnd(answer, err)
	}
	if err != nil {
		return "", fmt.Errorf("%s agent failed: %w", def.Name, err)
	}
	if strings.TrimSpace(answer) == "" {
		answer = "(the sub-agent returned no text)"
	}
	return answer, nil
}

// attributedConfirm labels a subagent's confirmation requests with the agent
// name (e.g. "code-writer › bash"), so the user sees who is asking and an
// "Always" approval is scoped to that agent's tool rather than every caller's.
func (t *taskTool) attributedConfirm(agentName string) ConfirmFunc {
	if t.confirm == nil {
		return nil
	}
	return func(name string, args json.RawMessage) bool {
		return t.confirm(agentName+" › "+name, args)
	}
}

func excluding(names []string, drop string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}

// ---- nested-event plumbing ----

// Sink receives events from a running subagent so the UI can show its nested
// activity. It is passed to the delegation tool via the context. The id that
// groups a run is supplied by the implementation (the parent task call's id).
type Sink interface {
	AgentStart(agent, prompt string)
	AgentEnd(result string, err error)
	SubToolStart(agent, tool string, args json.RawMessage)
	SubToolEnd(agent, tool, result string, err error)
	SubNotice(agent, text string)
}

type sinkKey struct{}

// WithSink attaches a Sink to ctx for the delegation tool to find.
func WithSink(ctx context.Context, s Sink) context.Context {
	return context.WithValue(ctx, sinkKey{}, s)
}

// SinkFrom returns the Sink attached to ctx, or nil.
func SinkFrom(ctx context.Context) Sink {
	s, _ := ctx.Value(sinkKey{}).(Sink)
	return s
}
