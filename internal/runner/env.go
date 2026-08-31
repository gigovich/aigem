// Package runner builds what an agent session needs, and the sessions
// themselves.
//
// All of it used to be inlined in the CLI's startup path, which built exactly
// one session and then handed its parts to one front-end. A browser daemon
// builds many at once, and the split this package draws is the one that makes
// that possible: Env is what a project has - its skills, hooks, MCP servers,
// subagents and system prompt - discovered once and shared, while Spec and
// NewSession are what a single conversation owns, starting with a tools
// registry that must never be shared.
package runner

import (
	"context"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/mcp"
	"github.com/gigovich/aigem/internal/search"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
)

// sessionStartTimeout bounds the SessionStart hook, so a slow one delays
// startup rather than freezing it.
const sessionStartTimeout = 10 * time.Second

// Options is what Load cannot work out for itself.
type Options struct {
	// Cwd is the directory every discovery below is resolved from. The project
	// root is derived from it, so this is the sandbox root of the sessions that
	// will run here.
	Cwd string
	// Version labels this client to MCP servers in the initialize handshake.
	Version string
	// Search configures the web tools. A zero value registers none of them,
	// which is what an unconfigured search backend means.
	Search search.Config

	// TrustProjectHooks, TrustProjectMCP and TrustProjectSkills approve this
	// project's current local definitions before they are loaded. They are the
	// --trust-project-* flags: a decision a person makes, never a default.
	TrustProjectHooks  bool
	TrustProjectMCP    bool
	TrustProjectSkills bool
}

// Notice is something Load could not do and the caller should say so.
//
// They are returned rather than printed because where they belong depends on
// the front-end: a terminal writes them to stderr, and a daemon has no stderr
// anyone is reading. InChat marks the ones a front-end on the alt screen has to
// repeat in the transcript - they were raised before it took the screen, so
// whatever it wrote there is about to be painted over.
type Notice struct {
	Text   string
	InChat bool
}

// Env is one project's environment, shared by every session that runs in it.
//
// Sharing is deliberate for each of these: a skill registry is a read-only
// catalog, a hooks runner is stateless between events, and one MCP manager per
// project is what keeps two parallel sessions from starting two copies of every
// stdio server. What is not shared is the tools registry - see NewTools.
type Env struct {
	Cwd    string
	Search search.Config

	Agents *agent.SubagentRegistry
	Skills *skill.Registry
	Hooks  *hooks.Runner
	MCP    *mcp.Manager

	// Project is the project instruction files as they read at load time, for
	// the consumers that want the text itself rather than the assembled prompt -
	// the delegation tool hands it to a subagent. SystemPrompt re-reads them
	// instead, so a fresh conversation picks up an edit.
	Project string
	// Pending are the project-local skills discovery withheld for lack of
	// approval, or nil when there are none. Discovery drops them silently, which
	// is indistinguishable from the project having none, so a front-end that can
	// ask is expected to.
	Pending *skill.PendingSkills

	// SessionTitle and SystemMessage are what the SessionStart hook returned.
	// The message is not a Notice: a hook said it deliberately, and prefixing it
	// with "warning" would misreport it.
	SessionTitle  string
	SystemMessage string

	// hookContext is the SessionStart hook's additionalContext, appended to
	// every system prompt built here. It is captured once: /new rebuilds the
	// prompt but does not re-run the hook, so this is the output being reused.
	hookContext string
}

// Load discovers the environment at opts.Cwd. It never fails: everything it
// cannot do degrades to a Notice and a missing capability, because a project
// with a broken hook config is still a project a person wants to work in.
//
// The caller owns Close.
func Load(opts Options) (*Env, []Notice) {
	var notices []Notice
	warn := func(text string) { notices = append(notices, Notice{Text: text}) }
	warnInChat := func(text string) { notices = append(notices, Notice{Text: text, InChat: true}) }

	e := &Env{Cwd: opts.Cwd, Search: opts.Search}

	e.Agents = agent.DefaultSubagents()
	if dir, err := config.AgentsDir(); err == nil {
		if err := agent.LoadSubagentsInto(e.Agents, dir); err != nil {
			warn("could not load custom agents: " + err.Error())
		}
	}

	if opts.TrustProjectSkills {
		if err := skill.ApproveProject(opts.Cwd); err != nil {
			warn("could not approve project skills: " + err.Error())
		}
	}
	skills, skillErrs := skill.Discover(opts.Cwd)
	e.Skills = skills
	for _, err := range skillErrs {
		warnInChat("skipped skill: " + err.Error())
	}
	pending, err := skill.Pending(opts.Cwd)
	if err != nil {
		warnInChat("could not evaluate project skill trust: " + err.Error())
	}
	e.Pending = pending
	// Skill dirs above the project root belong to no project, so they are
	// neither loaded nor offered for approval. Saying so beats letting them
	// disappear.
	for _, dir := range skill.OutOfScopeAncestors(opts.Cwd) {
		warnInChat("skills in " + dir + " are outside this project and were not loaded")
	}

	// Project conventions (AGENTS.md / CLAUDE.md / context.md / .claude/CLAUDE.md)
	// are discovered by the harness and injected, not left for the model to find.
	e.Project = config.ProjectInstructions(opts.Cwd)

	// MCP servers are dialled here and summarized in the prompt; their tools are
	// registered per session by NewTools. A server that fails to come up is
	// skipped with a warning rather than taking startup down with it.
	mgr, mcpCfgErrs := mcp.NewWithTrust(opts.Cwd, opts.Version, opts.TrustProjectMCP)
	e.MCP = mgr
	for _, err := range mcpCfgErrs {
		warn("mcp config: " + err.Error())
	}
	if !mgr.Empty() {
		mgr.Connect(context.Background())
		for _, w := range mgr.Warnings() {
			warn(w)
		}
	}

	runner, hookErrs := hooks.Load(opts.Cwd)
	e.Hooks = runner
	for _, err := range hookErrs {
		warn("hook config: " + err.Error())
	}
	if opts.TrustProjectHooks && runner.HasUntrustedProjectHooks() {
		if err := runner.TrustProject(); err != nil {
			warn("could not persist project trust: " + err.Error())
		}
	}
	// SessionStart runs once, here, so its additionalContext can enrich the
	// system prompt before the first model call.
	dec := runner.RunBounded(hooks.EventSessionStart, hooks.Input{Source: "startup"}, sessionStartTimeout)
	e.hookContext = dec.Context
	e.SessionTitle = dec.SessionTitle
	e.SystemMessage = dec.SystemMessage

	return e, notices
}

// Close releases what Load started, which today is the MCP servers. A session
// built from this Env must not outlive it.
func (e *Env) Close() {
	if e.MCP != nil {
		e.MCP.Close()
	}
}

// NewTools builds the sandbox for one conversation.
//
// It is a constructor rather than a field because a registry is not shareable
// between sessions: the delegation and skill tools are registered into it bound
// to that session's confirmation function, so two conversations sharing one
// would have tool calls in the first asking the second's clients for approval.
//
// Persisted path grants are enabled by the session, not here.
func (e *Env) NewTools() (*tools.Registry, error) {
	r, err := tools.NewRegistry(e.Cwd)
	if err != nil {
		return nil, err
	}
	if t := search.NewTool(e.Search); t != nil {
		r.Register(t)
	}
	if t := search.NewBrowseTool(e.Search); t != nil {
		r.Register(t)
	}
	if t := search.NewBrowserActionTool(e.Search); t != nil {
		r.Register(t)
	}
	// Every registry gets them, not just the first: MCP tools are adapters bound
	// to the server connection, which the manager owns, so a second session
	// registering them again shares the connection rather than opening one.
	if !e.MCP.Empty() {
		e.MCP.RegisterTools(r)
	}
	return r, nil
}

// SystemPrompt assembles the full prompt for a session driving reg: the base
// instructions, the current on-disk project conventions, the skill catalog,
// what search and MCP make available, and the launch-time hook context.
//
// It re-reads the instruction files on every call, so a front-end that starts a
// fresh conversation picks up an edit to AGENTS.md or CLAUDE.md without a
// restart. The SessionStart hook and the MCP servers are not re-run - only
// their captured output is reused.
//
// reg is the session's own registry, and the reason this takes an argument at
// all: the instruction files it just put in the prompt are now in that
// session's context and read_file must not re-emit them, which is a fact about
// one conversation rather than about the project.
func (e *Env) SystemPrompt(reg *tools.Registry) string {
	sp := config.SystemPrompt()
	now := time.Now()
	sp += "\n\n# Current date and time\n\n" +
		"The current local date and time is " + now.Format("Monday, 2 January 2006, 15:04:05 MST") +
		" (" + now.Format(time.RFC3339) + "). This is authoritative: it reflects the real clock at the " +
		"start of this session, which your training data cannot tell you. Treat it as today's date. " +
		"When you compute any date or duration - ages, deadlines, \"how many days ago\", \"how long until\", " +
		"weekday of a date, or whether something is past or future - calculate strictly from this value, " +
		"never from your training cutoff, and never guess the current date. (This value is captured when " +
		"the prompt is built and refreshed on /new; in a very long session the time of day may have drifted.)"
	if proj := config.ProjectInstructions(e.Cwd); proj != "" {
		sp += "\n\n" + proj
		if reg != nil {
			reg.MarkInContext(config.InstructionPaths(e.Cwd))
		}
	}
	// Every front-end registers the task tool, so the block always applies. It is
	// appended rather than baked into the base prompt so a custom SYSTEM.md
	// cannot leave the tool present but unexplained.
	if d := agent.DelegationPrompt(e.Agents); d != "" {
		sp += "\n\n" + d
	}
	if sk := e.Skills.Prompt(); sk != "" {
		sp += "\n\n" + sk
	}
	if s := search.Prompt(e.Search); s != "" {
		sp += "\n\n" + s
	}
	if !e.MCP.Empty() {
		if p := e.MCP.Prompt(); p != "" {
			sp += "\n\n" + p
		}
	}
	if e.hookContext != "" {
		sp += "\n\n" + e.hookContext
	}
	return sp
}
