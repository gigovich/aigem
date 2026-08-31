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
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
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
//
// A variable so a test can shrink it: the behaviour that matters - Load
// returning while the hook is still running - only appears once a hook outlives
// it, and a test that waits ten real seconds is a test nobody runs.
var sessionStartTimeout = 10 * time.Second

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

	// Notify, when set, is called with each Notice as it is raised rather than
	// only at the end.
	//
	// Load dials the MCP servers and runs the SessionStart hook, either of which
	// can take tens of seconds, and a terminal that says nothing until they
	// finish reads as a hang - it used to report what it had found within a
	// hundred milliseconds. The returned slice is unchanged, so a caller that
	// only wants the list at the end can still ignore this.
	Notify func(Notice)
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
	// Askable marks a notice a front-end that can ask should answer with a
	// question rather than a line of text: a withheld capability the person can
	// approve on the spot. The TUI opens its trust overlay for these and does
	// not print them; a front-end with nobody to ask prints them like any other,
	// because the alternative is a capability that goes missing in silence.
	Askable bool
}

// Env is one project's environment, shared by every session that runs in it.
//
// Sharing is deliberate for the catalog and the servers: a skill registry is
// read-only, and one MCP manager per project is what keeps two parallel
// sessions from starting two copies of every stdio server. What is not shared
// is the tools registry - see NewTools.
//
// Hooks is shared because loading it is cheap and its configuration is the
// project's, but it is NOT stateless: it carries the id of the conversation its
// hooks are being fired for, and uisession writes that as each turn begins. One
// runner across two live sessions therefore hands every hook whichever
// conversation started last. That is invisible while a process holds one
// session, and is the first thing to split when the daemon holds several.
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

	// closed is set by Close. It is read by NewTools, which must not hand out a
	// registry full of MCP tools whose servers have been torn down: the manager
	// goes on listing them after it closes, so without this a session built
	// after Close gets a full catalog and a prompt advertising it, and every
	// call fails.
	closed atomic.Bool
}

// Load discovers the environment at opts.Cwd.
//
// The only thing it refuses is a working directory it cannot resolve, and it
// refuses that first, before anything below has run: a SessionStart hook
// executes the person's own commands and the MCP servers are started, so a
// startup that was always going to fail must not do either on its way out.
// Everything else degrades to a Notice and a missing capability, because a
// project with a broken hook config is still a project a person wants to work
// in.
//
// The caller owns Close.
func Load(opts Options) (*Env, []Notice, error) {
	// The same failure tools.NewRegistry reports, raised where the CLI used to
	// raise it: os.Getwd failing under a deleted working directory.
	root, err := filepath.Abs(opts.Cwd)
	if err != nil {
		return nil, nil, fmt.Errorf("runner: resolve working directory %q: %w", opts.Cwd, err)
	}

	var notices []Notice
	raise := func(n Notice) {
		notices = append(notices, n)
		if opts.Notify != nil {
			opts.Notify(n)
		}
	}
	warn := func(text string) { raise(Notice{Text: text}) }
	warnInChat := func(text string) { raise(Notice{Text: text, InChat: true}) }

	e := &Env{Cwd: root, Search: opts.Search}

	e.Agents = agent.DefaultSubagents()
	if dir, err := config.AgentsDir(); err == nil {
		if err := agent.LoadSubagentsInto(e.Agents, dir); err != nil {
			warn("could not load custom agents: " + err.Error())
		}
	}

	if opts.TrustProjectSkills {
		if err := skill.ApproveProject(root); err != nil {
			warn("could not approve project skills: " + err.Error())
		}
	}
	skills, skillErrs := skill.Discover(root)
	e.Skills = skills
	for _, err := range skillErrs {
		warnInChat("skipped skill: " + err.Error())
	}
	pending, err := skill.Pending(root)
	if err != nil {
		warnInChat("could not evaluate project skill trust: " + err.Error())
	}
	e.Pending = pending
	// Raised here rather than by the caller after Load returns: discovery has
	// just decided to withhold them, and everything below - the MCP dial, the
	// SessionStart hook - can take tens of seconds. A person waiting for their
	// skills to appear should not learn why last.
	if w := pendingSkillsWarning(pending); w != "" {
		raise(Notice{Text: w, Askable: true})
	}
	// Skill dirs above the project root belong to no project, so they are
	// neither loaded nor offered for approval. Saying so beats letting them
	// disappear.
	for _, dir := range skill.OutOfScopeAncestors(root) {
		warnInChat("skills in " + dir + " are outside this project and were not loaded")
	}

	// Project conventions (AGENTS.md / CLAUDE.md / context.md / .claude/CLAUDE.md)
	// are discovered by the harness and injected, not left for the model to find.
	e.Project = config.ProjectInstructions(root)

	// MCP servers are dialled here and summarized in the prompt; their tools are
	// registered per session by NewTools. A server that fails to come up is
	// skipped with a warning rather than taking startup down with it.
	mgr, mcpCfgErrs := mcp.NewWithTrust(root, opts.Version, opts.TrustProjectMCP)
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

	runner, hookErrs := hooks.Load(root)
	e.Hooks = runner
	for _, err := range hookErrs {
		warn("hook config: " + err.Error())
	}
	if opts.TrustProjectHooks && runner.HasUntrustedProjectHooks() {
		if err := runner.TrustProject(); err != nil {
			warn("could not persist project trust: " + err.Error())
		}
	}
	if runner.HasUntrustedProjectHooks() {
		raise(Notice{
			Text: "project-local hooks present but untrusted - skipping; " +
				"pass --trust-project-hooks to run them.",
			Askable: true,
		})
	}
	// SessionStart runs once, here, so its additionalContext can enrich the
	// system prompt before the first model call.
	dec := runner.RunBounded(hooks.EventSessionStart, hooks.Input{Source: "startup"}, sessionStartTimeout)
	e.hookContext = dec.Context
	e.SessionTitle = dec.SessionTitle
	e.SystemMessage = dec.SystemMessage

	return e, notices, nil
}

// Close releases what Load started, which today is the MCP servers. A session
// built from this Env must not outlive it. Calling it twice is safe.
func (e *Env) Close() {
	e.closed.Store(true)
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
	if e.closed.Load() {
		return nil, errors.New("runner: the environment is closed; its MCP servers are gone")
	}
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
// It returns the prompt and the instruction files it injected. Those files are
// now in one session's context and that session's read_file must not re-emit
// them, so the caller hands the paths to its own registry with MarkInContext -
// which is a fact about one conversation rather than about the project, and
// stays visible here rather than hiding in a closure that captured whichever
// registry happened to exist when it was built.
func (e *Env) SystemPrompt() (string, []string) {
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
	var injected []string
	if proj := config.ProjectInstructions(e.Cwd); proj != "" {
		sp += "\n\n" + proj
		injected = config.InstructionPaths(e.Cwd)
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
	return sp, injected
}

// pendingSkillsWarning describes project-local skills that discovery withheld.
// It returns "" when nothing is pending. The names are already sanitized for
// display by skill.Pending.
func pendingSkillsWarning(p *skill.PendingSkills) string {
	if p == nil {
		return ""
	}
	state := "untrusted"
	if p.Invalidated {
		state = "changed since you approved them"
	}
	return fmt.Sprintf("project-local skills in %s %s - not loaded (%s); "+
		"pass --trust-project-skills to enable them.", p.Dir, state, strings.Join(p.Names, ", "))
}
