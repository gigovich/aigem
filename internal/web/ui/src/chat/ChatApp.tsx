import { useCallback, useEffect, useState, type ReactNode } from "react";
import { ArrowLeft, Bell, KeyRound, WifiOff } from "lucide-react";
import { api } from "@/lib/protocol";
import { countsOf, useChat } from "@/lib/chat";
import { displayName, OPERATOR, type Actor, type ChatMeta, type ThreadView } from "@/lib/chatprotocol";
import { RAIL_DOCKS, useMedia } from "@/lib/media";
import { cn } from "@/lib/utils";
import { useThread } from "@/lib/route";
import { useNotifications, useTitleBadge } from "@/lib/notify";
import { Fatal } from "@/components/fatal";
import { Login } from "@/components/login";
import { Spend } from "@/components/usage";
import { Badge, Button, RunDot } from "@/components/ui";
import { Inbox } from "./Inbox";
import { ThreadPane } from "./ThreadView";
import { Composer } from "./Composer";

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
};

/** How often the roster is re-read. Slow on purpose: it changes when a bot is
 *  started or stopped, which is an operator action minutes apart, not something
 *  that happens inside a turn. */
const FLEET_POLL_MS = 30_000;

export function ChatApp({ modeSwitch }: { modeSwitch?: ReactNode }) {
  const [notice, setNotice] = useState<string | null>(null);
  // A refusal of one of this client's ops is the answer to something the
  // operator just did, so it goes where they are looking rather than nowhere.
  const { state, refresh, archived, open, older, say, markRead, create, alerted } =
    useChat(setNotice);
  const [meta, setMeta] = useState<ChatMeta | null>(null);
  const [fleet, setFleet] = useState<Actor[]>([]);
  // The open thread lives in the URL, so the back button leaves a thread
  // instead of leaving the app, and a thread is a link that can be sent.
  const { thread: active, open: openThread, close: closeThread } = useThread();
  const [loaded, setLoaded] = useState(false);
  // Two kinds of failure, and only one of them is the end of the screen. The
  // page cannot start without its meta and its inbox; a thread that failed to
  // open is one request, and taking the whole workspace down for it loses the
  // live socket, the inbox and every other thread over a blip the socket layer
  // would have survived on its own.
  const [fatal, setFatal] = useState<string | null>(null);
  const [login, setLogin] = useState(false);
  const railDocks = useMedia(RAIL_DOCKS);

  const thread = active ? state.threads[active]?.view : undefined;
  const counts = countsOf(state);
  const limits = meta ?? PROVISIONAL;

  useEffect(() => {
    (async () => {
      try {
        const [m, f] = await Promise.all([
          api<ChatMeta>("/api/chat/meta"),
          api<Actor[]>("/api/chat/fleet"),
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
      api<Actor[]>("/api/chat/fleet").then(setFleet).catch(() => {});
    };
    const timer = window.setInterval(poll, FLEET_POLL_MS);
    return () => window.clearInterval(timer);
  }, [loaded]);

  const titleOf = useCallback(
    (id: string) => state.threads[id]?.view.title || "a thread",
    [state.threads],
  );
  useTitleBadge(counts.needs_you);
  const { permission, ask } = useNotifications(state.alerts, titleOf, alerted);

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
  // is the reason to have a rail at all.
  const showsThread = railDocks || !!thread;

  return (
    <div className="flex h-full flex-col">
      <ChatHeader
        modeSwitch={modeSwitch}
        thread={thread}
        needsYou={counts.needs_you}
        botCount={fleet.filter((a) => a.kind === "bot").length}
        connected={state.connected}
        showBack={!railDocks && showsThread}
        askNotify={permission === "default" ? ask : undefined}
        onBack={closeThread}
        onToggleProviders={() => setLogin((v) => !v)}
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
          !showsThread && <div className="flex min-h-0 flex-1 flex-col bg-panel">{inbox}</div>
        )}

        {/* On a phone with no thread open, this holds nothing - and a zone that
            holds nothing must not take half the viewport from the one that
            does. That is the single column the comment above describes. */}
        <main
          className={cn(
            "relative flex min-w-0 flex-col",
            railDocks || showsThread || login ? "flex-1" : "hidden",
          )}
        >
          {login && <Login onClose={() => setLogin(false)} />}
          {login && <Spend />}

          {showsThread && thread ? (
            <>
              <div className="shrink-0 border-b border-line px-4 py-2">
                <p className="truncate text-[15px] font-medium">{thread.title || "untitled"}</p>
                <p className="truncate text-[12px] text-muted">
                  {thread.participants.map((p) => displayName(p, limits.operator)).join(" · ")}
                </p>
              </div>
              <ThreadPane
                key={thread.id}
                thread={thread}
                operator={limits.operator}
                held={held}
                onOlder={() => {
                  if (held?.more) {
                    older(thread.id, held.cursor).catch((e: unknown) =>
                      setNotice(e instanceof Error ? e.message : String(e)),
                    );
                  }
                }}
              />
              {/* Keyed, like the pane above it. Without a key React reuses the
                  fiber across a thread switch, so a half-typed reply follows
                  the reader into the next thread - and Enter sends it there. */}
              <Composer
                key={thread.id}
                maxBytes={limits.max_body_bytes}
                connected={state.connected}
                onSend={(text) => say(thread.id, text)}
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
  onToggleProviders: () => void;
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
  onToggleProviders,
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

      <div className="hidden shrink-0 items-center gap-2 md:flex">
        <Badge className="font-mono">
          {botCount} {botCount === 1 ? "bot" : "bots"}
        </Badge>
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
