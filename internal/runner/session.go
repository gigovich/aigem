package runner

import (
	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
)

// RetryAttempts is how many total tries one LLM call gets before the failure
// reaches the caller. Someone is waiting, so this rides out a brief provider
// hiccup without turning a failure into a long silence. A stream that already
// emitted text is not retried, so an interruption mid-answer still surfaces -
// the deltas were delivered and a second attempt would duplicate them.
//
// Exported because the front-ends that do not build a session through this
// package - the one-shot -p run and the REPL - wrap the same backend the same
// way, and two constants would drift.
const RetryAttempts = 3

// DefaultCtxSize is the context window assumed for a model that declares none
// and a caller that names none.
const DefaultCtxSize = 8192

// Spec is one conversation: the parts that are not shared with the other
// sessions in the same Env.
//
// Tools and Backend are the two that must be private. A registry carries the
// session's confirmation function, and the Ref is what a live model switch
// swaps the provider inside, so two sessions sharing one would switch together.
type Spec struct {
	// Tools is this session's sandbox, from Env.NewTools.
	Tools *tools.Registry
	// Backend is the handle every model call goes through. Models resolves a
	// reference when the session switches model.
	Backend *llm.Ref
	Models  *llm.Registry

	// Agents, Skills, Hooks and Project come from the Env. They are named here
	// rather than read off an Env so that a front-end which already has them -
	// the TUI, which is handed them by the CLI - can build a session without
	// one.
	Agents  *agent.SubagentRegistry
	Skills  *skill.Registry
	Hooks   *hooks.Runner
	Project string

	// System is the assembled system prompt, from Env.SystemPrompt. Title names
	// the conversation before its first turn does.
	System string
	Title  string
	// RebuildSystem reassembles the prompt when a fresh conversation starts, so
	// an edit to AGENTS.md or CLAUDE.md takes effect without a restart. Leaving
	// it nil pins the session to the prompt it was built with, which is what a
	// caller wants only if it installs its own rebuilder afterwards.
	RebuildSystem func() string

	Temp      float64
	MaxTokens int
	// CtxSize is the context window to fall back on for a model that declares
	// none; zero picks DefaultCtxSize.
	CtxSize int
	// Compact configures compaction. A zero CtxSize in it is filled from the
	// session's, because zero there switches auto-compaction off rather than
	// selecting a default.
	Compact agent.CompactConfig

	// OnRetry reports a provider call being retried, so the wait reads as a wait
	// rather than as a hang. It is called from the goroutine running the turn,
	// and must not block indefinitely.
	OnRetry func(llm.RetryNotice)
}

// Session is a conversation and the sandbox it runs in.
type Session struct {
	// Local is the session itself. It is the concrete type rather than the
	// uisession.Session interface because SetAutoMode and SwitchModel are
	// declared on it, and a run needs both.
	Local *uisession.Local
	// Tools is Spec.Tools, carried along so a caller holding the session does
	// not have to hold the registry separately.
	Tools *tools.Registry
	// RegisterSkillTool registers the skill tool against a skill registry,
	// replacing whatever was registered at construction.
	//
	// It exists because a project whose only skills are untrusted has none to
	// advertise at launch: approving them mid-session re-runs discovery, and the
	// tool has to be registered again against the result. It takes the registry
	// rather than closing over one, so a caller cannot silently register a tool
	// built from a catalog it has since replaced.
	//
	// It is only safe to call while no turn is running. It writes the session's
	// tools registry, whose map is unguarded and is read from the turn goroutine
	// as the model's tool definitions are assembled - concurrent map access
	// there is a fatal runtime error that takes every conversation in the
	// process with it, not an error this call can return.
	RegisterSkillTool func(*skill.Registry)
}

// NewSession builds a conversation from spec.
//
// The session builds the agent rather than being handed one, because the
// confirmation function the agent is constructed with belongs to the session:
// it is what parks a tool call on the approval queue that every attached
// front-end can answer.
func NewSession(spec Spec) *Session {
	// Ride out transient provider failures (429/5xx, an overloaded backend, a
	// dropped stream) instead of surfacing them into the session. It wraps the
	// Ref rather than the backend inside it, so a live model switch keeps the
	// retries.
	stream := llm.NewRetrying(spec.Backend, RetryAttempts)
	if spec.OnRetry != nil {
		stream.SetOnRetry(spec.OnRetry)
	}
	ctxSize := spec.CtxSize
	if ctxSize <= 0 {
		ctxSize = DefaultCtxSize
	}
	// The window the compaction settings work against is the session's, and a
	// zero there does not mean "default" - it means auto-compaction is off
	// entirely. Two fields from one intent must not be able to disagree: a
	// caller that names no window would otherwise get a usage gauge reading the
	// default while nothing ever compacts, until an unrelated model switch
	// repaired it.
	compact := spec.Compact
	if compact.CtxSize <= 0 {
		compact.CtxSize = ctxSize
	}
	reg := spec.Tools
	// A registry belongs to one conversation. Registering the delegation, skill
	// and todo tools into a second session's agent replaces them by name, so the
	// first session's registry would drive the second session's agent and route
	// its approval requests to the second session's clients. There is no error
	// to return - NewSession has no failure mode a caller could act on - and the
	// alternative is two people answering each other's questions.
	if _, taken := reg.Get(agent.TodoToolName); taken {
		panic("runner: this tools registry already belongs to a session; build one per " +
			"conversation with Env.NewTools")
	}

	out := &Session{Tools: reg}
	out.Local = uisession.New(uisession.Config{
		Tools:     reg,
		Hooks:     spec.Hooks,
		Title:     spec.Title,
		ModelRef:  func() string { return spec.Backend.Model().Ref() },
		Models:    spec.Models,
		Backend:   spec.Backend,
		MaxTokens: spec.MaxTokens,
		CtxSize:   ctxSize,
		Compact:   compact,

		RebuildSystem: spec.RebuildSystem,
		NewAgent: func(confirm agent.ConfirmFunc) *agent.Agent {
			if spec.Agents != nil {
				reg.Register(agent.NewTaskTool(stream, reg, spec.Temp, confirm, spec.Agents, spec.Project))
			}
			out.RegisterSkillTool = func(sk *skill.Registry) {
				if st := agent.NewSkillTool(sk, stream, reg, spec.Temp, confirm); st != nil {
					reg.Register(st)
				}
			}
			out.RegisterSkillTool(spec.Skills)
			ag := agent.New(stream, reg, spec.Temp, confirm, spec.System)
			reg.Register(agent.NewTodoTool(ag))
			ag.SetHooks(spec.Hooks)
			ag.SetCompaction(compact)
			if spec.Skills != nil {
				ag.WatchSkills(spec.Skills.Conditional())
			}
			return ag
		},
	})
	return out
}
