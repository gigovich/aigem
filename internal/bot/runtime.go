package bot

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
)

// isNonActionableErr reports whether a run error is one a chat participant cannot act on - a model
// context-window overflow or a malformed-request error from the provider. These are surfaced in the
// logs but not posted into the thread, where they would only invite an error cascade.
func isNonActionableErr(err error) bool {
	return llm.IsRequestShapeErr(err)
}

// Runner is the subset of *agent.Agent the runtime drives. *agent.Agent satisfies it.
type Runner interface {
	Run(ctx context.Context, input string, ev agent.Events) (string, error)
}

// ImageRunner is a Runner that can additionally take image attachments with the
// turn input. *agent.Agent satisfies it.
type ImageRunner interface {
	RunWithImages(ctx context.Context, input string, images []llm.Image, ev agent.Events) (string, error)
}

// AgentFactory builds (or returns a cached) Runner for a given thread key.
type AgentFactory func(threadKey string) Runner

// Runtime routes inbound chat messages to per-thread agents and posts their answers back.
type Runtime struct {
	transport Transport
	mk        AgentFactory
	sem       chan struct{}
	log       *slog.Logger
	// turnCap bounds concurrent turns across every bot in the process (not to be confused with
	// Fleet, the roster). Nil means no fleet-wide cap, which is what a lone bot wants.
	turnCap *TurnLimiter
	// enqueued carries messages delivered in-process by a teammate rather than by
	// the chat transport. Buffered so a delivering bot is never blocked by a
	// receiving bot that is mid-turn.
	enqueued chan Inbound
	// stopped closes when Serve returns, so a delivery is refused instead of being
	// queued into a runtime that has stopped reading.
	stopped chan struct{}
	// resumeDelay is how long an automatic continuation waits before re-running
	// the thread after a budget stop or a transient provider failure. Set before
	// Serve; tests shorten it.
	resumeDelay time.Duration

	mu       sync.Mutex
	threads  map[string]*threadState
	inFlight int // turns currently running; the cron busy-gate reads this
	// seenMsgs remembers which messages have already been routed, so one that
	// arrives both from a teammate in this process and from the store's own
	// publisher is acted on once. seenOrder bounds it.
	seenMsgs  map[uint64]bool
	seenOrder []uint64

	// onAddressed, when set, is called whenever a human or teammate addresses the bot
	// directly (mention or DM). The heartbeat uses it to drop back to its active interval.
	onAddressed func()
	// isTeammate reports whether an author is another bot in this process. Set before Serve.
	isTeammate func(userID string) bool
}

type threadState struct {
	runner Runner
	lock   chan struct{} // capacity-1: single-flight per thread

	mu      sync.Mutex
	pending *Inbound  // latest coalesced thread_update awaiting its turn
	resumes int       // consecutive automatic resumes (budget stop / transient failure)
	touched time.Time // last time a turn was routed here, for eviction
}

// maxThreads bounds how many conversations a bot keeps an agent for. Each one holds that
// thread's whole message history, compacted only down to the model's context window, so the map
// is the largest thing a long-lived bot accumulates. Evicting the coldest idle thread costs it
// its in-context history - it rebuilds from memory and the thread itself on the next message, the
// same as after a restart - which is much cheaper than the alternative now that every bot's
// agents live in one heap and one OOM takes the whole team down.
const maxThreads = 200

// SetTeammateCheck installs the test for whether an author is another bot in this process. Set
// before Serve.
func (r *Runtime) SetTeammateCheck(fn func(userID string) bool) {
	r.mu.Lock()
	r.isTeammate = fn
	r.mu.Unlock()
}

// noteAddressed records that someone addressed the bot: it restores the auto-resume allowance, so
// a thread whose resumes were exhausted gets its full chain back on the ping the stalled note asks
// for - even if the turn it lands in budget-stops again - and it drops the heartbeat back to its
// active interval.
//
// Only a person refills the allowance. maxAutoResumes is the only bound on a thread continuing
// itself, and two bots handing off to each other in one thread would otherwise reset each other's
// counter faster than it can run out, leaving nothing to stop the pair.
func (r *Runtime) noteAddressed(st *threadState, in Inbound) {
	r.mu.Lock()
	notify, teammate := r.onAddressed, r.isTeammate
	r.mu.Unlock()
	if teammate == nil || !teammate(in.Author) {
		st.mu.Lock()
		st.resumes = 0
		st.mu.Unlock()
	}
	if notify != nil {
		notify()
	}
}

// authorName resolves an inbound's author id to a name when the transport can,
// falling back to the empty string rather than showing the model a raw id.
func (r *Runtime) authorName(ctx context.Context, in Inbound) string {
	namer, ok := r.transport.(AuthorNamer)
	if !ok {
		return ""
	}
	return namer.AuthorName(ctx, in.Author)
}

// setPending records (or replaces) the thread's coalesced update. A
// thread_update carries no text of its own - the turn re-reads the whole
// thread - so only the latest one matters.
func (st *threadState) setPending(in Inbound) {
	st.mu.Lock()
	st.pending = &in
	st.mu.Unlock()
}

func (st *threadState) takePending() (Inbound, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pending == nil {
		return Inbound{}, false
	}
	in := *st.pending
	st.pending = nil
	return in, true
}

func (st *threadState) hasPending() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.pending != nil
}

// NewRuntime builds a runtime over a transport, an agent factory, and a concurrency cap.
func NewRuntime(t Transport, mk AgentFactory, workers int) *Runtime {
	if workers < 1 {
		workers = 1
	}
	return &Runtime{
		transport:   t,
		mk:          mk,
		sem:         make(chan struct{}, workers),
		log:         slog.Default(),
		enqueued:    make(chan Inbound, 16),
		stopped:     make(chan struct{}),
		resumeDelay: defaultResumeDelay,
		threads:     map[string]*threadState{},
		seenMsgs:    map[uint64]bool{},
	}
}

// SetLogger installs the logger every turn logs through. Bots share one process,
// so without a logger carrying the bot's name their lines are indistinguishable.
// Set before Serve.
func (r *Runtime) SetLogger(l *slog.Logger) {
	if l == nil {
		return
	}
	r.mu.Lock()
	r.log = l
	r.mu.Unlock()
}

// SetTurnLimiter installs the fleet-wide turn cap. Set before Serve.
func (r *Runtime) SetTurnLimiter(l *TurnLimiter) {
	r.mu.Lock()
	r.turnCap = l
	r.mu.Unlock()
}

// Enqueue routes a message delivered in-process by a teammate. It reports whether the message
// was accepted; a full queue or a stopped runtime returns false, and the caller's chat post is
// then the only path - the same path it had before.
//
// It never blocks: a teammate handing off must not wait on the receiver's turn.
func (r *Runtime) Enqueue(in Inbound) bool {
	select {
	case <-r.stopped:
		return false
	default:
	}
	select {
	case r.enqueued <- in:
		return true
	default:
		return false
	}
}

// maxSeenPosts bounds the routed-post memory. It only has to outlive the gap
// between a fleet delivery and the same post arriving over the websocket, which
// is milliseconds; the cap exists so a long-lived bot does not grow the map
// forever.
const maxSeenMsgs = 512

// alreadyRouted reports whether this message was routed before, recording it
// when it was not. Anything with no message behind it - a synthetic resume, a
// thread update - is never deduplicated: it has no identity to compare.
func (r *Runtime) alreadyRouted(seq uint64) bool {
	if seq == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seenMsgs[seq] {
		return true
	}
	r.seenMsgs[seq] = true
	r.seenOrder = append(r.seenOrder, seq)
	if len(r.seenOrder) > maxSeenMsgs {
		delete(r.seenMsgs, r.seenOrder[0])
		r.seenOrder = r.seenOrder[1:]
	}
	return false
}

// logger returns the logger to use from a goroutine that does not hold r.mu.
func (r *Runtime) logger() *slog.Logger {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.log
}

// Busy reports whether any agent work is in flight. Wire it into Scheduler.SetBusy so a scheduled
// job never starts a second agent on top of work that is already running.
func (r *Runtime) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlight > 0
}

// SetOnAddressed installs a callback invoked when a mention or DM arrives. Set before Serve.
func (r *Runtime) SetOnAddressed(fn func()) {
	r.mu.Lock()
	r.onAddressed = fn
	r.mu.Unlock()
}

// EnterTurn marks agent work as running and returns the func that marks it finished. Scheduled
// runs must wrap themselves in it too: they build their own agent outside this runtime, so without
// it the busy gate would only see chat turns and a long scheduled run could still be joined by
// the next one.
func (r *Runtime) EnterTurn() func() {
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
	}
}

// threadKey is the per-conversation key. A thread is now the whole of it: with
// no channel above one, the id is the key.
func threadKey(in Inbound) string { return string(in.Thread) }

// state returns the thread's state, creating it on first sight. Building the agent reads the
// workdir, the skills directory and the memory index, so it happens with r.mu released: Busy()
// takes that lock, and a teammate asking the roster who is free must not queue behind one bot's
// disk. Two goroutines racing to open the same thread build two agents and one is discarded,
// which is cheaper than holding the lock across the I/O.
func (r *Runtime) state(key string) *threadState {
	now := time.Now()
	r.mu.Lock()
	st := r.threads[key]
	if st != nil {
		st.touched = now
	}
	r.mu.Unlock()
	if st != nil {
		return st
	}
	built := &threadState{runner: r.mk(key), lock: make(chan struct{}, 1), touched: now}
	r.mu.Lock()
	defer r.mu.Unlock()
	if st = r.threads[key]; st != nil {
		st.touched = now
		return st // another goroutine won the race; its agent is the thread's
	}
	r.threads[key] = built
	r.evictColdestLocked()
	return built
}

// evictColdestLocked drops the least recently used idle thread once the map is over its cap. A
// thread whose lock is held is running or queued and is never evicted; neither is one holding a
// pending update, which would otherwise be silently dropped. Caller holds r.mu.
func (r *Runtime) evictColdestLocked() {
	for len(r.threads) > maxThreads {
		oldest, found := "", false
		var at time.Time
		for k, st := range r.threads {
			if len(st.lock) > 0 || st.hasPending() {
				continue // busy or owed a turn
			}
			if !found || st.touched.Before(at) {
				oldest, at, found = k, st.touched, true
			}
		}
		if !found {
			return // every thread is busy; the cap gives way rather than losing work
		}
		delete(r.threads, oldest)
	}
}

// Serve consumes inbound events until the transport's channel closes or ctx is done.
func (r *Runtime) Serve(ctx context.Context) error {
	defer close(r.stopped)
	var wg sync.WaitGroup
	events := r.transport.Events()
	dispatch := func(in Inbound) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.handle(ctx, in)
		}()
	}
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case in, ok := <-events:
			if !ok {
				wg.Wait()
				return nil
			}
			dispatch(in)
		case in := <-r.enqueued:
			dispatch(in)
		}
	}
}

// buildInput turns an inbound into the agent's prompt. A thread_update carries no text of its
// own: it is a nudge that the thread the bot owns or is in has new replies, so the agent is
// handed the full thread and asked to decide whether to act. A resume carries its self-contained
// continuation instruction in Text. An addressed message gets its text, prefixed with the thread
// it arrived in when there is one, and otherwise with recent unaddressed channel chatter.
func (r *Runtime) buildInput(ctx context.Context, in Inbound) string {
	switch in.Kind {
	case "thread_update":
		if th := r.threadContext(ctx, in); th != "" {
			return threadUpdatePreamble + th
		}
		return threadUpdatePreamble
	case "resume":
		return in.Text
	}
	// A message addressed inside a thread needs that thread, not just its own text. Without this a
	// bare "@bot" in a thread the bot itself started reads as a greeting out of nowhere - and the
	// thread's own agent may never have seen the thread, because a scheduled run posts from a fresh
	// agent and a restart drops per-thread history entirely.
	if th := r.threadContext(ctx, in); th != "" {
		return threadAddressedPreamble + th + "\n\n---\n" + in.Text
	}
	return in.Text
}

// threadContext returns the thread the inbound belongs to, or "" when there is none, when the
// transport cannot read it, or when the thread is just this one message (a mention that opened its
// own thread), where repeating it would only duplicate the text.
func (r *Runtime) threadContext(ctx context.Context, in Inbound) string {
	if in.Thread == "" {
		return ""
	}
	tr, ok := r.transport.(ThreadHistoryReader)
	if !ok {
		return ""
	}
	th := tr.ThreadHistory(ctx, in.Thread)
	if strings.Count(strings.TrimSpace(th), "\n") == 0 {
		return ""
	}
	return th
}

const threadUpdatePreamble = "New replies landed in a thread you are in. Read the whole thread " +
	"and decide what to do, if anything: answer, save to memory, hand the work over, or say " +
	"nothing. Answer only if you have something to add. If you already answered the substance, or " +
	"there is nothing to add, reply with exactly NO_REPLY - that reply is posted nowhere. Do not " +
	"repeat or reword what the thread already says, and never post a message whose only content " +
	"is that you replied somewhere else.\n\n" +
	"Thread:\n"

// threadAddressedPreamble prefixes the thread an addressed message arrived in. The message itself
// follows after the separator, so the agent answers the question in the context that produced it -
// including a question that is an answer to something this bot asked earlier.
const threadAddressedPreamble = "Someone wrote to you in a thread. The whole thread is below; " +
	"the message addressed to you follows the separator. Read the thread first - the message may " +
	"be the answer to a question you asked earlier, in which case answer that question rather " +
	"than greeting them as if this were a new conversation.\n\nThread:\n"

// CronEvents returns the same step logging a chat turn gets, tagged with a job id instead of a
// thread key, so a timer-driven run is as visible in the log as an inbound one.
func CronEvents(log *slog.Logger, jobID string) agent.Events {
	return stepEvents(log, "cron:"+jobID)
}

// logEvents returns agent.Events that log each meaningful step (not content/reasoning
// deltas) through slog, tagged with the thread key.
func (r *Runtime) logEvents(in Inbound) agent.Events {
	return stepEvents(r.logger(), threadKey(in))
}

func stepEvents(log *slog.Logger, key string) agent.Events {
	if log == nil {
		log = slog.Default()
	}
	return agent.Events{
		OnToolStart: func(_, name string, _ json.RawMessage) {
			log.Info("tool start", "thread", key, "tool", name)
		},
		OnToolEnd: func(_, name, _ string, err error) {
			if err != nil {
				log.Error("tool end", "thread", key, "tool", name, "err", err)
				return
			}
			log.Info("tool end", "thread", key, "tool", name, "ok", true)
		},
		OnNotice: func(text string) {
			log.Info("notice", "thread", key, "text", text)
		},
		OnAssistantMessage: func(content string) {
			log.Info("assistant step", "thread", key, "chars", len(content))
		},
		OnAgentStart: func(id, agentName, _ string) {
			log.Info("subagent start", "thread", key, "id", id, "agent", agentName)
		},
		OnAgentEnd: func(id, _ string, err error) {
			if err != nil {
				log.Error("subagent end", "thread", key, "id", id, "err", err)
				return
			}
			log.Info("subagent end", "thread", key, "id", id)
		},
	}
}

// handle routes one inbound to its thread. A thread_update is coalesced: it is
// parked as the thread's pending update and run when the single-flight lock is
// free, so a burst of updates (or updates landing while a turn is running)
// becomes one follow-up turn over a fresh thread snapshot instead of a queue of
// near-identical turns. Addressed messages (mention/dm/broadcast/resume) carry
// unique text, so they wait for the lock and run each.
func (r *Runtime) handle(ctx context.Context, in Inbound) {
	// Contain a panic here, in the goroutine that can actually recover it. Every turn runs on its
	// own goroutine, and bots now share a process: an unrecovered panic in one bot's turn would
	// take the whole team down, where before it cost one process out of several. Turns themselves
	// recover in runLocked, which owns the thread lock; this is the backstop for the routing code.
	defer func() {
		if p := recover(); p != nil {
			r.logger().Error("turn panicked", "thread", threadKey(in), "kind", in.Kind,
				"panic", p, "stack", string(debug.Stack()))
		}
	}()
	// A teammate's message reaches this bot twice - once in-process, once when the
	// chat server pushes the post back - and the copies race. Whichever lands
	// first is the one acted on.
	if r.alreadyRouted(in.MessageSeq) {
		return
	}
	key := threadKey(in)
	st := r.state(key)
	if in.Kind == "thread_update" {
		st.setPending(in)
		r.pump(ctx, key, st)
		return
	}
	// A message addressed to the bot while it is working goes into that turn
	// rather than behind it, and the model decides what it means.
	if in.Kind == "mention" {
		if inj, ok := st.runner.(Injector); ok && inj.Inject(midTurnDelivery(r.authorName(ctx, in), in.Text)) {
			r.logger().Info("delivered into the running turn", "thread", key, "author", in.Author)
			r.noteAddressed(st, in)
			return
		}
	}
	select {
	case st.lock <- struct{}{}:
	case <-ctx.Done():
		return
	}
	r.runLocked(ctx, key, st, in)
	r.pump(ctx, key, st)
}

// runLocked runs one turn under the lock the caller took and always releases it.
// A panicking turn is contained here rather than by handle's backstop: unwinding
// past the release would leave the lock held forever, wedging the conversation
// and pinning a thread that evictColdestLocked then refuses to reclaim.
func (r *Runtime) runLocked(ctx context.Context, key string, st *threadState, in Inbound) {
	defer func() { <-st.lock }()
	defer func() {
		if p := recover(); p != nil {
			r.logger().Error("turn panicked", "thread", key, "kind", in.Kind,
				"panic", p, "stack", string(debug.Stack()))
		}
	}()
	r.runTurn(ctx, key, st, in)
}

// pump runs the thread's pending coalesced update whenever the lock is free.
// Every lock holder calls it after releasing, which closes the race where an
// update is parked between the holder's last pending check and its release.
func (r *Runtime) pump(ctx context.Context, key string, st *threadState) {
	for ctx.Err() == nil {
		select {
		case st.lock <- struct{}{}:
		default:
			return // busy: the current holder pumps again after it releases
		}
		in, ok := st.takePending()
		if !ok {
			<-st.lock
			// An update parked between the empty takePending and the release above
			// found the lock busy and trusted this holder to run it - loop and
			// re-check so it is not stranded until the next inbound event.
			if !st.hasPending() {
				return
			}
			continue
		}
		r.runLocked(ctx, key, st, in)
	}
}

// noReplySentinel is the exact reply a bot gives to explicitly stay silent; the
// runtime drops it instead of posting. Advertised in the operating protocol and
// the thread_update preamble, it gives the model a concrete action for "say
// nothing" - otherwise it posts filler acknowledgements like "staying silent".
const noReplySentinel = "NO_REPLY"

// isNoReply reports whether the answer is the silence sentinel, tolerating the
// markdown wrapping models add around bare tokens (backticks, bold, quotes).
// isNoReply also swallows the heartbeat's idle marker. A chat turn is never asked for it, but the
// marker is described in the always-on protocol, so a turn that reaches for the wrong sentinel
// should fall silent rather than post the bare word into the channel.
func isNoReply(answer string) bool {
	s := strings.TrimSpace(answer)
	s = strings.Trim(s, "`*_\"'.")
	return strings.EqualFold(s, noReplySentinel) || strings.EqualFold(s, HeartbeatIdleMarker)
}

// Automatic continuation after an interrupted turn: how long to wait before
// re-running the thread, and how many consecutive automatic turns are allowed
// before the bot goes quiet and waits for a human ping.
const (
	defaultResumeDelay = 2 * time.Minute
	maxAutoResumes     = 3
)

const budgetResumeNote = "_Hit the work limit mid-turn; I will pick this up again in a couple of minutes._"

const budgetStalledNote = "_Hit the work limit again and I am out of automatic continuations - ping me to carry on._"

func budgetResumeInput(reason string) string {
	return "Your previous turn in this thread stopped at a work limit (" + reason + "). Carry on " +
		"from where you left off: take the next concrete step and report the result here. If the " +
		"work is already finished, reply with exactly NO_REPLY."
}

const transientResumeInput = "Your previous answer was never produced: the model provider failed " +
	"briefly. Answer the last message addressed to you in this thread now. If that answer is no " +
	"longer needed, reply with exactly NO_REPLY."

func (r *Runtime) runTurn(ctx context.Context, key string, st *threadState, in Inbound) {
	// Count the turn as in flight before queueing for the semaphore, not after: a turn waiting
	// its slot is work about to happen, and a scheduled job started in that window would land on
	// top of it.
	done := r.EnterTurn()
	defer done()
	select {
	case r.sem <- struct{}{}: // bound this bot's own concurrency
	case <-ctx.Done():
		return
	}
	defer func() { <-r.sem }()

	log := r.logger()
	// Then queue for the fleet's cap, which is what bounds the whole process.
	r.mu.Lock()
	cap := r.turnCap
	r.mu.Unlock()
	release, err := cap.Acquire(ctx)
	if err != nil {
		return // shutdown while waiting for a slot
	}
	defer release()

	log.Info("inbound", "thread", key, "kind", in.Kind, "author", in.Author)

	if in.Kind == "mention" {
		r.noteAddressed(st, in)
	}

	input := r.buildInput(ctx, in)
	images := r.fetchAttachments(ctx, in, &input)

	budgetReason := ""
	ev := r.logEvents(in)
	ev.OnBudgetExhausted = func(reason string) { budgetReason = reason }

	answer, err := r.run(ctx, st.runner, input, images, ev)
	if err != nil {
		r.handleRunErr(ctx, key, st, in, err)
		return
	}
	if isNoReply(answer) {
		log.Info("silent", "thread", key, "kind", in.Kind)
		answer = ""
	}
	// Budget notes ride on an answer the agent chose to post, or go to a thread
	// where a human addressed the bot; a proactive turn with nothing to say must
	// not post a bare runtime-status line (the other bots observe it).
	noteworthy := answer != "" || in.Kind == "mention"
	switch {
	case budgetReason == "":
		// Only a clean human-addressed or resume turn ends the automatic chain.
		// A clean thread_update/broadcast in between (e.g. another bot's reply
		// answered with NO_REPLY) must not reset the cap, or periodic chatter in
		// the thread would let the bot self-continue indefinitely.
		if in.Kind != "thread_update" {
			st.mu.Lock()
			st.resumes = 0
			st.mu.Unlock()
		}
	case r.scheduleResume(ctx, st, in, budgetResumeInput(budgetReason)):
		log.Warn("budget stop; auto-resume scheduled", "thread", key, "reason", budgetReason)
		if noteworthy {
			answer = appendNote(answer, budgetResumeNote)
		}
	default:
		log.Warn("budget stop; auto-resumes exhausted", "thread", key, "reason", budgetReason)
		if noteworthy {
			answer = appendNote(answer, budgetStalledNote)
		}
	}
	if answer == "" {
		return
	}
	log.Info("reply", "thread", key, "chars", len(answer))
	if err := r.transport.Reply(in.Thread, answer); err != nil {
		log.Error("reply failed", "thread", key, "chars", len(answer), "err", err)
	}
}

// run dispatches to RunWithImages when the turn carries images and the runner
// supports them; otherwise the images stay represented only by the attachment
// note already appended to the input.
func (r *Runtime) run(ctx context.Context, runner Runner, input string, images []llm.Image,
	ev agent.Events) (string, error) {
	if len(images) > 0 {
		if ir, ok := runner.(ImageRunner); ok {
			return ir.RunWithImages(ctx, input, images, ev)
		}
	}
	return runner.Run(ctx, input, ev)
}

// fetchAttachments downloads the inbound's file attachments when the transport
// can, appending a description of every attachment to the input so the model
// knows what arrived even when it cannot view it.
func (r *Runtime) fetchAttachments(ctx context.Context, in Inbound, input *string) []llm.Image {
	if len(in.AttachmentIDs) == 0 {
		return nil
	}
	af, ok := r.transport.(AttachmentFetcher)
	if !ok {
		return nil
	}
	images, note := af.Attachments(ctx, in.AttachmentIDs)
	if note != "" {
		*input += "\n\n" + note
	}
	return images
}

// handleRunErr reports a failed run without echoing raw provider dumps into the
// thread (which is what set off cross-bot error cascades). Transient provider
// failures schedule the same automatic resume as a budget stop, so the bot
// answers when the provider recovers instead of dropping the request.
func (r *Runtime) handleRunErr(ctx context.Context, key string, st *threadState, in Inbound, err error) {
	if ctx.Err() != nil {
		return // shutdown or turn cancellation, not a reportable failure
	}
	r.logger().Error("run failed", "thread", key, "kind", in.Kind, "err", err)
	transient := llm.IsTransientErr(err)
	resuming := transient && r.scheduleResume(ctx, st, in, transientResumeInput)
	// Never echo the failure into the thread for our own proactive observation
	// (thread_update / broadcast / resume): the other bots observe such posts and it
	// sets off a reply cascade. For a message a human addressed to us directly,
	// answer once with a short, non-technical notice; the full error is in the logs.
	if in.Kind != "mention" {
		return
	}
	var msg string
	switch {
	case llm.IsAuthErr(err):
		msg = "I cannot authenticate with the model provider - the access token is no longer valid, " +
			"either expired or revoked. An operator needs to log in again (aigem login) and restart me."
	case isNonActionableErr(err):
		// Request-shape 400s are mostly oversized context, but not only that
		// (e.g. a parameter the model rejects), so echo the provider's reason
		// instead of asserting a cause that may be wrong.
		msg = "The provider rejected the request (" + errSummary(err) + "). If the size is what it " +
			"objects to, trim what you sent or carry on in a new thread."
	case resuming:
		msg = "The model provider is unavailable right now (" + errSummary(err) + "). I will try " +
			"again automatically in a couple of minutes."
	case transient:
		msg = "The model provider is unavailable right now (" + errSummary(err) + ") and I am out " +
			"of automatic retries. Ping me later and I will answer."
	default:
		msg = "I could not handle that request: " + errSummary(err) + ". The details are in my logs."
	}
	_ = r.transport.Reply(in.Thread, msg)
}

// errSummary reduces a provider error to a short class a chat message can carry:
// the first line, cut before any JSON payload, hard-capped. The full error goes
// to the logs; the thread gets enough to know what class of failure happened.
func errSummary(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '{'); i >= 0 {
		s = strings.TrimSpace(strings.TrimRight(s[:i], ": "))
	}
	const max = 160
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// scheduleResume queues an automatic follow-up turn for the thread after a
// budget stop or a transient provider failure, so interrupted work continues
// without a human re-ping. Consecutive automatic turns are capped; any turn
// that completes normally resets the counter.
func (r *Runtime) scheduleResume(ctx context.Context, st *threadState, in Inbound, prompt string) bool {
	st.mu.Lock()
	if st.resumes >= maxAutoResumes {
		st.mu.Unlock()
		return false
	}
	st.resumes++
	st.mu.Unlock()
	resume := Inbound{Kind: "resume", Thread: in.Thread, Author: in.Author, Text: prompt}
	delay := r.resumeDelay
	// The resume goroutine is not tracked by Serve's WaitGroup: it exits via ctx
	// on shutdown, and in the events-channel-close path (transport death) a late
	// handle only fails its Reply against the dead transport.
	go func() {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.handle(ctx, resume)
	}()
	return true
}

// appendNote appends a runtime status note to an answer, or returns the note
// alone when the answer is empty (the note is the only thing worth posting).
func appendNote(answer, note string) string {
	if answer == "" {
		return note
	}
	return answer + "\n\n" + note
}
