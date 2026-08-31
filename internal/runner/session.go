package runner

import (
	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
)

// retryAttempts is how many total tries one LLM call gets before the failure
// reaches the session. A stream that already emitted text is not retried, so an
// interruption mid-answer still surfaces - the deltas were delivered and a
// second attempt would duplicate them.
const retryAttempts = 3

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

	Temp      float64
	MaxTokens int
	// CtxSize is the context window to fall back on for a model that declares
	// none; zero picks DefaultCtxSize.
	CtxSize int
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
	stream := llm.NewRetrying(spec.Backend, retryAttempts)
	if spec.OnRetry != nil {
		stream.SetOnRetry(spec.OnRetry)
	}
	ctxSize := spec.CtxSize
	if ctxSize <= 0 {
		ctxSize = DefaultCtxSize
	}
	reg := spec.Tools

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
		Compact:   spec.Compact,
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
			ag.SetCompaction(spec.Compact)
			if spec.Skills != nil {
				ag.WatchSkills(spec.Skills.Conditional())
			}
			return ag
		},
	})
	return out
}
