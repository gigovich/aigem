import { useCallback, useEffect, useRef, useState } from "react";
import { CircleStop, Send } from "lucide-react";
import { api, type SessionView } from "@/lib/protocol";
import { useSession } from "@/lib/session";
import { Timeline } from "@/components/timeline";
import { Login } from "@/components/login";
import { ChangedFiles, DiffView, type Artifact } from "@/components/files";
import { Spend } from "@/components/usage";
import { Header } from "@/components/header";
import { Sidebar } from "@/components/sidebar";
import { Plan, planProgress } from "@/components/plan";
import { SidePanel, type PanelLayout } from "@/components/panel";
import { Button, RunDot } from "@/components/ui";
import { approvalDetail } from "@/lib/utils";

/** The daemon can hold several conversations. Adopt whichever it has, open one
 *  when it has none, and keep the list fresh enough to switch between them. */
function useDaemonSessions() {
  const [list, setList] = useState<SessionView[]>([]);
  const [id, setID] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    const next = await api<SessionView[]>("/api/sessions");
    setList(next);
    return next;
  }, []);

  const open = useCallback(
    () =>
      api<SessionView>("/api/sessions", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: "{}",
      }),
    [],
  );

  useEffect(() => {
    (async () => {
      try {
        const next = await refresh();
        if (next.length > 0) return setID(next[0].id);
        const made = await open();
        setList([made]);
        setID(made.id);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    })();
  }, [open, refresh]);

  const create = useCallback(async () => {
    try {
      const made = await open();
      await refresh();
      setID(made.id);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [open, refresh]);

  const close = useCallback(
    async (target: string) => {
      try {
        await api<void>(`/api/sessions/${target}`, { method: "DELETE" });
        const next = await refresh();
        // Closing the one being viewed moves to whatever is left rather than
        // leaving a socket pointed at a conversation that is gone.
        if (target === id) setID(next[0]?.id ?? null);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [id, refresh],
  );

  return { list, id, setID, error, refresh, create, close };
}

function Approval({
  req,
  onAnswer,
}: {
  req: { id: string; req: { kind: string; tool: string; args?: unknown; path?: string; write?: boolean; options: { value: string; label: string }[] } };
  onAnswer: (id: string, decision: string) => void;
}) {
  const a = req.req;
  const title =
    a.kind === "path"
      ? `Let ${a.tool} ${a.write ? "modify" : "read"} a file outside the working directory?`
      : `Run ${a.tool}?`;
  // What is being approved is the command, not the tool that runs it. Naming
  // only the tool asked the reader to approve "bash" and hope.
  const detail = a.path ?? approvalDetail(a.tool, a.args);
  return (
    // The accent, because a pending approval and the primary button are the same
    // event in this product: you are the one who has to act.
    <div className="shrink-0 border-t border-accent/35 bg-accent/12 px-4 py-3">
      <p className="text-[13px] font-medium text-accent">{title}</p>
      {detail && (
        <pre className="mt-1 max-h-40 max-w-[68ch] overflow-auto rounded-md border border-accent/25 bg-canvas/40 px-2 py-1.5 font-mono text-[12px] leading-[1.45] whitespace-pre-wrap break-words text-fg">
          {detail}
        </pre>
      )}
      <div className="mt-2 flex flex-wrap gap-2">
        {a.options.map((o, i) => (
          <Button
            key={o.value}
            size="sm"
            // The last option is always the refusal, whatever it is called.
            variant={i === a.options.length - 1 ? "danger" : i === 0 ? "default" : "outline"}
            onClick={() => onAnswer(req.id, o.value)}
          >
            {o.label}
          </Button>
        ))}
      </div>
    </div>
  );
}

/** The two lines from DESIGN.md, in JS. The conversation rail stands from the
 *  single-column breakpoint up; the session panel needs a workspace wide enough
 *  to hold a diff beside the stream, which is a good deal more. */
const RAIL_DOCKS = "(min-width: 768px)";
const PANEL_DOCKS = "(min-width: 1280px)";

/** Sampling the width once at mount left a resized window - or a rotated tablet,
 *  or a zoomed page - with two drawers open over the conversation and two
 *  backdrops between the reader and every control. */
function useMedia(query: string): boolean {
  const [matches, setMatches] = useState(
    () => typeof window !== "undefined" && window.matchMedia?.(query).matches === true,
  );
  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) return;
    const mq = window.matchMedia(query);
    const sync = () => setMatches(mq.matches);
    mq.addEventListener("change", sync);
    return () => mq.removeEventListener("change", sync);
  }, [query]);
  return matches;
}

export default function App() {
  const { list, id, setID, error, refresh, create, close } = useDaemonSessions();
  const { state, submit, interrupt, resolve } = useSession(id);
  const railDocks = useMedia(RAIL_DOCKS);
  const panelDocks = useMedia(PANEL_DOCKS);
  const navLayout: PanelLayout = railDocks ? "docked" : "drawer";
  // On a phone the session panel rises from the bottom edge, where the thumb
  // already is; a left-and-right pair of drawers on a 380px screen is two ways
  // of covering the conversation.
  const railLayout: PanelLayout = panelDocks ? "docked" : railDocks ? "drawer" : "sheet";
  const [draft, setDraft] = useState("");
  const [nav, setNav] = useState(railDocks);
  const [rail, setRail] = useState(panelDocks);
  const [login, setLogin] = useState(false);
  const [diff, setDiff] = useState<Artifact | null>(null);
  const bottom = useRef<HTMLDivElement>(null);
  const scroller = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);
  const plan = planProgress(state.todos);
  // The socket's reset empties the timeline before the replay arrives, so "no
  // items yet" is true for a moment of every switch. The daemon numbers every
  // event it sends, including the presence one it opens with, so a sequence
  // number having arrived is what says the replay is done.
  const replayed = state.lastSeq > 0;

  // Adjusted during render rather than in an effect, which is how React wants
  // state that follows a prop: an effect would paint the wrong layout first.
  const [lastDocks, setLastDocks] = useState(`${railDocks}/${panelDocks}`);
  if (lastDocks !== `${railDocks}/${panelDocks}`) {
    // Crossing a breakpoint changes what a rail *is*, so it also decides
    // whether one should be showing: columns by default, drawers only on ask.
    setLastDocks(`${railDocks}/${panelDocks}`);
    setNav(railDocks);
    setRail(panelDocks);
  }

  const [lastID, setLastID] = useState(id);
  if (lastID !== id) {
    // A diff belongs to the conversation it was opened from; carrying it across
    // would refetch a path the next conversation never touched and leave a
    // blank overlay titled with someone else's file.
    setLastID(id);
    setDiff(null);
  }

  // An undocked panel covers the page, so two of them cover it twice and the
  // backdrop only dismisses the one on top. Both have to be undocked for that to
  // be true: at the middle width the rail is a standing column, and dismissing
  // it to open a drawer that does not overlap it just took the column away.
  const bothCover = navLayout !== "docked" && railLayout !== "docked";
  const showNav = (open: boolean) => {
    setNav(open);
    if (open && bothCover) setRail(false);
  };
  const showRail = (open: boolean) => {
    setRail(open);
    if (open && bothCover) setNav(false);
  };

  // Follow the end only while the reader is already there; scrolling up to read
  // something must not be undone by the next token.
  useEffect(() => {
    if (pinned.current) bottom.current?.scrollIntoView({ block: "end" });
  }, [state.items, state.approval]);

  const onScroll = () => {
    const el = scroller.current;
    if (!el) return;
    pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
  };

  // The sidebar shows which conversation is working, so the list has to be
  // refetched when a turn ends too - not only when the title first arrives.
  useEffect(() => {
    // Not before a conversation is adopted: on a cold daemon that GET races the
    // one that creates the first conversation, and an empty answer landing last
    // wins - leaving the sidebar empty next to an open conversation.
    if (!id) return;
    // A background refresh that fails is a stale list, not a broken page, so it
    // must not reach the error screen the initial load uses.
    refresh().catch(() => {});
  }, [id, state.title, state.running, refresh]);

  const send = () => {
    const text = draft.trim();
    if (!text) return;
    submit(text);
    setDraft("");
  };

  if (error) {
    return (
      <div className="flex h-full items-center p-6">
        <div className="max-w-[68ch]">
          <p className="text-[14px] text-bad">{error}</p>
          <p className="mt-2 text-[13px] text-muted">
            Open the URL the daemon printed - it carries the token.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="flex h-full flex-col">
      <Header
        conversationCount={list.length}
        title={state.title}
        model={state.model}
        tokens={state.tokens}
        ctx={state.ctx}
        clientCount={state.clients.length}
        connected={state.connected}
        planDone={plan.done}
        planTotal={plan.total || undefined}
        navOpen={nav}
        railOpen={rail}
        onToggleConversations={() => { showNav(!nav); refresh().catch(() => {}); }}
        onToggleFiles={() => showRail(!rail)}
        onToggleProviders={() => setLogin((v) => !v)}
      />

      {/* Three unequal zones: a narrow rail, the stream, a medium panel. The
          columns are declared here rather than by each child sizing itself, so
          a long tool result widens nothing. */}
      <div className="relative flex min-h-0 flex-1">
        <SidePanel side="left" open={nav} layout={navLayout} title="Conversations" onDismiss={() => showNav(false)}>
          <Sidebar
            list={list}
            activeID={id}
            onSelect={(next) => { setID(next); if (navLayout !== "docked") showNav(false); }}
            onCreate={() => void create()}
            onCloseConversation={(target) => void close(target)}
          />
        </SidePanel>

        <main
          className="relative flex min-w-0 flex-1 flex-col"
          inert={(navLayout !== "docked" && nav) || (railLayout !== "docked" && rail)}
        >
          {login && <Login onClose={() => setLogin(false)} />}
          {login && <Spend />}

          <div className="relative flex min-h-0 flex-1 flex-col">
          <div ref={scroller} onScroll={onScroll} className="min-h-0 flex-1 overflow-y-auto">
            {id && replayed && state.items.length === 0 && !state.running && (
              // The empty workspace does the job a hero section would: it names
              // what this pane will hold, and stops.
              <div className="px-4 py-6">
                <p className="max-w-[68ch] text-[14px] text-muted">
                  Every command this agent runs lands here, in order. The plan and the files
                  it touched are in the session panel.
                </p>
              </div>
            )}
            {id ? <Timeline items={state.items} sessionID={id} /> : null}
            <div ref={bottom} />
          </div>

          {diff && id && (
            // Keyed by path: a fresh mount is what clears the previous file's
            // diff, instead of showing it under the new file's name.
            <DiffView
              key={diff.path}
              sessionID={id}
              artifact={diff}
              version={state.fileEvents}
              onClose={() => setDiff(null)}
            />
          )}
          </div>

          {state.approval && (
            <Approval req={state.approval} onAnswer={(a, d) => resolve(a, d as never)} />
          )}

          <div className="shrink-0 border-t border-line bg-panel px-4 py-2">
            <div className="flex items-end gap-2">
              <textarea
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  // Enter sends; a newline needs a modifier, as it does everywhere
                  // else a message is typed.
                  if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
                    e.preventDefault();
                    send();
                  }
                }}
                rows={1}
                placeholder={state.running ? "Add to what it is doing..." : "Ask aigem..."}
                // Six lines at 14px and 1.6, then it scrolls: past that the
                // composer is eating the conversation it is being written about.
                className="max-h-[9.5rem] min-h-9 flex-1 resize-y rounded-md border border-line bg-raised px-3 py-2 text-[14px] outline-none placeholder:text-muted focus:border-accent/60"
              />
              {state.running ? (
                <Button variant="outline" size="icon" onClick={interrupt} title="Interrupt">
                  <CircleStop className="h-4 w-4" />
                </Button>
              ) : (
                <Button size="icon" onClick={send} disabled={!draft.trim()} title="Send">
                  <Send className="h-4 w-4" />
                </Button>
              )}
            </div>
            {state.running && (
              <div className="mt-1 flex items-center gap-2 text-[12px] text-muted">
                <RunDot /> working
              </div>
            )}
          </div>
        </main>

        <SidePanel side="right" open={rail} layout={railLayout} title="Session" onDismiss={() => showRail(false)}>
          {state.todos.length > 0 && <Plan todos={state.todos} />}
          {id && (
            <ChangedFiles
              // Keyed by conversation: reconciling instead would leave the last
              // conversation's files on screen, and a slow response for it could
              // land after this one's and stick.
              key={id}
              sessionID={id}
              version={state.fileEvents}
              openPath={diff?.path}
              onOpen={(a) => { setDiff(a); if (railLayout !== "docked") showRail(false); }}
            />
          )}
        </SidePanel>
      </div>
    </div>
  );
}
