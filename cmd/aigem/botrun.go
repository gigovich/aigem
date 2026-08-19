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
	"github.com/gigovich/aigem/internal/bot/chatlink"
	"github.com/gigovich/aigem/internal/chat"
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

	// presenceWriteTimeout bounds the one store write a stopping bot makes. It
	// runs on a context the process shutdown has already cancelled, so it needs
	// a deadline of its own rather than the fleet's writer queue for however
	// long that takes.
	presenceWriteTimeout = 5 * time.Second
)

// setPresent records whether a bot is running. A failure is worth saying and not
// worth failing over: the flag decides whether a dot is drawn beside a name, and
// refusing to start a working bot over it would be the wrong trade.
func setPresent(ctx context.Context, store *chat.Store, actor string, present bool, log *slog.Logger) {
	if err := store.SetPresent(ctx, actor, present); err != nil {
		log.Warn("could not record whether the bot is running", "present", present, "err", err)
	}
}

// fleetResources are the things every bot in the process shares. They exist once
// per run and are what makes one process cheaper than one process per bot.
type fleetResources struct {
	fleet    *bot.Fleet
	turns    *bot.TurnLimiter
	launches *search.LaunchGate
	// store is the conversation store every bot reads and writes. One store,
	// not one per bot: it is a single SQLite writer, and the whole point of the
	// participants table is that one place decides who may see what.
	store *chat.Store
	// live is what the fleet screen reads for the columns the store cannot
	// answer: the model, the heartbeat, the next scheduled job, and which bots
	// are up.
	live *liveFleet
}

// botStart runs the named bots, or the whole configured fleet when no name is
// given. Every bot gets its own goroutine, transport, scheduler and agent; they
// share this process, its provider connections and its caps.
func botStart(args []string) error {
	addr, origins, names, err := chatAddrFlag(args)
	if err != nil {
		return fmt.Errorf("%w\n\nusage: aigem bot start [--addr host:port] [--origin url] [<name>...]", err)
	}
	names, err = resolveBotNames(names)
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
		live:     newLiveFleet(names),
	}
	if len(names) == 1 {
		// One bot cannot contend with anyone, and a cap it can hit on its own would
		// only slow it down.
		shared.turns = nil
	}
	slog.Info("fleet limits", "bots", len(names), "max_turns", shared.turns.Cap(),
		"max_browsers", limits.BrowserCap())

	// The conversation store and the daemon serving it come up before any bot,
	// so a bot that fails to start is still visible in a UI that is already
	// running - and so the operator can reach the fleet even when none of it
	// came up.
	chatSrv, err := startChatServer(ctx,
		chatServerOpts{addr: addr, origins: origins, names: names, live: shared.live}, slog.Default())
	if err != nil {
		return err
	}
	defer chatSrv.Close()
	shared.store = chatSrv.store

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
	// The store is where a bot's conversations are. Without one there is nothing
	// for it to read, write or be woken by, so this is a wiring mistake worth
	// naming rather than a nil dereference three frames down.
	if shared.store == nil {
		return nil, fmt.Errorf("bot %q: no conversation store", name)
	}
	client, err := openBotModel(name, c, log)
	if err != nil {
		return nil, err
	}
	logUsagePerCall(log, client)
	billUsageToTurn(client)

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
	tr := chatlink.Open(shared.store, name, log)
	closers := []func(){func() { _ = tr.Close() }}
	closeAll := func() {
		cancelBot() // stop the scheduler first, so nothing new starts while we tear down
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}

	// The tracked-thread file is gone with the transport that needed it: which
	// threads a bot follows is which threads it participates in, and that is a
	// table now.
	self := chat.BotActor(name)
	local := &bot.LocalDelivery{Self: name, SelfActor: self, Fleet: shared.fleet}

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

	// threadKey is the thread this agent belongs to, and is empty for the one the
	// scheduler builds: a cron job runs against no conversation, so there is
	// nothing for its file changes or its plan to be filed under.
	buildAgent := func(threadKey string) (*agent.Agent, func() string, error) {
		reg, rerr := tools.NewRegistry(c.Workdir)
		if rerr != nil {
			return nil, nil, rerr
		}
		// Before Subset, which copies the hook onto the child. Registering after
		// would in fact still work - a subset shares the parent's tool instances,
		// and each one calls reportFileChange on the registry it was built with -
		// but relying on that makes the wiring depend on a detail two packages
		// away. Registered here, it holds however Subset is implemented.
		if threadKey != "" {
			root := reg.Root()
			reg.OnFileChange(func(c tools.FileChange) {
				tr.FileChanged(bot.ThreadID(threadKey), chat.Artifact{
					Path: tools.RelTo(root, c.Path), Created: c.Created, Old: c.Old, New: c.New,
				})
			})
		}
		reg.Register(bot.NewMemoryTool(store))
		reg.Register(bot.NewScheduleTool(scheduler))
		reg.Register(bot.NewPostMessageTool(tr, tr, local))
		reg.Register(bot.NewHandoffTool(tr, local, name))
		reg.Register(bot.NewReadThreadsTool(tr))
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
		allowed := intersectTools(role.Allow, capProfile.Allow)
		sub := reg.Subset(allowed)
		catalog := skills.Prompt()
		build := func() string {
			idx, ierr := store.Index()
			if ierr != nil {
				log.Warn("could not read memory index", "err", ierr)
			}
			return bot.ComposeSystem(c, role, idx, catalog, extra)
		}
		ag := agent.New(paced, sub, 0.3, gate, build())
		// After the agent, because the plan tool writes into it, and gated by the
		// role like every other tool: a bot that may not plan should not be handed
		// the tool that does it.
		if slices.Contains(allowed, agent.TodoToolName) {
			sub.Register(agent.NewTodoTool(ag))
		}
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
	mk := func(threadKey string) bot.Runner {
		ag, build, aerr := buildAgent(threadKey)
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
		ag, _, aerr := buildAgent("")
		return ag, aerr
	}, heartbeat, rt.EnterTurn, shared.turns))

	// Join the roster last: a teammate must not be able to deliver into a runtime
	// that is still being wired.
	shared.fleet.Register(bot.Member{Name: name, Role: role.Name, Actor: self,
		Runtime: rt, Participation: shared.store})
	closers = append(closers, func() { shared.fleet.Unregister(name) })
	if shared.live != nil {
		shared.live.started(name, &runningBot{model: client.Model().Ref(), hb: heartbeat, sched: scheduler})
		closers = append(closers, func() { shared.live.stopped(name) })
	}
	// The durable half of the same fact. `present` is what the inbox, the
	// composer and every participant list draw a running dot from, and what
	// `aigem chat fleet` reads; the live roster above is what the fleet screen
	// adds to it. Both are written here and cleared together, so they cannot
	// disagree about whether this bot is up.
	setPresent(ctx, shared.store, self, true, log)
	closers = append(closers, func() {
		// Not ctx: on shutdown it is already cancelled, and the flag would be
		// left set for a fleet that has stopped. The next start clears it
		// anyway, but only after however long the machine was off.
		off, cancel := context.WithTimeout(context.WithoutCancel(ctx), presenceWriteTimeout)
		defer cancel()
		setPresent(off, shared.store, self, false, log)
	})

	return &botHandle{name: name, role: role.Name, rt: rt, sched: scheduler, log: log,
		ctx: botCtx, close: closeAll}, nil
}

// billUsageToTurn charges each model call to the thread it was made for, so a
// thread can say what the work in it cost rather than only that it happened.
//
// The attribution has to be per call. One client serves every thread the bot is
// working on at once, so a total sampled around a turn would be smeared by
// whatever else was in flight; the context of the call is what carries the turn,
// and a call made outside one - a heartbeat, a scheduled job with no thread -
// finds no sink and is accounted for in the log alone, as before.
func billUsageToTurn(client *llm.Ref) {
	rep, ok := llm.UsageOf(client)
	if !ok {
		return
	}
	rep.OnCallCtx(func(ctx context.Context, u llm.Usage, _ llm.UsageReport) {
		if spend := bot.UsageFrom(ctx); spend != nil {
			spend(u, client.Model().Ref())
		}
	})
}

// openBotModel opens exactly the configured override or role default. Selection
// is binding: provider order and authentication state must never silently move a
// bot to a different model.
func openBotModel(name string, c bot.Config, log *slog.Logger) (*llm.Ref, error) {
	selection, err := c.ModelSelection()
	if err != nil {
		return nil, fmt.Errorf("select model for bot %q (role %q): %w", name, c.Role, err)
	}
	localCfg, _, _ := local.Load()
	modelReg, modelWarns := llm.NewRegistry(c.Workdir, localProvider(localCfg, defaultMaxTokens))
	warnModelsConfig(modelWarns)
	backend, _, info, err := auth.OpenModel(modelReg, selection.Effective, defaultMaxTokens)
	if err != nil {
		return nil, fmt.Errorf("bot %q (role %q) requires model %q from %s; set an override with `aigem bot model %s <ref>`: %w",
			name, c.Role, selection.Effective, selection.Source, name, err)
	}
	log.Info("model", "ref", info.Ref(), "ctx", info.ContextWindow, "source", selection.Source)
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
