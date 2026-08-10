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
import { SidePanel } from "@/components/panel";
import { Button, Spinner } from "@/components/ui";
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
    <div className="border-t border-warn/40 bg-warn/10 px-3 py-3">
      <div className="mx-auto max-w-3xl">
        <p className="text-[13px] font-medium text-warn">{title}</p>
        {detail && (
          <pre className="mt-1 max-h-40 overflow-auto rounded-md border border-warn/25 bg-bg/40 px-2 py-1.5 font-mono text-[12px] whitespace-pre-wrap break-words text-fg">
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
    </div>
  );
}

/** Tailwind's lg, in JS. The rails are standing columns above it and drawers
 *  below, and the two have to agree on where the line is. */
const WIDE = "(min-width: 1024px)";

/** Sampling the width once at mount left a resized window - or a rotated tablet,
 *  or a zoomed page - with two drawers open over the conversation and two
 *  backdrops between the reader and every control. */
function useWide(): boolean {
  const [wide, setWide] = useState(
    () => typeof window !== "undefined" && window.matchMedia?.(WIDE).matches === true,
  );
  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) return;
    const mq = window.matchMedia(WIDE);
    const sync = () => setWide(mq.matches);
    mq.addEventListener("change", sync);
    return () => mq.removeEventListener("change", sync);
  }, []);
  return wide;
}

export default function App() {
  const { list, id, setID, error, refresh, create, close } = useDaemonSessions();
  const { state, submit, interrupt, resolve } = useSession(id);
  const isWide = useWide();
  const [draft, setDraft] = useState("");
  const [nav, setNav] = useState(isWide);
  const [rail, setRail] = useState(isWide);
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
  const [lastWide, setLastWide] = useState(isWide);
  if (lastWide !== isWide) {
    // Crossing the breakpoint changes what a rail *is*, so it also decides
    // whether one should be showing: columns by default, drawers only on ask.
    setLastWide(isWide);
    setNav(isWide);
    setRail(isWide);
  }

  const [lastID, setLastID] = useState(id);
  if (lastID !== id) {
    // A diff belongs to the conversation it was opened from; carrying it across
    // would refetch a path the next conversation never touched and leave a
    // blank overlay titled with someone else's file.
    setLastID(id);
    setDiff(null);
  }

  // Below the breakpoint a drawer covers the page, so two of them cover it twice
  // and the backdrop only dismisses the one on top.
  const showNav = (open: boolean) => {
    setNav(open);
    if (open && !isWide) setRail(false);
  };
  const showRail = (open: boolean) => {
    setRail(open);
    if (open && !isWide) setNav(false);
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
      <div className="grid h-full place-items-center p-6 text-center">
        <div>
          <p className="text-bad">{error}</p>
          <p className="mt-2 text-sm text-muted">
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

      <div className="relative flex min-h-0 flex-1">
        <SidePanel side="left" open={nav} modal={!isWide} title="Conversations" onDismiss={() => showNav(false)}>
          <Sidebar
            list={list}
            activeID={id}
            onSelect={(next) => { setID(next); if (!isWide) showNav(false); }}
            onCreate={() => void create()}
            onCloseConversation={(target) => void close(target)}
          />
        </SidePanel>

        <main className="relative flex min-w-0 flex-1 flex-col" inert={!isWide && (nav || rail)}>
          {login && <Login onClose={() => setLogin(false)} />}
          {login && <Spend />}

          <div className="relative flex min-h-0 flex-1 flex-col">
          <div ref={scroller} onScroll={onScroll} className="min-h-0 flex-1 overflow-y-auto">
            {id && replayed && state.items.length === 0 && !state.running && (
              <div className="grid h-full place-items-center p-6 text-center">
                <p className="max-w-sm text-sm text-muted">
                  Ask for a change and watch it happen: every command lands here, and the
                  buttons above open the plan and the files it touched.
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

          <div className="shrink-0 border-t border-border bg-panel p-2">
            <div className="mx-auto flex max-w-3xl items-end gap-2">
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
                className="max-h-40 min-h-9 flex-1 resize-y rounded-md border border-border bg-panel-2 px-3 py-2 text-[15px] outline-none placeholder:text-muted focus:border-accent/60"
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
              <div className="mx-auto mt-1 flex max-w-3xl items-center gap-2 text-[12px] text-muted">
                <Spinner /> working
              </div>
            )}
          </div>
        </main>

        <SidePanel side="right" open={rail} modal={!isWide} title="Session" onDismiss={() => showRail(false)}>
          {state.todos.length > 0 && (
            <>
              <Plan todos={state.todos} />
              <div className="mx-3 shrink-0 border-t border-border" />
            </>
          )}
          {id && (
            <ChangedFiles
              // Keyed by conversation: reconciling instead would leave the last
              // conversation's files on screen, and a slow response for it could
              // land after this one's and stick.
              key={id}
              sessionID={id}
              version={state.fileEvents}
              openPath={diff?.path}
              onOpen={(a) => { setDiff(a); if (!isWide) showRail(false); }}
            />
          )}
        </SidePanel>
      </div>
    </div>
  );
}
