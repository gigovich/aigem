import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { ArrowLeft, Bell, KeyRound, PanelRight, WifiOff } from "lucide-react";
import { api } from "@/lib/protocol";
import { countsOf, useChat } from "@/lib/chat";
import {
  OPERATOR,
  type ChatMeta,
  type FleetMember,
  type Message,
  type Spend as ThreadSpend,
  type ThreadView,
  type Turn,
} from "@/lib/chatprotocol";
import { PANEL_DOCKS, RAIL_DOCKS, useMedia } from "@/lib/media";
import { useTraces } from "@/lib/trace";
import { cn } from "@/lib/utils";
import { useFleetScreen, useThread } from "@/lib/route";
import { useNotifications, useTitleBadge } from "@/lib/notify";
import { useWebPush } from "@/lib/push";
import { Fatal } from "@/components/fatal";
import { Login } from "@/components/login";
import { Spend } from "@/components/usage";
import { SidePanel, type PanelLayout } from "@/components/panel";
import { DiffView, threadArtifacts, type Artifact } from "@/components/files";
import { Badge, Button, RunDot } from "@/components/ui";
import { Inbox } from "./Inbox";
import { ThreadPane } from "./ThreadView";
import { Composer } from "./Composer";
import { AgentTrace } from "./AgentTrace";
import { Participants } from "./Participants";
import { ThreadPanel } from "./ThreadPanel";
import { Fleet } from "./Fleet";
import { DeleteThreadControl } from "./DeleteThreadDialog";

/** The fallback limits, used only for the moment before the daemon answers.
 *  They are the daemon's own numbers, not guesses, and they never outlive the
 *  first response - see the note on /api/chat/meta for why the client must not
 *  be the authority on them. */
const PROVISIONAL: ChatMeta = {
  operator: OPERATOR,
  states: ["needs_you", "working", "waiting", "idle"],
  max_body_bytes: 256 << 10,
  max_title_chars: 200,
  max_unread: 99,
  max_attachment_bytes: 3 << 20,
  max_attachments: 8,
  inline_image_types: ["image/png", "image/jpeg", "image/gif", "image/webp"],
};

/** How often the roster is re-read. Slow on purpose: it changes when a bot is
 *  started or stopped, which is an operator action minutes apart, not something
 *  that happens inside a turn. */
const FLEET_POLL_MS = 30_000;

/** How long file changes are gathered before the panel re-reads them. A bot
 *  rewriting a tree emits one event per file, and each refetch is a list whose
 *  rows carry file contents on both sides. */
const FILE_COALESCE_MS = 1_000;

export function ChatApp({ modeSwitch }: { modeSwitch?: ReactNode }) {
  const [notice, setNotice] = useState<string | null>(null);
  // The open thread lives in the URL, so the back button leaves a thread
  // instead of leaving the app, and a thread is a link that can be sent.
  const { thread: active, open: openThread, close: closeThread } = useThread();
  // The roster is a screen over the inbox, like a thread: reachable by link,
  // left by the back button, and never both open at once - the URL is one or
  // the other.
  const { fleet: onFleet, open: openFleet, close: closeFleet } = useFleetScreen();
  // The agent timeline is held apart from the conversation, and only for the
  // thread on screen. These are the highest-volume frames on the wire, and
  // nothing the inbox draws comes from one.
  const traces = useTraces(active, setNotice);
  const { live: liveEvent, ended: traceEnded, resumed: traceResumed } = traces;
  // Turn frames are what the summary lines and the panel are drawn from, and
  // the row's counters only settle when a run ends - so the end of one is what
  // re-reads them, along with what the thread has now spent.
  const [turns, setTurns] = useState<Turn[]>([]);
  // What to ask for the next page of runs, and whether there is one. Held so
  // paging the transcript back can page the runs with it: a message draws a
  // trace only if its run's row is on hand.
  const [turnCursor, setTurnCursor] = useState(0);
  const [spend, setSpend] = useState<ThreadSpend | null>(null);
  const [panelLoaded, setPanelLoaded] = useState(false);
  const [panelError, setPanelError] = useState<string | null>(null);
  // Bumped when a watched thread writes a file. It is both the version an open
  // diff refetches at and the key the run rows are re-read on, because a file
  // landing moves both and nothing else moves either.
  const [fileEvents, setFileEvents] = useState(0);
  // The pending coalesce timer for file changes, if any.
  const filesDue = useRef(0);
  useEffect(
    () => () => {
      if (filesDue.current) window.clearTimeout(filesDue.current);
    },
    [],
  );

  const onEvent = useCallback(
    (thread: string, turn: number, ev: { kind: string }) => {
      if (thread !== active) return;
      liveEvent(turn, ev as never);
      // Nothing is done with turn_end here. Dropping the buffered events on it
      // was a visible flicker: the row this trace would fall back to was last
      // read at turn start, when its counters were zero, so the summary went
      // "14 steps · 6 tools" -> "working" -> back again, once per answer. The
      // events are dropped on the `working` transition instead, which is the
      // same signal that re-reads the row.
      // A file written moves both: the diff list, and the run rows the panel
      // picks its run from - without the second, the panel stays pinned to the
      // last finished run for the whole of the one being watched, refreshing
      // its files pointlessly on every write of a run it is not showing.
      //
      // Coalesced, because a bot rewriting a tree emits one of these per file
      // and each is a fetch of a list whose rows carry file contents. The panel
      // catching up a second late is not a fact anyone is reading that closely.
      if (ev.kind === "file_changed") {
        if (filesDue.current) return;
        filesDue.current = window.setTimeout(() => {
          filesDue.current = 0;
          setFileEvents((n) => n + 1);
        }, FILE_COALESCE_MS);
      }
    },
    [active, liveEvent],
  );

  // A refusal of one of this client's ops is the answer to something the
  // operator just did, so it goes where they are looking rather than nowhere.
  const {
    state, refresh, archived, open, older, upload, say, markRead, create, deleteThread, alerted,
    turns: fetchTurns, spend: fetchSpend, addActor, removeActor,
  } = useChat(setNotice, onEvent, traceResumed);
  const [meta, setMeta] = useState<ChatMeta | null>(null);
  const [fleet, setFleet] = useState<FleetMember[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [diff, setDiff] = useState<Artifact | null>(null);
  // Two kinds of failure, and only one of them is the end of the screen. The
  // page cannot start without its meta and its inbox; a thread that failed to
  // open is one request, and taking the whole workspace down for it loses the
  // live socket, the inbox and every other thread over a blip the socket layer
  // would have survived on its own.
  const [fatal, setFatal] = useState<string | null>(null);
  const [login, setLogin] = useState(false);
  const railDocks = useMedia(RAIL_DOCKS);
  const panelDocks = useMedia(PANEL_DOCKS);
  // On a phone the panel rises from the bottom edge, where the thumb already
  // is; at the middle width it is a drawer over the thread, and only a genuinely
  // wide window gets the third standing column.
  const panelLayout: PanelLayout = panelDocks ? "docked" : railDocks ? "drawer" : "sheet";
  const [panel, setPanel] = useState(panelDocks);

  // Adjusted during render rather than in an effect, which is how React wants
  // state that follows a prop: an effect would paint the wrong layout first.
  const [lastDocks, setLastDocks] = useState(panelDocks);
  if (lastDocks !== panelDocks) {
    // Crossing the breakpoint changes what the panel *is*, so it also decides
    // whether one should be showing: a column by default, a drawer only on ask.
    setLastDocks(panelDocks);
    setPanel(panelDocks);
  }

  const thread = active ? state.threads[active]?.view : undefined;
  const working = thread?.working;
  const counts = countsOf(state);
  const limits = meta ?? PROVISIONAL;

  useEffect(() => {
    (async () => {
      try {
        const [m, f] = await Promise.all([
          api<ChatMeta>("/api/chat/meta"),
          api<FleetMember[]>("/api/chat/fleet"),
        ]);
        setMeta(m);
        setFleet(f);
        await refresh();
        setLoaded(true);
      } catch (e) {
        setFatal(e instanceof Error ? e.message : String(e));
      }
    })();
  }, [refresh]);

  // A bot starting or stopping is not something the conversation stream can
  // report - `present` is a property of the process, and no frame carries it -
  // so the roster is polled. It was keyed on the stream's cursor once: that is
  // one request per frame, and a single bot mid-turn produces hundreds a
  // minute, each one an actors scan and a re-render of the rail.
  useEffect(() => {
    if (!loaded) return;
    const poll = () => {
      api<FleetMember[]>("/api/chat/fleet").then(setFleet).catch(() => {});
    };
    // Once on arrival as well as on the interval: opening the roster is the one
    // moment someone is reading it, and a screen entered a tick after the last
    // poll would otherwise open half a minute out of date.
    if (onFleet) poll();
    const timer = window.setInterval(poll, FLEET_POLL_MS);
    return () => window.clearInterval(timer);
  }, [loaded, onFleet]);

  const titleOf = useCallback(
    (id: string) => state.threads[id]?.view.title || "a thread",
    [state.threads],
  );
  useTitleBadge(counts.needs_you);
  const { permission, ask } = useNotifications(state.alerts, titleOf, alerted);
  // Subscribing follows the permission the button above earns, and draws
  // nothing: push either reaches the phone or the tab title carries the count.
  useWebPush(permission);

  const select = useCallback(
    (id: string) => {
      openThread(id);
      setNotice(null);
    },
    [openThread],
  );

  const retry = useCallback(
    (id: string) => {
      setNotice(null);
      open(id).catch((e: unknown) => setNotice(e instanceof Error ? e.message : String(e)));
    },
    [open],
  );

  // Opening follows whatever the URL names, so a reload, a shared link and a
  // click all take the same path into the store.
  useEffect(() => {
    if (!active) return;
    open(active).catch((e: unknown) => setNotice(e instanceof Error ? e.message : String(e)));
  }, [active, open]);

  // Both a successful local DELETE and a tombstone from another client use the
  // same gone set. Closing from that one fact keeps the URL from naming an
  // object the screen can no longer draw, regardless of which arrived first.
  useEffect(() => {
    if (active && state.gone.includes(active)) closeThread();
  }, [active, state.gone, closeThread]);

  // The runs in the thread, and what they cost. Re-read when one starts or ends
  // rather than streamed: the counters on a turn row move on every step, and a
  // frame per step is the traffic this screen keeps off the inbox in the first
  // place. Nothing else moves them, so nothing else has to ask.
  useEffect(() => {
    if (!active) return;
    let live = true;
    void (async () => {
      try {
        const [t, s] = await Promise.all([fetchTurns(active), fetchSpend(active)]);
        if (!live) return;
        setTurns(t.items);
        setTurnCursor(t.more ? (t.cursor ?? 0) : 0);
        setSpend(s);
        setPanelLoaded(true);
        setPanelError(null);
      } catch (e) {
        // The panel is not the screen. A thread whose runs could not be read
        // still shows its conversation, which is the part that matters - so the
        // failure is reported inside the panel, next to what is missing, rather
        // than in the bar above the whole application.
        if (live) setPanelError(e instanceof Error ? e.message : String(e));
      }
    })();
    return () => {
      live = false;
    };
    // `working` rather than the turn_start/turn_end events: it is derived from
    // the turns table and republished by the same transactions that open and
    // close a run, so a frame carrying it is proof the rows are in the state it
    // describes. Keying on the events read a row the daemon had not written yet.
  }, [active, fileEvents, working, fetchTurns, fetchSpend]);

  // Everything about a thread is dropped when the reader leaves it. A diff
  // carried across would refetch a path the next thread never touched and leave
  // a blank overlay titled with someone else's file; turns and spend carried
  // across would draw the last thread's cost under this one's name until the
  // fetch above lands, which is the worst moment to be confidently wrong.
  //
  // Adjusted during render rather than in an effect, which is how React wants
  // state that follows a prop: an effect would paint the stale panel first.
  const [lastThread, setLastThread] = useState(active);
  if (lastThread !== active) {
    setLastThread(active);
    setDiff(null);
    setTurns([]);
    setTurnCursor(0);
    setSpend(null);
    setPanelLoaded(false);
    setPanelError(null);
  }

  // A run's events are held only while it is worth holding them: once the thread
  // stops working, the rows have been re-read and carry the same counts, so the
  // buffers go. Keyed on the same transition as the refetch above, and not on
  // turn_end, which arrives before the row it would be replaced by.
  useEffect(() => {
    if (working) return;
    for (const t of turns) {
      if (t.ended) traceEnded(t.seq);
    }
  }, [working, turns, traceEnded]);

  const turnOf = useMemo(() => new Map(turns.map((t) => [t.seq, t])), [turns]);
  // The run in flight, if there is one. Its row is the newest with no end.
  const running = useMemo(() => turns.find((t) => !t.ended), [turns]);
  // Stable per thread, not per render. An inline arrow here is a new identity
  // for every trace on screen on every frame, which is exactly what the memo on
  // AgentTrace exists to avoid.
  const blobURL = useCallback(
    (seq: number) => `/api/chat/threads/${encodeURIComponent(active ?? "")}/blobs/${seq}`,
    [active],
  );
  const trace = useCallback(
    (turn: Turn) => (
      <AgentTrace
        turn={turn}
        held={traces.turns[turn.seq]}
        open={traces.open.includes(turn.seq)}
        blobURL={blobURL}
        onToggle={traces.toggle}
        onMore={traces.more}
      />
    ),
    [traces, blobURL],
  );

  const traceFor = useCallback(
    (m: Message) => {
      if (!active || !m.turn) return null;
      const turn = turnOf.get(m.turn);
      // A run whose row has not been fetched yet, or has been pruned out of the
      // page, draws nothing. An empty disclosure would be worse than none.
      if (!turn) return null;
      return trace(turn);
    },
    [active, turnOf, trace],
  );

  // Reading a thread is what marks it read, and the mark follows the newest
  // message rather than the scroll position: an operator who opened it has seen
  // the row, which is what the unread count is counting.
  //
  // Keyed on the connection too, because the mark travels over the socket and a
  // write to one that is not open is dropped. Without it, a thread read during
  // a reconnect stays unread until someone says something else in it.
  const held = active ? state.messages[active] : undefined;
  const newest = held?.items.length ? held.items[held.items.length - 1].seq : 0;
  useEffect(() => {
    if (active && newest > 0 && state.connected) markRead(active, newest);
  }, [active, newest, state.connected, markRead]);

  if (fatal) return <Fatal error={fatal} />;

  const inbox = (
    <Inbox
      state={state}
      fleet={fleet}
      activeID={active}
      maxUnread={limits.max_unread}
      states={limits.states}
      operator={limits.operator}
      maxTitleChars={limits.max_title_chars}
      loaded={loaded}
      onSelect={select}
      onCreate={async (title, participants, text) => {
        const made = await create(title, participants, text);
        select(made.id);
      }}
      onLoadDone={() =>
        archived().catch((e: unknown) => setNotice(e instanceof Error ? e.message : String(e)))
      }
    />
  );

  // On a phone the inbox is the root and a thread is pushed over it: one column
  // at a time, no drawer, because a drawer holding a second copy of the list
  // beside the list is two of everything - two Threads landmarks, two of every
  // row, two new-thread forms with their own half-typed state.
  //
  // On a wide screen both stand, because switching threads while one is working
  // is the reason to have a rail at all. The roster is pushed over the inbox on
  // the same terms as a thread.
  const showsMain = railDocks || !!thread || onFleet;

  return (
    <div className="flex h-full flex-col">
      <ChatHeader
        modeSwitch={modeSwitch}
        thread={thread}
        needsYou={counts.needs_you}
        botCount={fleet.filter((a) => a.kind === "bot").length}
        connected={state.connected}
        // The roster is a pushed screen at every width, so it always offers the
        // way out. A thread on a wide screen is not: the rail beside it is how
        // one is chosen, and a back arrow over a standing list leads nowhere.
        showBack={onFleet || (!railDocks && showsMain)}
        askNotify={permission === "default" ? ask : undefined}
        onBack={onFleet ? closeFleet : closeThread}
        onOpenFleet={onFleet ? undefined : openFleet}
        onToggleProviders={() => setLogin((v) => !v)}
        // Only where it does something: with no thread open the panel has
        // nothing to describe, and a docked column needs no button to reveal it.
        onTogglePanel={thread && !panelDocks ? () => setPanel((v) => !v) : undefined}
      />

      {notice && (
        // Above both zones, not inside the thread pane. A refusal that arrives
        // with no thread open - a socket op the daemon rejected, a link to a
        // thread that was deleted - has no pane to appear in, and nesting it
        // there is how the one case URL routing introduced went silent.
        <div className="flex shrink-0 items-start gap-2 border-b border-bad/35 bg-bad/12 px-4 py-2">
          <p role="alert" className="min-w-0 flex-1 font-mono text-[12px] text-bad">
            {notice}
          </p>
          {/* A thread whose page never arrived shows a skeleton with nothing
              behind it, and clicking its row again is not something the screen
              says anywhere. */}
          {active && !held?.loaded && (
            <Button variant="outline" size="sm" onClick={() => retry(active)}>
              Retry
            </Button>
          )}
          <Button variant="ghost" size="sm" onClick={() => setNotice(null)}>
            Dismiss
          </Button>
        </div>
      )}

      <div className="relative flex min-h-0 flex-1">
        {railDocks ? (
          <aside className="flex h-full w-[300px] shrink-0 flex-col border-r border-line bg-panel">
            {inbox}
          </aside>
        ) : (
          // Not while the providers block is open: below the rail breakpoint
          // that block lives in `main`, and two siblings both claiming flex-1
          // turn a 375px viewport into two unusable columns.
          !showsMain && !login && (
            <div className="flex min-h-0 flex-1 flex-col bg-panel">{inbox}</div>
          )
        )}

        {/* On a phone with no thread open, this holds nothing - and a zone that
            holds nothing must not take half the viewport from the one that
            does. That is the single column the comment above describes. */}
        <main
          className={cn(
            "relative flex min-w-0 flex-col",
            railDocks || showsMain || login ? "flex-1" : "hidden",
          )}
        >
          {login && <Login onClose={() => setLogin(false)} />}
          {login && <Spend />}

          {onFleet ? (
            <Fleet members={fleet} loaded={loaded} />
          ) : showsMain && thread ? (
            <>
              <div className="flex shrink-0 items-start gap-2 border-b border-line px-4 py-2">
                <div className="min-w-0 flex-1">
                  <p className="truncate text-[15px] font-medium">{thread.title || "untitled"}</p>
                  <Participants
                    participants={thread.participants}
                    fleet={fleet}
                    operator={limits.operator}
                    connected={state.connected}
                    onAdd={(actor) => addActor(thread.id, actor)}
                    onRemove={(actor) => removeActor(thread.id, actor)}
                  />
                </div>
                <DeleteThreadControl
                  title={thread.title || "untitled"}
                  onDelete={() => deleteThread(thread.id)}
                />
              </div>
              {/* The diff covers the transcript and nothing else: the composer
                  below it stays usable, so a file can be read while a reply is
                  being written. That is what this wrapper is positioned for. */}
              <div className="relative flex min-h-0 flex-1 flex-col">
                <ThreadPane
                  key={thread.id}
                  thread={thread}
                  operator={limits.operator}
                  held={held}
                  traceFor={traceFor}
                  live={running ? trace(running) : undefined}
                  onOlder={() => {
                    if (held?.more) {
                      older(thread.id, held.cursor).catch((e: unknown) =>
                        setNotice(e instanceof Error ? e.message : String(e)),
                      );
                    }
                    // And the runs behind them, or every message the older page
                    // brings in loses the trace under it.
                    if (turnCursor > 0) {
                      fetchTurns(thread.id, turnCursor)
                        .then((page) => {
                          setTurns((held2) => [...held2, ...page.items]);
                          setTurnCursor(page.more ? (page.cursor ?? 0) : 0);
                        })
                        .catch(() => {});
                    }
                  }}
                />
                {diff && (
                  // Keyed by path: a fresh mount is what clears the previous
                  // file's diff, instead of showing it under the new file's name.
                  <DiffView
                    key={diff.path}
                    artifactsURL={threadArtifacts(thread.id, diff.turn)}
                    artifact={diff}
                    version={fileEvents}
                    onClose={() => setDiff(null)}
                  />
                )}
              </div>
              {/* Keyed, like the pane above it. Without a key React reuses the
                  fiber across a thread switch, so a half-typed reply follows
                  the reader into the next thread - and Enter sends it there. */}
              <Composer
                key={thread.id}
                maxBytes={limits.max_body_bytes}
                maxAttachmentBytes={limits.max_attachment_bytes}
                maxAttachments={limits.max_attachments}
                inlineImageTypes={limits.inline_image_types}
                onUpload={(file) => upload(thread.id, file)}
                connected={state.connected}
                fleet={fleet}
                participants={thread.participants}
                onSend={(text, attachments) => say(thread.id, text, undefined, attachments)}
                onAdd={(actor) => addActor(thread.id, actor)}
              />
            </>
          ) : (
            railDocks && (
              // The empty pane does the job a hero section would: it names what
              // will be here, and stops. The rail beside it already carries the
              // only action.
              <div className="px-4 py-6">
                <p className="max-w-[68ch] text-[14px] text-muted">
                  A thread is one task, with the bots working on it in it. Everything each of
                  them did while answering lands here, in order.
                </p>
              </div>
            )
          )}
        </main>

        {/* Only for a thread. What the panel holds - the plan, the files a run
            touched, what it cost - are all properties of one conversation, and
            a standing column of three empty sections beside an empty stream is
            two empty zones where one would do. */}
        {thread && (
          <SidePanel
            side="right"
            open={panel}
            layout={panelLayout}
            title="Thread"
            onDismiss={() => setPanel(false)}
          >
            <ThreadPanel
              thread={thread.id}
              operator={limits.operator}
              turns={turns}
              spend={spend}
              loaded={panelLoaded}
              failed={panelError}
              openPath={diff?.path}
              version={fileEvents}
              onOpenDiff={(a) => {
                setDiff(a);
                if (panelLayout !== "docked") setPanel(false);
              }}
            />
          </SidePanel>
        )}
      </div>
    </div>
  );
}

interface ChatHeaderProps {
  modeSwitch?: ReactNode;
  thread?: ThreadView;
  needsYou: number;
  botCount: number;
  connected: boolean;
  showBack: boolean;
  askNotify?: () => void;
  onBack: () => void;
  onOpenFleet?: () => void;
  onToggleProviders: () => void;
  onTogglePanel?: () => void;
}

function ChatHeader({
  modeSwitch,
  thread,
  needsYou,
  botCount,
  connected,
  showBack,
  askNotify,
  onBack,
  onOpenFleet,
  onToggleProviders,
  onTogglePanel,
}: ChatHeaderProps) {
  return (
    <header className="flex h-11 shrink-0 items-center gap-2 border-b border-line bg-panel px-3">
      {/* Only where it does something. On a wide screen the rail is a standing
          column, so a button offering to reveal it is a control that answers a
          tap with nothing. */}
      {showBack && (
        <Button variant="ghost" size="icon" onClick={onBack} aria-label="Threads">
          <ArrowLeft className="h-4 w-4" />
        </Button>
      )}
      <div className="flex min-w-0 flex-1 items-center gap-2">
        <span className="shrink-0 text-[15px] font-medium">aigem</span>
        {modeSwitch}
        {thread && <span className="truncate text-[13px] text-muted">{thread.title}</span>}
      </div>

      {/* Outside the md-only cluster below, deliberately. The count was already
          here and already meant "the fleet", so it is the way in - but that
          cluster is display:none on a phone, and the phone is the device the
          roster was built for. A control that exists only where it is least
          needed is the same as no control.

          It stays a plain badge on the roster itself, where it would only lead
          back to the screen the reader is on. */}
      {onOpenFleet ? (
        <Button variant="ghost" size="sm" className="shrink-0 font-mono" onClick={onOpenFleet}>
          {botCount} {botCount === 1 ? "bot" : "bots"}
        </Button>
      ) : (
        <Badge className="shrink-0 font-mono">
          {botCount} {botCount === 1 ? "bot" : "bots"}
        </Badge>
      )}

      <div className="hidden shrink-0 items-center gap-2 md:flex">
        {needsYou > 0 && (
          <Badge className="border-accent/40 font-mono text-accent">{needsYou} need you</Badge>
        )}
        {thread?.working && (
          <Badge>
            <RunDot className="mr-1" /> working
          </Badge>
        )}
      </div>

      {/* Only while the answer is still open. Asked on a click rather than on
          load, which is the only form most browsers still honour. */}
      {askNotify && (
        <Button variant="ghost" size="icon" onClick={askNotify} aria-label="Enable notifications">
          <Bell className="h-4 w-4" />
        </Button>
      )}
      {onTogglePanel && (
        <Button variant="ghost" size="icon" onClick={onTogglePanel} aria-label="Thread panel">
          <PanelRight className="h-4 w-4" />
        </Button>
      )}
      <Button variant="ghost" size="icon" onClick={onToggleProviders} aria-label="Providers">
        <KeyRound className="h-4 w-4" />
      </Button>
      {/* Muted, not the accent. A socket that is retrying itself is not a
          decision the reader has to make, and peach beside the genuine "needs
          you" badge is exactly the confusion one accent meaning prevents. */}
      {!connected && (
        <>
          {/* The word below md would push the header past a 375px viewport,
              and horizontal document overflow is a critical failure. */}
          <Badge className="hidden shrink-0 md:inline-flex">
            <WifiOff className="mr-1 h-3 w-3" aria-hidden /> reconnecting
          </Badge>
          <WifiOff
            className="h-4 w-4 shrink-0 text-muted md:hidden"
            role="img"
            aria-label="Reconnecting"
          />
        </>
      )}
    </header>
  );
}
