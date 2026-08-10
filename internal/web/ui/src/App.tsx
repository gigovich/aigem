import { useCallback, useEffect, useRef, useState } from "react";
import { CircleStop, FileDiff, KeyRound, MessagesSquare, Plus, Send, WifiOff, X } from "lucide-react";
import { api, type SessionView } from "@/lib/protocol";
import { useSession } from "@/lib/session";
import { Timeline } from "@/components/timeline";
import { Login } from "@/components/login";
import { Files } from "@/components/files";
import { Spend } from "@/components/usage";
import { Badge, Button, Spinner } from "@/components/ui";
import { cn } from "@/lib/utils";

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

  useEffect(() => {
    (async () => {
      try {
        const next = await refresh();
        if (next.length > 0) return setID(next[0].id);
        setID((await open()).id);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    })();
  }, [refresh]);

  const open = async () =>
    api<SessionView>("/api/sessions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });

  const create = useCallback(async () => {
    try {
      const made = await open();
      await refresh();
      setID(made.id);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [refresh]);

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
  req: { id: string; req: { kind: string; tool: string; path?: string; write?: boolean; options: { value: string; label: string }[] } };
  onAnswer: (id: string, decision: string) => void;
}) {
  const a = req.req;
  const title =
    a.kind === "path"
      ? `Let ${a.tool} ${a.write ? "modify" : "read"} a file outside the working directory?`
      : "Run this tool?";
  return (
    <div className="border-t border-warn/40 bg-warn/10 px-3 py-3">
      <div className="mx-auto max-w-3xl">
        <p className="text-[13px] font-medium text-warn">{title}</p>
        <p className="mt-0.5 truncate font-mono text-[13px] text-fg">{a.path ?? a.tool}</p>
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

export default function App() {
  const { list, id, setID, error, refresh, create, close } = useDaemonSessions();
  const { state, submit, interrupt, resolve } = useSession(id);
  const [draft, setDraft] = useState("");
  const [picker, setPicker] = useState(false);
  const [login, setLogin] = useState(false);
  const [files, setFiles] = useState(false);
  const bottom = useRef<HTMLDivElement>(null);
  const scroller = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);

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

  useEffect(() => {
    if (state.title) void refresh();
  }, [state.title, refresh]);

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
      <header className="flex shrink-0 items-center gap-2 border-b border-border bg-panel px-3 py-2">
        <Button
          variant="ghost"
          size="sm"
          onClick={() => { setPicker((p) => !p); void refresh(); }}
          title="Conversations"
        >
          <MessagesSquare className="h-4 w-4" />
          {list.length > 1 && <span className="text-xs">{list.length}</span>}
        </Button>
        <span className="font-semibold tracking-tight">aigem</span>
        {state.title && <span className="truncate text-sm text-muted">{state.title}</span>}
        <div className="ml-auto flex items-center gap-2">
          <Button variant="ghost" size="icon" onClick={() => setFiles((v) => !v)} title="Files">
            <FileDiff className="h-4 w-4" />
          </Button>
          <Button variant="ghost" size="icon" onClick={() => setLogin((v) => !v)} title="Providers">
            <KeyRound className="h-4 w-4" />
          </Button>
          {state.model && <Badge>{state.model}</Badge>}
          {state.tokens > 0 && (
            <Badge>
              {Math.round(state.tokens / 1000)}k
              {state.ctx > 0 && ` / ${Math.round(state.ctx / 1000)}k`} ctx
            </Badge>
          )}
          {state.clients.length > 1 && <Badge>{state.clients.length} attached</Badge>}
          {!state.connected && (
            <Badge className="border-warn/40 text-warn">
              <WifiOff className="mr-1 h-3 w-3" /> reconnecting
            </Badge>
          )}
        </div>
      </header>

      {login && <Login onClose={() => setLogin(false)} />}
      {login && <Spend />}

      {picker && (
        <div className="shrink-0 border-b border-border bg-panel-2">
          <div className="mx-auto max-w-3xl px-3 py-2">
            {list.map((s) => (
              <div key={s.id} className="flex items-center gap-2 py-1">
                <button
                  onClick={() => { setID(s.id); setPicker(false); }}
                  className={cn(
                    "flex min-w-0 flex-1 items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm",
                    s.id === id ? "bg-panel text-fg" : "text-muted hover:bg-panel",
                  )}
                >
                  <span className="truncate">{s.title || "new conversation"}</span>
                  {s.running && <Spinner className="ml-auto" />}
                </button>
                <Button variant="ghost" size="icon" title="Close" onClick={() => void close(s.id)}>
                  <X className="h-4 w-4" />
                </Button>
              </div>
            ))}
            <Button variant="outline" size="sm" className="mt-1" onClick={() => void create()}>
              <Plus className="h-3.5 w-3.5" /> New conversation
            </Button>
          </div>
        </div>
      )}

      {state.todos.length > 0 && (
        <div className="shrink-0 overflow-x-auto border-b border-border bg-panel px-3 py-1.5">
          <div className="mx-auto flex max-w-3xl gap-2">
            {state.todos.map((t, i) => (
              <Badge
                key={i}
                className={cn(
                  "shrink-0",
                  t.status === "in_progress" && "border-accent/50 text-accent",
                  t.status === "completed" && "text-good line-through",
                )}
              >
                {t.text}
              </Badge>
            ))}
          </div>
        </div>
      )}

      {files && id ? (
        <Files sessionID={id} onClose={() => setFiles(false)} />
      ) : (
        <div ref={scroller} onScroll={onScroll} className="min-h-0 flex-1 overflow-y-auto">
          {id ? <Timeline items={state.items} sessionID={id} /> : null}
          <div ref={bottom} />
        </div>
      )}

      {state.approval && <Approval req={state.approval} onAnswer={(a, d) => resolve(a, d as never)} />}

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
    </div>
  );
}
