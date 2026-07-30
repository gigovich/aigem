package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/local"
	"github.com/gigovich/aigem/internal/mcp"
	"github.com/gigovich/aigem/internal/search"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/tui"
)

// version labels this client to MCP servers in the initialize handshake and is
// what `aigem version` prints. Release builds stamp it with -ldflags; a plain
// `go build` leaves the placeholder, and a `go install` of a tagged module
// picks the tag up from the build info below.
var version = "dev"

// commit and date are stamped by the release build; they stay empty otherwise.
var (
	commit = ""
	date   = ""
)

// versionString renders the full version line. When the binary was not stamped
// (a bare `go install`), it falls back to the module version the toolchain
// recorded in the build info, so installed builds still identify themselves.
func versionString() string {
	v := version
	if v == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
	}
	if commit != "" {
		v += " (" + commit
		if date != "" {
			v += ", " + date
		}
		v += ")"
	}
	return v
}

// Built-in local-provider defaults, shared by the flag defaults and the
// auth/models subcommands so the two never drift.
const (
	defaultURL       = "http://127.0.0.1:9280"
	defaultCtxSize   = 262144
	defaultMaxTokens = 8192
)

// localProvider builds the registry's local provider from the on-disk local
// config (or defaults when unset).
func localProvider(cfg local.Config, maxTokens int) llm.Provider {
	return llm.LocalProvider(cfg.BaseURL(), cfg.ModelName, cfg.CtxSize, maxTokens)
}

// flagWasSet reports whether the named flag was passed explicitly (vs. its
// default), so config values only yield to flags the user actually set.
func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// hostPortFromURL extracts host and port from a base URL like
// http://127.0.0.1:9280, falling back to the given defaults on parse failure.
func hostPortFromURL(raw, defHost string, defPort int) (string, int) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return defHost, defPort
	}
	host := u.Hostname()
	port := defPort
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	if host == "" {
		host = defHost
	}
	return host, port
}

// ensureLocalReady handles the local model when it is the resolved startup
// selection. Only -p (non-interactive) mode acts here: it auto-starts an
// initialized-but-down server and fatals when the model was never set up, since
// it cannot prompt. Interactive front-ends do nothing at startup - the user
// picks a model first and only then sets up / starts the local one on demand
// (the TUI /model flow), so startup never blocks on local setup.
func ensureLocalReady(cfg local.Config, nonInteractive bool) {
	if !nonInteractive {
		return
	}
	switch local.Assess(cfg, local.Exists()) {
	case local.ActionNeedsStart:
		if err := startLocalServerCLI(cfg); err != nil {
			fatal(err)
		}
	case local.ActionNeedsInit:
		fatal(fmt.Errorf("local model not initialized; run `aigem models init` first"))
	}
}

func main() {
	// "aigem mcp ..." and "aigem auth ..." manage config and exit before the flag
	// parser (which is tuned for the chat front-ends) ever runs.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mcp":
			if err := runMCPCommand(os.Args[2:]); err != nil {
				fatal(err)
			}
			return
		case "auth":
			if err := runAuthCommand(os.Args[2:]); err != nil {
				fatal(err)
			}
			return
		case "models":
			if err := runModelsCommand(os.Args[2:]); err != nil {
				fatal(err)
			}
			return
		case "usage":
			if err := runUsageCommand(os.Args[2:]); err != nil {
				fatal(err)
			}
			return
		case "search":
			if err := runSearchCommand(os.Args[2:]); err != nil {
				fatal(err)
			}
			return
		case "paths":
			if err := runPathsCommand(os.Args[2:]); err != nil {
				fatal(err)
			}
			return
		case "version", "--version", "-v":
			fmt.Println("aigem " + versionString())
			return
		case "bot":
			if err := runBotCommand(os.Args[2:]); err != nil {
				fatal(err)
			}
			return
		}
	}

	url := flag.String("url", defaultURL, "llama.cpp server base URL")
	model := flag.String("model", "",
		"model to use: provider/id (e.g. openai/gpt-5.6-sol), a bare local model name, or empty for the default")
	cwd := flag.String("cwd", ".", "working directory (sandbox root)")
	temp := flag.Float64("temp", 0.3, "sampling temperature")
	maxTokens := flag.Int("max-tokens", defaultMaxTokens, "cap tokens per response (bounds runaway generation; 0 = no cap)")
	ctxSize := flag.Int("ctx-size", defaultCtxSize, "model context window in tokens (for the usage gauge)")
	repl := flag.Bool("repl", false, "run the plain CLI REPL instead of the TUI")
	prompt := flag.String("p", "", "run a single prompt non-interactively and exit")
	yes := flag.Bool("y", false, "auto-approve confirm-gated tools in -p mode (bash requires --capability-profile shell or dangerous-shell)")
	capProfileName := flag.String("capability-profile", tools.DefaultCapabilityProfile,
		"non-interactive capability profile: read-only, workspace-write, shell, or dangerous-shell")
	trustProject := flag.Bool("trust-project-hooks", false,
		"trust and run this project's local hooks (.aigem/.claude settings)")
	trustProjectMCP := flag.Bool("trust-project-mcp", false,
		"trust and start this project's currently configured local MCP servers and approval policies")
	trustProjectSkills := flag.Bool("trust-project-skills", false,
		"trust this project's current local skill definitions")
	compactAuto := flag.Bool("compact-auto", true, "enable automatic context compaction")
	compactAtPct := flag.Int("compact-at-pct", 70, "context %% at which to summarize (stage 3)")
	evictAtPct := flag.Int("evict-at-pct", 50, "context %% at which to evict old tool output (stage 1+2)")
	keepTurns := flag.Int("compact-keep-turns", 10, "recent user turns kept verbatim across summarization")
	keepTools := flag.Int("compact-keep-tools", 4, "recent tool results kept verbatim during eviction")
	turnTimeout := flag.Duration("turn-timeout", agent.DefaultBudgetMaxDuration, "wall-clock budget for one -p turn (0 disables)")
	maxModelRounds := flag.Int("max-model-rounds", agent.DefaultBudgetMaxModelRounds, "max model rounds for one -p turn (0 disables)")
	maxToolCalls := flag.Int("max-tool-calls", agent.DefaultBudgetMaxToolCalls, "max tool calls for one -p turn (0 disables)")
	maxRepeatToolCalls := flag.Int("max-repeat-tool-calls", agent.DefaultBudgetMaxRepeatedToolCalls, "max identical tool calls for one -p turn (0 disables)")
	flag.Parse()

	// Resolve the model from the registry. A bare --model name configures the
	// local provider (back-compat); a provider/id ref selects another provider;
	// empty prefers an authenticated OpenAI, else local.
	localCfg, _, _ := local.Load()
	if flagWasSet("url") {
		localCfg.Host, localCfg.Port = hostPortFromURL(*url, localCfg.Host, localCfg.Port)
	}
	if flagWasSet("ctx-size") {
		localCfg.CtxSize = *ctxSize
	}
	selectionRef := *model
	if *model != "" && !strings.Contains(*model, "/") {
		localCfg.ModelName = *model
		selectionRef = llm.LocalProviderID + "/" + *model
	}
	modelReg, modelWarns := llm.NewRegistry(*cwd, localProvider(localCfg, *maxTokens))
	for _, w := range modelWarns {
		fmt.Fprintln(os.Stderr, "warning: models config:", w)
	}
	// No --model flag: prefer the last model the user selected (if it still
	// resolves and is usable), else fall back to an authenticated default.
	if selectionRef == "" {
		if pref := config.LoadPrefs().Model; pref != "" && modelUsable(modelReg, pref) {
			selectionRef = pref
		}
	}
	if selectionRef == "" {
		if def, ok := modelReg.DefaultPreferring(auth.IsAuthenticated); ok {
			selectionRef = def.Ref()
		}
	}
	// If the resolved selection is the local model, in -p mode make sure it is
	// set up and serving before we try to open it (interactive front-ends defer
	// this to an explicit model selection).
	if prov, _, rerr := modelReg.Resolve(selectionRef); rerr == nil && prov.ID == llm.LocalProviderID {
		ensureLocalReady(localCfg, *prompt != "")
	}
	backend, _, info, err := auth.OpenModel(modelReg, selectionRef, *maxTokens)
	if err != nil {
		fatal(err)
	}
	ref := llm.NewRef(backend)
	modelLabel, urlLabel := info.Ref(), backend.Endpoint()
	if info.ContextWindow > 0 {
		*ctxSize = info.ContextWindow
	}

	compactCfg := agent.CompactConfig{
		Auto:         *compactAuto,
		CtxSize:      *ctxSize,
		CompactAtPct: *compactAtPct,
		EvictAtPct:   *evictAtPct,
		KeepTurns:    *keepTurns,
		KeepTools:    *keepTools,
	}

	reg, err := tools.NewRegistry(*cwd)
	if err != nil {
		fatal(err)
	}
	// Directories the user has already approved for this project are read without
	// asking again. Each front-end installs its own approver for the ones that are
	// not yet granted; -p has no one to ask and keeps refusing them.
	reg.SetPathGrants(true)

	// First-run search setup: in an interactive front-end (TUI or REPL, never -p),
	// ask the user to pick a search provider before the session starts. Skipping it
	// just leaves the agent without web_search.
	if *prompt == "" && !search.Exists() {
		if err := runSearchSetup(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: search setup skipped:", err)
		}
	}
	// Load the (possibly just-configured) search provider and expose web_search.
	searchCfg, err := search.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not load search config:", err)
	}
	if st := search.NewTool(searchCfg); st != nil {
		reg.Register(st)
	}
	if bt := search.NewBrowseTool(searchCfg); bt != nil {
		reg.Register(bt)
	}
	if at := search.NewBrowserActionTool(searchCfg); at != nil {
		reg.Register(at)
	}

	agents := loadAgents()
	if *trustProjectSkills {
		if err := skill.ApproveProject(*cwd); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not approve project skills:", err)
		}
	}
	// skillNotices are repeated inside the TUI, which runs on the alt screen and
	// so never shows what was written to stderr before it started.
	var skillNotices []string
	skills, skillErrs := skill.Discover(*cwd)
	for _, e := range skillErrs {
		fmt.Fprintln(os.Stderr, "warning: skipped skill:", e)
		skillNotices = append(skillNotices, "skipped skill: "+e.Error())
	}
	// Discovery drops untrusted project-local skills silently, which is
	// indistinguishable from the project having none. Interactive front-ends ask
	// (the TUI overlay below); the rest at least say what was withheld.
	pendingSkills, err := skill.Pending(*cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not evaluate project skill trust:", err)
		skillNotices = append(skillNotices, "could not evaluate project skill trust: "+err.Error())
	}
	if w := pendingSkillsWarning(pendingSkills); w != "" && (*prompt != "" || *repl) {
		fmt.Fprintln(os.Stderr, w)
	}
	// Skill dirs above the project root belong to no project, so they are neither
	// loaded nor offered for approval. Saying so beats letting them disappear.
	for _, d := range skill.OutOfScopeAncestors(*cwd) {
		w := "skills in " + d + " are outside this project and were not loaded"
		fmt.Fprintln(os.Stderr, "warning:", w)
		skillNotices = append(skillNotices, w)
	}
	// Project conventions (AGENTS.md / CLAUDE.md / context.md / .claude/CLAUDE.md)
	// are discovered by the harness and injected, not left for the model to find.
	// The full system prompt is assembled by buildSystem below; this snapshot is
	// for consumers that need the project text directly (subagent task tool).
	project := config.ProjectInstructions(*cwd)

	// MCP servers: dial configured servers, register their tools (main agent
	// only), and summarize them in the prompt. A failed server is skipped.
	mcpMgr, mcpCfgWarns := mcp.NewWithTrust(*cwd, version, *trustProjectMCP)
	defer mcpMgr.Close() // also tears down stdio servers if a run path panics
	for _, w := range mcpCfgWarns {
		fmt.Fprintln(os.Stderr, "warning: mcp config:", w)
	}
	if !mcpMgr.Empty() {
		mcpMgr.Connect(context.Background())
		for _, w := range mcpMgr.Warnings() {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		mcpMgr.RegisterTools(reg)
	}

	// Hooks from project/global settings. SessionStart runs once now (bounded, so
	// a slow hook cannot freeze startup) so its additionalContext can enrich the
	// system prompt before the first model call.
	runner, hookWarns := hooks.Load(*cwd)
	for _, w := range hookWarns {
		fmt.Fprintln(os.Stderr, "warning: hook config:", w)
	}
	if *trustProject && runner.HasUntrustedProjectHooks() {
		if err := runner.TrustProject(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not persist project trust:", err)
		}
	}
	// Non-interactive front-ends cannot prompt, so untrusted project hooks stay
	// off; tell the user how to enable them.
	if (*prompt != "" || *repl) && runner.HasUntrustedProjectHooks() {
		fmt.Fprintln(os.Stderr, "warning: project-local hooks present but untrusted - skipping; "+
			"pass --trust-project-hooks to run them.")
	}
	dec := runner.RunBounded(hooks.EventSessionStart, hooks.Input{Source: "startup"}, 10*time.Second)
	hookCtx := dec.Context // launch-time SessionStart context, reused verbatim on /new
	if dec.SystemMessage != "" {
		fmt.Fprintln(os.Stderr, dec.SystemMessage)
	}

	// buildSystem assembles the full system prompt: base instructions, the current
	// on-disk project conventions (re-read each call), the skill catalog, search
	// and MCP availability, and the launch-time hook context. /new re-runs it so
	// edits to AGENTS.md/CLAUDE.md/context.md take effect without a restart; the
	// SessionStart hook and MCP servers are not re-run, only their captured output
	// is reused.
	buildSystem := func() string {
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
		if proj := config.ProjectInstructions(*cwd); proj != "" {
			sp += "\n\n" + proj
			// Those files are now in context; read_file should not re-emit them.
			reg.MarkInContext(config.InstructionPaths(*cwd))
		}
		if sk := skills.Prompt(); sk != "" {
			sp += "\n\n" + sk
		}
		if s := search.Prompt(searchCfg); s != "" {
			sp += "\n\n" + s
		}
		if !mcpMgr.Empty() {
			if p := mcpMgr.Prompt(); p != "" {
				sp += "\n\n" + p
			}
		}
		if hookCtx != "" {
			sp += "\n\n" + hookCtx
		}
		return sp
	}
	sysPrompt := buildSystem()

	if *prompt != "" {
		capProfile, err := tools.ResolveCapabilityProfile(*capProfileName)
		if err != nil {
			fatal(err)
		}
		reg = reg.Subset(capProfile.Allow)
		turnBudget := agent.TurnBudget{
			MaxModelRounds:       *maxModelRounds,
			MaxToolCalls:         *maxToolCalls,
			MaxRepeatedToolCalls: *maxRepeatToolCalls,
			MaxDuration:          *turnTimeout,
		}
		runPrint(ref, reg, *temp, *prompt, *yes, capProfile, turnBudget, sysPrompt, agents, project, skills, runner, compactCfg)
		return
	}
	if *repl {
		runREPL(ref, reg, *temp, sysPrompt, agents, project, skills, runner, compactCfg)
		return
	}
	m := tui.New(ref, reg, *temp, modelLabel, urlLabel, sysPrompt, *ctxSize, *maxTokens, agents, project, skills, runner, mcpMgr, dec.SessionTitle, compactCfg, modelReg)
	m.SetSystemRebuilder(buildSystem)
	m.SetStartupNotices(skillNotices)
	m.SetPendingSkills(*cwd, pendingSkills)
	if err := tui.Run(m); err != nil {
		mcpMgr.Close() // defer is skipped by fatal's os.Exit; close before exiting
		fatal(err)
	}
}

// pendingSkillsWarning describes project-local skills that discovery withheld,
// for the front-ends that have no one to ask. It returns "" when nothing is
// pending. The names are already sanitized for display by skill.Pending.
func pendingSkillsWarning(p *skill.PendingSkills) string {
	if p == nil {
		return ""
	}
	state := "untrusted"
	if p.Invalidated {
		state = "changed since you approved them"
	}
	return fmt.Sprintf("warning: project-local skills in %s %s - not loaded (%s); "+
		"pass --trust-project-skills to enable them.", p.Dir, state, strings.Join(p.Names, ", "))
}

// loadAgents returns the built-in subagents plus any custom definitions found
// in the user's config directory.
func loadAgents() *agent.SubagentRegistry {
	agents := agent.DefaultSubagents()
	if dir, err := config.AgentsDir(); err == nil {
		if err := agent.LoadSubagentsInto(agents, dir); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not load custom agents:", err)
		}
	}
	return agents
}

// replPathApprover asks on the terminal about a path outside the working
// directory. "a" grants the file's directory for this project and is persisted;
// it is not offered for a write, which is approved one call at a time.
func replPathApprover(root string) tools.PathApprover {
	in := bufio.NewReader(os.Stdin)
	return func(path string, intent tools.PathIntent) tools.PathDecision {
		verb, choices := "read", "[y]es once / [a]lways this folder / [N]o"
		if intent.Write {
			verb, choices = "modify", "[y]es once / [N]o"
		}
		fmt.Printf("\n  [confirm] %s wants to %s %s\n  outside %s\n  %s ", intent.Tool, verb, path, root, choices)
		line, _ := in.ReadString('\n')
		switch strings.TrimSpace(strings.ToLower(line)) {
		case "y", "yes":
			return tools.PathAllowOnce
		case "a", "always":
			if intent.Write {
				return tools.PathDeny
			}
			return tools.PathAllowDir
		}
		return tools.PathDeny
	}
}

// llmStreamer is what the agent and its tools need from a backend. The retry
// decorator satisfies it, so it can stand in for the *llm.Ref everywhere the
// model is called, while the Ref keeps serving model metadata and usage.
type llmStreamer interface {
	Stream(ctx context.Context, messages []llm.Message, tools []llm.Tool, temperature float64,
		onEvent func(llm.StreamEvent)) (llm.Message, error)
}

// cliRetryAttempts is how many total tries an interactive or one-shot LLM call
// gets. Bots use llmRetryAttempts; the value is the same but the reasons differ,
// so they stay separate knobs.
const cliRetryAttempts = 3

// retrying wraps the live backend so a transient provider failure (429/5xx, an
// overloaded backend, a dropped stream) is retried instead of ending the run. It
// wraps the Ref, not the backend inside it, so a model switch keeps the retries.
// notice reports each backoff so the wait does not read as a hang.
func retrying(client *llm.Ref, notice func(string)) llmStreamer {
	r := llm.NewRetrying(client, cliRetryAttempts)
	r.SetOnRetry(func(n llm.RetryNotice) {
		reason := ""
		if n.Err != nil {
			reason = ": " + firstLine(n.Err.Error())
		}
		notice(fmt.Sprintf("provider call failed, retrying in %s (attempt %d of %d)%s",
			n.Delay.Round(time.Second), n.Attempt+1, n.Attempts, reason))
	})
	return r
}

// registerSkillTool adds the skill tool to reg when there are model-invocable
// skills (NewSkillTool returns nil otherwise).
func registerSkillTool(reg *tools.Registry, skills *skill.Registry, client llmStreamer, temp float64,
	confirm agent.ConfirmFunc) {
	if st := agent.NewSkillTool(skills, client, reg, temp, confirm); st != nil {
		reg.Register(st)
	}
}

func runREPL(client *llm.Ref, reg *tools.Registry, temp float64, sysPrompt string,
	agents *agent.SubagentRegistry, project string, skills *skill.Registry, runner *hooks.Runner,
	compactCfg agent.CompactConfig) {
	confirm := func(name string, args json.RawMessage) bool {
		fmt.Printf("\n  [confirm] run %s %s ? [y/N] ", name, string(args))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimSpace(strings.ToLower(line)) == "y"
	}
	reg.SetPathApprover(replPathApprover(reg.Root()))
	stream := retrying(client, func(text string) { fmt.Printf("\n  ⚠ %s\n", text) })
	reg.Register(agent.NewTaskTool(stream, reg, temp, confirm, agents, project))
	registerSkillTool(reg, skills, stream, temp, confirm)
	ag := agent.New(stream, reg, temp, confirm, sysPrompt)
	reg.Register(agent.NewTodoTool(ag))
	ag.SetHooks(runner)
	ag.SetCompaction(compactCfg)
	ag.WatchSkills(skills.Conditional())

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	fmt.Println("aigem REPL - type a message, Ctrl+C to quit.")
	for {
		fmt.Print("\n> ")
		if !in.Scan() {
			return
		}
		input := strings.TrimSpace(in.Text())
		if input == "" {
			continue
		}
		ev := agent.Events{
			OnContent: func(d string) { fmt.Print(d) },
			OnToolStart: func(name string, args json.RawMessage) {
				fmt.Printf("\n  · %s %s\n", name, string(args))
			},
			OnToolEnd: func(name, result string, err error) {
				fmt.Printf("  ⤷ %s\n", firstLine(result))
			},
			OnNotice: func(text string) { fmt.Printf("\n  ⚠ %s\n", text) },
		}
		if _, err := ag.Run(context.Background(), input, ev); err != nil {
			fmt.Printf("\n[error] %v\n", err)
		}
		fmt.Println()
	}
}

// runPrint executes one prompt non-interactively. Tool activity is written to
// stderr so stdout carries only the model's final answer.
func runPrint(client *llm.Ref, reg *tools.Registry, temp float64, prompt string, autoApprove bool,
	capProfile tools.CapabilityProfile, turnBudget agent.TurnBudget, sysPrompt string, agents *agent.SubagentRegistry, project string, skills *skill.Registry,
	runner *hooks.Runner, compactCfg agent.CompactConfig) {
	confirm := func(name string, args json.RawMessage) bool {
		if !autoApprove {
			fmt.Fprintf(os.Stderr, "  [denied] %s %s (pass -y to allow within --capability-profile %s)\n", name, string(args), capProfile.Name)
			return false
		}
		if name == "bash" {
			if !capProfile.AutoApproveBash {
				fmt.Fprintf(os.Stderr, "  [denied] %s %s (-y does not approve bash under --capability-profile %s; use shell or dangerous-shell)\n", name, string(args), capProfile.Name)
				return false
			}
			if tools.IsDestructive(name, args) && !capProfile.AutoApproveDestructiveBash {
				fmt.Fprintf(os.Stderr, "  [denied] %s %s (destructive bash requires --capability-profile dangerous-shell)\n", name, string(args))
				return false
			}
		}
		return true
	}
	stream := retrying(client, func(text string) { fmt.Fprintf(os.Stderr, "  ⚠ %s\n", text) })
	reg.Register(agent.NewTaskTool(stream, reg, temp, confirm, agents, project))
	registerSkillTool(reg, skills, stream, temp, confirm)
	ag := agent.New(stream, reg, temp, confirm, sysPrompt)
	reg.Register(agent.NewTodoTool(ag))
	ag.SetHooks(runner)
	ag.SetTurnBudget(turnBudget)
	ag.SetCompaction(compactCfg)
	ag.WatchSkills(skills.Conditional())

	ev := agent.Events{
		OnContent: func(d string) { fmt.Print(d) },
		OnToolStart: func(name string, args json.RawMessage) {
			fmt.Fprintf(os.Stderr, "  · %s %s\n", name, string(args))
		},
		OnToolEnd: func(name, result string, err error) {
			fmt.Fprintf(os.Stderr, "  ⤷ %s\n", firstLine(result))
		},
		OnNotice: func(text string) { fmt.Fprintf(os.Stderr, "  ⚠ %s\n", text) },
		OnAgentStart: func(id, ag, prompt string) {
			fmt.Fprintf(os.Stderr, "  ▸ %s: %s\n", ag, firstLine(prompt))
		},
		OnAgentEnd: func(id, result string, err error) {
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ▸ failed: %v\n", err)
			}
		},
		OnSubToolStart: func(id, ag, name string, args json.RawMessage) {
			fmt.Fprintf(os.Stderr, "    ▸ %s:%s %s\n", ag, name, string(args))
		},
		OnSubToolEnd: func(id, ag, name, result string, err error) {
			if err != nil {
				fmt.Fprintf(os.Stderr, "    ⤷ error: %v\n", err)
				return
			}
			fmt.Fprintf(os.Stderr, "    ⤷ %s\n", firstLine(result))
		},
		OnSubNotice: func(id, ag, text string) { fmt.Fprintf(os.Stderr, "    ⚠ %s: %s\n", ag, text) },
	}
	if _, err := ag.Run(context.Background(), prompt, ev); err != nil {
		fatal(err)
	}
	runner.Run(context.Background(), hooks.EventSessionEnd, hooks.Input{Source: "exit"})
	fmt.Println()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// modelUsable reports whether ref resolves in the registry and can be opened now
// - a model whose provider needs auth but is not authenticated is not usable, so
// a stale saved preference never blocks startup.
func modelUsable(reg *llm.Registry, ref string) bool {
	p, _, err := reg.Resolve(ref)
	if err != nil {
		return false
	}
	return !p.NeedsAuth() || auth.IsAuthenticated(p.ID)
}
