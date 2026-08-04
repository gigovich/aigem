package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/bot/mattermost"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/local"
	"github.com/gigovich/aigem/internal/search"
	"github.com/gigovich/aigem/internal/tools"
)

// Running the whole team in one process is what these constants are sized for. The two fleet
// caps - how many turns and how many browsers may run at once - live in fleet.json instead, since
// they depend on the provider account and the machine; see bot.FleetConfig.
const (
	// botRestartDelay is how long a crashed bot waits before its first restart,
	// doubling up to botRestartMaxDelay. A bot that dies instantly and forever
	// then costs a slow trickle of retries instead of a spin.
	botRestartDelay    = 5 * time.Second
	botRestartMaxDelay = 5 * time.Minute

	// botStableFor is how long a restarted bot must stay up before its backoff is
	// considered spent. Below it, the restart counts as part of the same failure.
	botStableFor = time.Minute
)

// fleetResources are the things every bot in the process shares. They exist once
// per run and are what makes one process cheaper than one process per bot.
type fleetResources struct {
	fleet    *bot.Fleet
	turns    *bot.TurnLimiter
	launches *search.LaunchGate
}

// botStart runs the named bots, or the whole configured fleet when no name is
// given. Every bot gets its own goroutine, transport, scheduler and agent; they
// share this process, its provider connections and its caps.
func botStart(names []string) error {
	for _, a := range names {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("unknown flag %q\n\nusage: aigem bot start [<name>...]", a)
		}
	}
	names, err := resolveBotNames(names)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	limits, err := bot.LoadFleetConfig()
	if err != nil {
		return err
	}
	shared := &fleetResources{
		fleet:    bot.NewFleet(),
		turns:    bot.NewTurnLimiter(limits.TurnCap()),
		launches: search.NewLaunchGate(limits.BrowserCap()),
	}
	if len(names) == 1 {
		// One bot cannot contend with anyone, and a cap it can hit on its own would
		// only slow it down.
		shared.turns = nil
	}
	slog.Info("fleet limits", "bots", len(names), "max_turns", shared.turns.Cap(),
		"max_browsers", limits.BrowserCap())

	// Start sequentially and wait for each to be connected before the next: one
	// Mattermost account allows one websocket, and opening several at once is what
	// the server rate-limits.
	var wg sync.WaitGroup
	started := 0
	for _, name := range names {
		log := slog.Default().With("bot", name)
		h, err := startBot(ctx, name, shared, log)
		if err != nil {
			if len(names) == 1 {
				return err
			}
			// One unstartable bot must not keep the rest of the team down. It is
			// still supervised, so a bot whose provider or token recovers joins later.
			log.Error("bot did not start; will keep retrying", "err", err)
			wg.Add(1)
			go func(name string, log *slog.Logger) {
				defer wg.Done()
				superviseBot(ctx, name, shared, log, botRestartDelay)
			}(name, log)
			continue
		}
		started++
		log.Info("bot running", "role", h.role)
		wg.Add(1)
		go func(h *botHandle, name string, log *slog.Logger) {
			defer wg.Done()
			serveAndSupervise(ctx, h, name, shared, log)
		}(h, name, log)
	}
	if started == 0 && len(names) > 1 {
		// Nothing came up, so stop the retrying supervisors rather than leaving them running
		// behind an error the caller has already been given. An orchestrator restarting the
		// process is the better retry loop when the whole fleet is down.
		stop()
		wg.Wait()
		return fmt.Errorf("none of the %d configured bots could be started", len(names))
	}
	fmt.Printf("%d/%d bots running; press Ctrl+C to stop\n", started, len(names))
	wg.Wait()
	return nil
}

// resolveBotNames turns the command's arguments into the bots to run: every
// configured bot when there are none, and otherwise exactly the ones named,
// rejecting an unknown name rather than silently running a smaller fleet.
func resolveBotNames(args []string) ([]string, error) {
	configured, err := bot.List()
	if err != nil {
		return nil, err
	}
	if len(configured) == 0 {
		return nil, fmt.Errorf("no bots configured (create one with: aigem bot create <name>)")
	}
	if len(args) == 0 {
		return configured, nil
	}
	var out []string
	for _, name := range args {
		if !slices.Contains(configured, name) {
			return nil, fmt.Errorf("unknown bot %q; configured bots: %s", name, strings.Join(configured, ", "))
		}
		if slices.Contains(out, name) {
			continue // naming a bot twice asks for one bot, not two websockets
		}
		out = append(out, name)
	}
	return out, nil
}

// serveAndSupervise runs a started bot's event loop and, if it ends before the
// process is shutting down, restarts the bot from scratch.
func serveAndSupervise(ctx context.Context, h *botHandle, name string, shared *fleetResources, log *slog.Logger) {
	err := h.serve(ctx)
	if ctx.Err() != nil {
		return
	}
	log.Error("bot stopped; restarting", "err", err)
	superviseBot(ctx, name, shared, log, botRestartDelay)
}

// superviseBot restarts a bot until the process stops.
//
// A crash used to take down one process out of five, and a supervisor outside
// aigem put it back. In one process a panic in any bot would take the whole team
// with it, so the recovery that used to live outside now has to live here.
func superviseBot(ctx context.Context, name string, shared *fleetResources, log *slog.Logger, delay time.Duration) {
	for ctx.Err() == nil {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		h, err := startBot(ctx, name, shared, log)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("bot restart failed", "err", err, "retry_in", delay)
			delay = nextDelay(delay)
			continue
		}
		log.Info("bot restarted", "role", h.role)
		started := time.Now()
		err = h.serve(ctx)
		if ctx.Err() != nil {
			return
		}
		// Reset the backoff only for a bot that actually stayed up. Resetting on any successful
		// start would let a bot that dies a second after connecting reconnect every few seconds
		// forever, paying a full dial, model open and team resolve each time.
		if time.Since(started) >= botStableFor {
			delay = botRestartDelay
		} else {
			delay = nextDelay(delay)
		}
		log.Error("bot stopped; restarting", "err", err, "retry_in", delay)
	}
}

func nextDelay(d time.Duration) time.Duration {
	d *= 2
	if d > botRestartMaxDelay {
		return botRestartMaxDelay
	}
	return d
}

// botHandle is one started bot: connected, scheduled, and waiting to be served.
type botHandle struct {
	name  string
	role  string
	rt    *bot.Runtime
	sched *bot.Scheduler
	log   *slog.Logger
	// ctx is this bot's own context; close cancels it.
	ctx context.Context
	// close cancels the bot's context and releases everything startBot opened, in reverse order.
	close func()
}

// serve runs the bot until its transport ends or ctx is cancelled, and always releases what
// startBot opened.
//
// The recover here only covers this goroutine. Turns run on their own goroutines and contain
// their own panics (see Runtime.handle and NewCronRunner), which is the containment that matters
// now that one panic could take every bot in the process down.
func (h *botHandle) serve(ctx context.Context) (err error) {
	defer h.close()
	defer func() {
		if p := recover(); p != nil {
			h.log.Error("bot panicked", "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	go h.sched.Run(h.ctx)
	if serr := h.rt.Serve(ctx); serr != nil && !errors.Is(serr, context.Canceled) {
		return serr
	}
	return nil
}

// startBot builds one bot: model, memory, skills, transport, scheduler and
// runtime. It returns once the bot is connected and ready to be served.
func startBot(ctx context.Context, name string, shared *fleetResources, log *slog.Logger) (*botHandle, error) {
	c, err := bot.Load(name)
	if err != nil {
		return nil, err
	}
	role, ok := bot.RoleByName(c.Role)
	if !ok {
		return nil, fmt.Errorf("bot %q has unknown role %q", name, c.Role)
	}
	token, err := bot.LoadToken(name)
	if err != nil {
		return nil, err
	}

	client, err := openBotModel(name, c, log)
	if err != nil {
		return nil, err
	}
	logUsagePerCall(log, client)

	extra := config.ProjectInstructions(c.Workdir)
	memDir, err := bot.MemoryDir(name)
	if err != nil {
		return nil, err
	}
	store := bot.NewStore(memDir)
	store.SetLogger(log)
	skillsDir, err := bot.SkillsDir(name)
	if err != nil {
		return nil, err
	}
	capProfile, err := tools.ResolveCapabilityProfile(c.CapabilityProfile)
	if err != nil {
		return nil, err
	}
	turnBudget, err := c.TurnBudget.ResolveTurnBudgetFor(bot.TurnBudgetForRole(role.Name))
	if err != nil {
		return nil, err
	}
	gate := bot.AllowGate(capProfile)

	// Throttle the request rate so unattended runs stay under provider limits,
	// and ride out transient provider failures (429/5xx/dropped streams) with a
	// few retries. Retrying wraps Paced - not the other way around - so a failed
	// attempt returns immediately (Paced only pauses after a successful call) and
	// the pacing pause is never multiplied by retry backoff. Both decorators are
	// stateless, so the per-thread agents built below share them.
	paceFactor := c.ResolveLLMPaceFactor()
	paced := llm.NewRetrying(llm.NewPaced(client, paceFactor), llmRetryAttempts)
	log.Info("llm pacing", "factor", paceFactor, "retries", llmRetryAttempts)

	if _, err := tools.NewRegistry(c.Workdir); err != nil {
		return nil, fmt.Errorf("workdir %q is not usable: %w", c.Workdir, err)
	}

	searchCfg := botSearchConfig(name, shared, log)

	// Everything this bot starts hangs off its own context, not the process one. A bot that is
	// restarted must take its scheduler with it: the process context outlives the restart, so a
	// scheduler bound to it would keep firing this bot's cron and heartbeat jobs forever, one
	// extra copy per restart, each still persisting its own snapshot of the job list.
	botCtx, cancelBot := context.WithCancel(ctx)
	tr, err := mattermost.Dial(botCtx, c.Transport.ServerURL, token, c.Transport.BotUserID, log)
	if err != nil {
		cancelBot()
		return nil, fmt.Errorf("connect mattermost: %w", err)
	}
	closers := []func(){func() { _ = tr.Close() }}
	closeAll := func() {
		cancelBot() // stop the scheduler first, so nothing new starts while we tear down
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}

	if ts, terr := bot.NewThreadStore(name); terr != nil {
		log.Warn("thread state unavailable", "err", terr)
	} else {
		tr.SeedThreads(ts.Load())
		tr.SetThreadSink(func(version uint64, ids []string) {
			if err := ts.Save(version, ids); err != nil {
				log.Warn("could not persist followed threads", "err", err)
			}
		})
	}

	mmClient := mattermost.NewClient(c.Transport.ServerURL, token)
	teamID, terr := mmClient.TeamID(botCtx, c.Transport.Team)
	if terr != nil {
		log.Warn("could not resolve team for posting", "err", terr)
	}
	resolver := mmResolver{client: mmClient, team: c.Transport.Team, teamID: teamID}
	local := &bot.LocalDelivery{Self: name, SelfUserID: c.Transport.BotUserID, Fleet: shared.fleet}

	scheduler, cronWarns := bot.NewScheduler(c.Cron, saveCronJobs(name))
	scheduler.SetLogger(log)
	for _, w := range cronWarns {
		log.Warn("cron job skipped", "err", w)
	}
	if err := scheduler.SetBuiltin(bot.MemoryReviewJob(name)); err != nil {
		log.Warn("memory review job not installed", "err", err)
	}
	heartbeat := bot.NewHeartbeat(name, scheduler)
	heartbeat.SetLogger(log)
	// Fatal on purpose: without the heartbeat a bot that runs out of chat wake-ups has no way
	// back to its own work, which is the exact stall this job exists to prevent.
	if err := heartbeat.Arm(); err != nil {
		closeAll()
		return nil, err
	}

	buildAgent := func() (*agent.Agent, func() string, error) {
		reg, rerr := tools.NewRegistry(c.Workdir)
		if rerr != nil {
			return nil, nil, rerr
		}
		reg.Register(bot.NewMemoryTool(store))
		reg.Register(bot.NewScheduleTool(scheduler))
		reg.Register(bot.NewPostMessageTool(tr, resolver, local))
		reg.Register(bot.NewHandoffTool(tr, resolver, local))
		reg.Register(bot.NewReadChatTool(tr, resolver))
		reg.Register(bot.NewSaveSkillTool(skillsDir))
		reg.Register(bot.NewDeleteSkillTool(skillsDir))
		if tt := bot.NewTeamStatusTool(name, shared.fleet); tt != nil {
			reg.Register(tt)
		}
		skills, skillErrs := bot.DiscoverBotSkills(skillsDir)
		for _, e := range skillErrs {
			log.Warn("skills discovery", "err", e)
		}
		if st := agent.NewSkillTool(skills, paced, reg, 0.3, gate); st != nil {
			reg.Register(st)
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
		sub := reg.Subset(intersectTools(role.Allow, capProfile.Allow))
		catalog := skills.Prompt()
		build := func() string {
			idx, ierr := store.Index()
			if ierr != nil {
				log.Warn("could not read memory index", "err", ierr)
			}
			return bot.ComposeSystem(c, role, idx, catalog, extra)
		}
		ag := agent.New(paced, sub, 0.3, gate, build())
		ag.SetTurnBudget(turnBudget)
		// Enable auto-compaction. Without this the per-thread agent accumulates every turn's
		// tool output until it overflows the model context window and then fails every wake with
		// context_length_exceeded. maybeCompact is a no-op unless CtxSize is set, so seed it from
		// the model's own context window (falling back to the CLI default).
		compactCfg := agent.DefaultCompactConfig()
		compactCfg.CtxSize = defaultCtxSize
		if cw := client.Model().ContextWindow; cw > 0 {
			compactCfg.CtxSize = cw
		}
		ag.SetCompaction(compactCfg)
		return ag, build, nil
	}
	mk := func(string) bot.Runner {
		ag, build, aerr := buildAgent()
		if aerr != nil {
			return errRunner{err: fmt.Errorf("workdir %q became unusable: %w", c.Workdir, aerr)}
		}
		return bot.RefreshingRunner{Agent: ag, Build: build}
	}
	// Wire the runtime before the scheduler starts ticking: a job that fired in between would
	// find no busy gate installed and could land on top of a live turn.
	rt := bot.NewRuntime(tr, mk, 4)
	rt.SetLogger(log)
	rt.SetTurnLimiter(shared.turns)
	rt.SetTeammateCheck(shared.fleet.IsMember)
	scheduler.SetBusy(rt.Busy)
	rt.SetOnAddressed(heartbeat.Addressed)
	scheduler.SetRunner(bot.NewCronRunner(log, func() (bot.Runner, error) {
		ag, _, aerr := buildAgent()
		return ag, aerr
	}, heartbeat, rt.EnterTurn, shared.turns))

	// Join the roster under the chat username the server reports, not the aigem name. A teammate
	// addresses a chat account; registering under a name the chat server does not agree with
	// would route someone else's direct messages into this bot.
	username := tr.AuthorName(botCtx, c.Transport.BotUserID)
	if username == "" {
		log.Warn("could not resolve own chat username; teammates will address this bot by its aigem name",
			"name", name)
	}
	// Join the roster last: a teammate must not be able to deliver into a runtime
	// that is still being wired.
	shared.fleet.Register(bot.Member{Name: name, Username: username, Role: role.Name,
		UserID: c.Transport.BotUserID, Runtime: rt, Resolver: resolver})
	closers = append(closers, func() { shared.fleet.Unregister(name) })

	return &botHandle{name: name, role: role.Name, rt: rt, sched: scheduler, log: log,
		ctx: botCtx, close: closeAll}, nil
}

// openBotModel resolves and opens the bot's model. A configured model is binding:
// if it cannot be opened the bot refuses to start rather than silently running on
// whatever else is authenticated. It is trimmed first because a blank ref means
// "the default model" to Resolve, which would be exactly the silent fallback this
// is meant to prevent.
func openBotModel(name string, c bot.Config, log *slog.Logger) (*llm.Ref, error) {
	localCfg, _, _ := local.Load()
	modelReg, modelWarns := llm.NewRegistry(c.Workdir, localProvider(localCfg, defaultMaxTokens))
	warnModelsConfig(modelWarns)
	pinned := strings.TrimSpace(c.Model)
	ref := pinned
	if ref == "" {
		if def, ok := modelReg.DefaultPreferring(auth.IsAuthenticated); ok {
			ref = def.Ref()
		}
	}
	backend, _, info, err := auth.OpenModel(modelReg, ref, defaultMaxTokens)
	if err != nil {
		if pinned != "" {
			return nil, fmt.Errorf("open model %s configured for bot %q (change it with `aigem bot model %s <ref>`): %w",
				pinned, name, name, err)
		}
		return nil, fmt.Errorf("open model: %w", err)
	}
	source := "auto"
	if pinned != "" {
		source = "configured"
	}
	log.Info("model", "ref", info.Ref(), "ctx", info.ContextWindow, "source", source)
	return llm.NewRef(backend), nil
}

// botSearchConfig gives the bot its own browser profile and wires in the shared
// launch cap.
//
// Profiles stay per bot even though the bots now share a process. A profile is
// where logins live - the tester's session in the app under test is not the
// researcher's - and Chrome allows one process per profile, so a shared profile
// would put every search in the fleet in a single queue. What is shared instead
// is the cap on how many browsers run at once, which is the actual cost.
func botSearchConfig(name string, shared *fleetResources, log *slog.Logger) search.Config {
	cfg, err := search.Load()
	if err != nil {
		log.Warn("could not load search config", "err", err)
	}
	if cfg.Browser == nil {
		return cfg
	}
	parent := cfg.Browser.ProfileDir
	if parent == "" {
		if d, derr := search.DefaultBrowserProfileDir(); derr == nil {
			parent = d
		}
	}
	if parent != "" {
		cfg.Browser.ProfileDir = filepath.Join(parent, name)
	}
	cfg.Browser.Log = log
	cfg.Browser.Launches = shared.launches
	return cfg
}
