package runner

import (
	"errors"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/session"
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

// Mode selects the safety policy for a session.
type Mode string

const (
	// ModeInteractive is the zero-value mode. It is intended for a person who
	// can answer approval requests.
	ModeInteractive Mode = "interactive"
	// ModeAutonomous runs without persisted path grants and approves reversible
	// tools automatically.
	ModeAutonomous Mode = "autonomous"
)

// AutoMode reports whether the mode enables automatic approval of reversible
// tool calls. Unknown modes fail closed to the interactive policy.
func (m Mode) AutoMode() bool { return m == ModeAutonomous }

// PathGrants reports whether persisted directory grants may be consulted.
// Autonomous sessions never inherit approvals made by an interactive session.
func (m Mode) PathGrants() bool { return m != ModeAutonomous }

// CapabilitySubset returns the tool names exposed to an autonomous session.
// Interactive and unknown modes retain the complete registry. The returned slice
// is independent so callers cannot mutate the profile shared by other sessions.
func (m Mode) CapabilitySubset() []string {
	if m != ModeAutonomous {
		return nil
	}
	profile, err := tools.ResolveCapabilityProfile("")
	if err != nil {
		// The default is a package constant and is validated by tools' tests. A
		// failure here indicates an internal programming error, not user input.
		panic("runner: default capability profile unavailable: " + err.Error())
	}
	return append([]string(nil), profile.Allow...)
}

// TurnBudget returns the runaway-protection policy for a mode. Interactive and
// unknown modes remain unbounded; autonomous turns use finite conservative
// defaults so a missing approval or a looping model cannot run forever.
func (m Mode) TurnBudget() agent.TurnBudget {
	if m != ModeAutonomous {
		return agent.TurnBudget{}
	}
	return agent.TurnBudget{
		MaxModelRounds:       agent.DefaultBudgetMaxModelRounds,
		MaxToolCalls:         agent.DefaultBudgetMaxToolCalls,
		MaxRepeatedToolCalls: agent.DefaultBudgetMaxRepeatedToolCalls,
		MaxDuration:          agent.DefaultBudgetMaxDuration,
	}
}

// Spec is one conversation: the parts that are not shared with the other
// sessions in the same Env.
//
// Tools and Backend are the two that must be private. A registry carries the
// session's confirmation function, and the Ref is what a live model switch
// swaps the provider inside, so two sessions sharing one would switch together.
type Spec struct {
	// Mode selects the session policy. Its zero value is interactive.
	Mode Mode

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

	// SessionID and TranscriptPath bind hooks to this conversation. An empty id
	// is replaced with a fresh id by NewSession.
	SessionID      string
	TranscriptPath string
	Cwd            string
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
	// registerSkillTool points the session's skill tool at a catalog, replacing
	// whatever was registered at construction and unregistering the tool when
	// the catalog has nothing left to advertise.
	//
	// It is unexported because calling it is only safe between turns: it writes
	// the session's tools registry, which the turn goroutine reads as it
	// assembles the model's tool definitions, and a schema that changes halfway
	// through a turn describes tools the model was never shown. SetSkills is
	// that call with the synchronisation it needs.
	registerSkillTool func(*skill.Registry)
}

// SetSkills points this session's skill tool at sk and tells the model about
// it: the tool is registered against the new catalog (or unregistered when the
// catalog advertises nothing), the path-gated skills are re-armed, and the
// system prompt is reassembled so the listing matches the tool.
//
// It exists because a project whose only skills are untrusted has none to
// advertise at launch: approving them mid-session re-runs discovery, and every
// session already built has to be brought to the result. It takes the catalog
// rather than closing over one, so a caller cannot silently register a tool
// built from a set it has since replaced.
//
// A turn in progress refuses the change with uisession.ErrBusy rather than
// racing it, and a closed session with uisession.ErrClosed.
func (s *Session) SetSkills(sk *skill.Registry) error {
	if s == nil || s.Local == nil {
		return errors.New("runner: no session to give the skills to")
	}
	return s.Local.Reconfigure(func(ag *agent.Agent) {
		if s.registerSkillTool != nil {
			s.registerSkillTool(sk)
		}
		if ag != nil {
			var conditional []*skill.Skill
			if sk != nil {
				conditional = sk.Conditional()
			}
			ag.WatchSkills(conditional)
		}
	})
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
	if reg == nil {
		panic("runner: session requires a tools registry")
	}
	if subset := spec.Mode.CapabilitySubset(); subset != nil {
		reg = reg.Subset(subset)
	}
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

	if spec.SessionID == "" {
		spec.SessionID = session.NewID(time.Now())
	}
	boundHooks := spec.Hooks
	if spec.Hooks != nil {
		boundHooks = spec.Hooks.ForSession(spec.SessionID, spec.TranscriptPath, spec.Cwd)
	}
	if boundHooks != nil {
		start := boundHooks.RunBounded(hooks.EventSessionStart,
			hooks.Input{Source: "startup"}, sessionStartTimeout)
		if spec.Title == "" {
			spec.Title = start.SessionTitle
		}
		if start.Context != "" {
			spec.System += "\n\n" + start.Context
		}
	}

	var newHooks func(id string) *hooks.Runner
	if spec.Hooks != nil {
		newHooks = func(id string) *hooks.Runner {
			return spec.Hooks.ForSession(id, spec.TranscriptPath, spec.Cwd)
		}
	}
	out := &Session{Tools: reg}
	out.Local = uisession.New(uisession.Config{
		Tools:          reg,
		AutoMode:       spec.Mode.AutoMode(),
		Hooks:          boundHooks,
		NewHooks:       newHooks,
		SessionID:      spec.SessionID,
		TranscriptPath: spec.TranscriptPath,
		Title:          spec.Title,
		ModelRef:       func() string { return spec.Backend.Model().Ref() },
		Models:         spec.Models,
		Backend:        spec.Backend,
		MaxTokens:      spec.MaxTokens,
		CtxSize:        ctxSize,
		Compact:        compact,

		RebuildSystem: spec.RebuildSystem,
		NewAgent: func(confirm agent.ConfirmFunc) *agent.Agent {
			if spec.Agents != nil {
				reg.Register(agent.NewTaskTool(stream, reg, spec.Temp, confirm, spec.Agents, spec.Project))
			}
			out.registerSkillTool = func(sk *skill.Registry) {
				st := agent.NewSkillTool(sk, stream, reg, spec.Temp, confirm)
				if st == nil {
					// The new catalog advertises nothing, so leaving the tool
					// registered would offer the model an empty enum built from
					// a set that is gone.
					reg.Unregister(agent.SkillToolName)
					return
				}
				reg.Register(st)
			}
			out.registerSkillTool(spec.Skills)
			ag := agent.New(stream, reg, spec.Temp, confirm, spec.System)
			reg.Register(agent.NewTodoTool(ag))
			ag.SetTurnBudget(spec.Mode.TurnBudget())
			ag.SetHooks(boundHooks)
			ag.SetCompaction(compact)
			if spec.Skills != nil {
				ag.WatchSkills(spec.Skills.Conditional())
			}
			return ag
		},
	})
	// Local defaults to the interactive policy for compatibility with existing
	// front-ends; apply the session mode after construction so autonomous runs
	// cannot inherit persisted grants.
	reg.SetPathGrants(spec.Mode.PathGrants())
	return out
}
