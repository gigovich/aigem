package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/local"
	"github.com/gigovich/aigem/internal/tools"
)

// llmRetryAttempts is how many total tries an unattended bot's LLM call gets
// before the failure surfaces to the runtime (which then schedules a resume).
const llmRetryAttempts = 3

const botUsage = `usage:
  aigem bot create <name>   define a bot interactively
  aigem bot list            list configured bots
  aigem bot rm <name>       delete a bot
  aigem bot start [--addr host:port] [--origin url] [<name>...]
                            run one bot, several, or the whole fleet
                            (flags first: the names end the flags)
  aigem bot model [<name>] [<ref>]   show or switch the model a bot runs on
  aigem bot prompt <name>   print the bot's full assembled system prompt`

const botModelUsage = `usage:
  aigem bot model                  show every bot's model
  aigem bot model <name>           show one bot's model
  aigem bot model <name> <ref>     switch that bot to <ref> (e.g. openai/gpt-5.6-sol)
  aigem bot model --all <ref>      switch every bot to <ref>
  aigem bot model <name> --clear   go back to the auto-picked default
  aigem bot model --all --clear    clear every bot's model

Refs are listed by "aigem models", but a bot can only be pinned to a provider
that comes from the built-ins or ~/.config/aigem/models.json - never from a
repo's own .aigem/models.json. A running bot keeps its current model until it
is restarted.`

func runBotCommand(args []string) error {
	if len(args) == 0 {
		fmt.Println(botUsage)
		return nil
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: aigem bot create <name>")
		}
		return botCreate(args[1])
	case "list":
		return botList()
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: aigem bot rm <name>")
		}
		return bot.Remove(args[1])
	case "start":
		return botStart(args[1:])
	case "model":
		return botModel(args[1:])
	case "prompt":
		if len(args) < 2 {
			return fmt.Errorf("usage: aigem bot prompt <name>")
		}
		return botPrompt(args[1])
	case "-h", "--help", "help":
		fmt.Println(botUsage)
		return nil
	default:
		return fmt.Errorf("unknown bot subcommand %q\n\n%s", args[0], botUsage)
	}
}

func botList() error {
	names, err := bot.List()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("no bots configured")
		return nil
	}
	for _, n := range names {
		c, err := bot.Load(n)
		if err != nil {
			fmt.Printf("%s\t(unreadable: %v)\n", n, err)
			continue
		}
		profile := c.CapabilityProfile
		if profile == "" {
			profile = tools.DefaultCapabilityProfile
		}
		model := c.Model
		if model == "" {
			model = "auto"
		}
		fmt.Printf("%s\trole=%s\tprofile=%s\tmodel=%s\n", n, c.Role, profile, model)
	}
	return nil
}

// botModelRegistry builds the registry a pinned bot model is resolved against.
// See llm.NewUserRegistry for why the project-local models.json is left out.
func botModelRegistry() *llm.Registry {
	localCfg, _, _ := local.Load()
	reg, warns := llm.NewUserRegistry(localProvider(localCfg, defaultMaxTokens))
	warnModelsConfig(warns)
	return reg
}

// resolveBotModel resolves ref and opens it the way botStart will, discarding the
// backend: opening is what actually rejects a logged-out provider or a model the
// stored credential cannot reach, so a bad switch fails here instead of at the
// bot's next start. Opening builds clients only - no request is sent - so a
// revoked or expired credential still surfaces later, at the first turn.
func resolveBotModel(reg *llm.Registry, ref string) (llm.ModelInfo, error) {
	if strings.TrimSpace(ref) == "" {
		return llm.ModelInfo{}, fmt.Errorf("empty model ref")
	}
	_, _, m, err := auth.OpenModel(reg, ref, defaultMaxTokens)
	if err != nil {
		return llm.ModelInfo{}, err
	}
	return m, nil
}

// botModel dispatches "aigem bot model ...". Flags and positional arguments may
// be interleaved.
func botModel(args []string) error {
	var all, clear bool
	var pos []string
	for _, a := range args {
		switch a {
		case "--all":
			all = true
		case "--clear":
			clear = true
		case "-h", "--help", "help":
			fmt.Println(botModelUsage)
			return nil
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown flag %q\n\n%s", a, botModelUsage)
			}
			pos = append(pos, a)
		}
	}

	configured, err := bot.List()
	if err != nil {
		return err
	}
	// A single positional means "report this bot" and only turns into a model ref
	// when a second one follows, so it is never guessed by shape.
	names, ref := pos, ""
	switch {
	case all && clear:
		if len(pos) > 0 {
			return fmt.Errorf("--all --clear takes no other arguments\n\n%s", botModelUsage)
		}
	case all:
		if len(pos) != 1 {
			return fmt.Errorf("--all needs exactly one model ref\n\n%s", botModelUsage)
		}
		// Without this, "aigem bot model kate --all" would read the bot name as a
		// ref and, if some bot's name matched a bare model id, silently switch every bot.
		if slices.Contains(configured, pos[0]) {
			return fmt.Errorf("%q is a bot name; --all switches every bot and takes a model ref\n\n%s",
				pos[0], botModelUsage)
		}
		ref, names = pos[0], nil
	case clear:
		if len(pos) != 1 {
			return fmt.Errorf("--clear needs exactly one bot name\n\n%s", botModelUsage)
		}
	case len(pos) == 2:
		names, ref = pos[:1], pos[1]
	case len(pos) > 2:
		return fmt.Errorf("too many arguments\n\n%s", botModelUsage)
	}

	if all || len(names) == 0 {
		if len(configured) == 0 {
			fmt.Println("no bots configured")
			return nil
		}
		names = configured
	}

	if ref == "" && !clear {
		return reportBotModels(names, len(pos) == 0)
	}
	return setBotModels(names, ref)
}

// reportBotModels prints one row per bot. tolerate keeps a broken config from
// hiding the others, the way `bot list` does; a name the operator typed is an
// error instead - and rows are collected before anything prints, so that error
// is not preceded by a half-written table.
func reportBotModels(names []string, tolerate bool) error {
	reg := botModelRegistry()
	rows := make([]string, 0, len(names))
	for _, n := range names {
		row, err := botModelRow(reg, n)
		if err != nil {
			if !tolerate {
				return err
			}
			row = fmt.Sprintf("%s\t\t%v", n, err)
		}
		rows = append(rows, row)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "bot\tmodel\tsource")
	for _, r := range rows {
		fmt.Fprintln(w, r)
	}
	return w.Flush()
}

// setBotModels validates the ref and every target before writing any of them, so
// a ref one bot rejects cannot leave the fleet half-switched.
func setBotModels(names []string, ref string) error {
	for _, n := range names {
		if _, err := bot.Load(n); err != nil {
			return fmt.Errorf("bot %q: %w", n, err)
		}
	}
	if ref != "" {
		m, err := resolveBotModel(botModelRegistry(), ref)
		if err != nil {
			return explainPinFailure(ref, err)
		}
		ref = m.Ref() // normalize a bare id to provider/id
	}
	changed := 0
	for _, n := range names {
		// Re-read rather than reuse the pre-flight copy: a running bot rewrites its
		// own bot.yaml to persist cron jobs, and writing back a snapshot taken
		// before that would erase them.
		c, err := bot.Load(n)
		if err != nil {
			return fmt.Errorf("bot %q: %w", n, err)
		}
		if c.Model == ref {
			continue
		}
		c.Name, c.Model = n, ref
		if err := bot.Save(c); err != nil {
			return fmt.Errorf("bot %q: %w", n, err)
		}
		changed++
		if ref == "" {
			fmt.Printf("%s: model cleared (auto)\n", n)
		} else {
			fmt.Printf("%s: model set to %s\n", n, ref)
		}
	}
	if changed == 0 {
		fmt.Println("no change")
		return nil
	}
	fmt.Println("restart the affected bots for the change to take effect")
	return nil
}

// explainPinFailure names the one rejection that looks like a bug: a ref that
// `aigem models` lists from the current directory, but that pinning cannot use
// because only a project-local models.json defines it.
func explainPinFailure(ref string, err error) error {
	if _, _, uerr := botModelRegistry().Resolve(ref); uerr == nil {
		return err // it resolved; the failure is about the credential, not the file
	}
	if _, _, perr := defaultModelRegistry().Resolve(ref); perr != nil {
		return err
	}
	return fmt.Errorf("%w\n%q comes from a project-local .aigem/models.json, which is not consulted when pinning a bot",
		err, ref)
}

// logUsagePerCall makes an unattended bot report what it spends: it has no
// screen to put a gauge on, so the log is the only record of the burn rate. The
// callback fires once per model call and carries that call's own cost, so
// concurrent threads cannot smear each other's numbers. A retried attempt is
// reported only if it reached the model: one the provider rejected outright
// returns before any usage is recorded.
func logUsagePerCall(log *slog.Logger, client *llm.Ref) {
	rep, ok := llm.UsageOf(client)
	if !ok {
		return
	}
	rep.OnCall(func(u llm.Usage, r llm.UsageReport) {
		args := []any{
			"in", u.InputTokens, "cached", u.CachedTokens, "out", u.OutputTokens,
			"total_in", r.Total.InputTokens, "total_out", r.Total.OutputTokens, "calls", r.Calls,
		}
		if r.Uncounted > 0 {
			args = append(args, "uncounted", r.Uncounted)
		}
		if w, ok := r.Limits.Tightest(); ok {
			args = append(args, "limit", w.Name)
			if w.UsedPercent > 0 {
				args = append(args, "used_pct", w.UsedPercent)
			}
			if w.Remaining != "" {
				args = append(args, "remaining", w.Remaining)
			}
		}
		log.Info("llm usage", args...)
	})

	// Persisting hangs off the quota callback, not the usage one: a rejected call
	// reports no tokens, and a 429's reading is the one most worth having on disk.
	// It is throttled because a turn is dozens of calls and the snapshot only has
	// to be fresh enough for `aigem usage` to be worth reading.
	var saved atomic.Pointer[time.Time]
	rep.OnLimits(func(l llm.Limits) {
		now := time.Now()
		if last := saved.Load(); last != nil && now.Sub(*last) < quotaSaveInterval {
			return
		}
		saved.Store(&now)
		if serr := llm.SaveLimits(l); serr != nil {
			log.Debug("could not persist quota snapshot", "err", serr)
		}
	})
}

// quotaSaveInterval bounds how often a bot rewrites its provider's snapshot.
const quotaSaveInterval = time.Minute

// saveCronJobs returns the scheduler's persist hook. It re-reads bot.yaml on
// every call instead of closing over the config loaded at startup, so a model
// switch made from the CLI while the bot runs is not erased by the next cron
// write. Nothing locks the file, so a switch landing inside this read-modify-write
// can still be lost; the scheduler re-persists its jobs, a lost switch is silent,
// which is why the command tells the operator to restart the bot.
func saveCronJobs(name string) func([]bot.CronJob) error {
	return func(jobs []bot.CronJob) error {
		c, err := bot.Load(name)
		if err != nil {
			return err
		}
		c.Name, c.Cron = name, jobs
		return bot.Save(c)
	}
}

// botModelRow renders one tab-separated row. An unpinned bot shows the model it
// would pick today, which is the value a switch would actually be changing.
func botModelRow(reg *llm.Registry, name string) (string, error) {
	c, err := bot.Load(name)
	if err != nil {
		return "", fmt.Errorf("bot %q: %w", name, err)
	}
	if pinned := strings.TrimSpace(c.Model); pinned != "" {
		note := "configured"
		if _, err := resolveBotModel(reg, pinned); err != nil {
			note = "configured, UNUSABLE: " + err.Error()
		}
		return fmt.Sprintf("%s\t%s\t%s", name, pinned, note), nil
	}
	effective := "(none available)"
	if def, ok := reg.DefaultPreferring(auth.IsAuthenticated); ok {
		effective = def.Ref()
	}
	return fmt.Sprintf("%s\t%s\tauto", name, effective), nil
}

func promptLine(rd *bufio.Reader, label string) string {
	fmt.Print(label)
	line, _ := rd.ReadString('\n')
	return strings.TrimSpace(line)
}

func botCreate(name string) error {
	rd := bufio.NewReader(os.Stdin)

	fmt.Println("Roles:")
	for _, r := range bot.Roles() {
		fmt.Printf("  %-10s %s\n", r.Name, r.Description)
	}
	role := promptLine(rd, "role: ")
	if _, ok := bot.RoleByName(role); !ok {
		return fmt.Errorf("unknown role %q", role)
	}

	persona := promptLine(rd, "persona (optional, e.g. \"female; use feminine forms in Russian\"): ")

	workdir := promptLine(rd, "workdir [.]: ")
	if workdir == "" {
		workdir = "."
	}
	profile := promptLine(rd, "capability profile ["+tools.DefaultCapabilityProfile+"]: ")
	if profile == "" {
		profile = tools.DefaultCapabilityProfile
	}
	if _, err := tools.ResolveCapabilityProfile(profile); err != nil {
		return err
	}

	// Four questions and no network call. There is no server to point at, no
	// team to resolve and no token to verify: a bot reaches its conversations
	// through the store this process already owns.
	c := bot.Config{Name: name, Role: role, Persona: persona, Workdir: workdir,
		CapabilityProfile: profile}
	if err := bot.Save(c); err != nil {
		return err
	}
	fmt.Printf("bot %q ready (role %s)\n", name, role)
	return nil
}

// intersectTools is the tools a role wants and the capability profile allows.
// A tool outside the intersection is not registered at all, so it is not merely
// refused at call time - the model never sees it.
func intersectTools(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, name := range b {
		set[name] = true
	}
	out := make([]string, 0, len(a))
	for _, name := range a {
		if set[name] {
			out = append(out, name)
		}
	}
	return out
}

// errRunner is returned by the agent factory when a thread's tool registry cannot be
// built; its error surfaces as an in-thread reply instead of crashing the whole bot.
type errRunner struct{ err error }

func (r errRunner) Run(_ context.Context, _ string, _ agent.Events) (string, error) {
	return "", r.err
}

// botPrompt assembles and prints the bot's full system prompt exactly as botStart would,
// minus the live transport and model. Memory index and skills catalog are read from the
// bot's own directories, so the output matches what the running bot sees.
func botPrompt(name string) error {
	c, err := bot.Load(name)
	if err != nil {
		return err
	}
	role, ok := bot.RoleByName(c.Role)
	if !ok {
		return fmt.Errorf("bot %q has unknown role %q", name, c.Role)
	}

	extra := config.ProjectInstructions(c.Workdir)

	memDir, err := bot.MemoryDir(name)
	if err != nil {
		return err
	}
	idx, err := bot.NewStore(memDir).Index()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not read memory index:", err)
	}

	skillsDir, err := bot.SkillsDir(name)
	if err != nil {
		return err
	}
	skills, skillErrs := bot.DiscoverBotSkills(skillsDir)
	for _, e := range skillErrs {
		fmt.Fprintln(os.Stderr, "warning: skills:", e)
	}

	fmt.Println(bot.ComposeSystem(c, role, idx, skills.Prompt(), extra))
	return nil
}
