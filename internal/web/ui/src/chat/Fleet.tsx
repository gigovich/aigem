import { useEffect, useMemo, useRef, useState } from "react";
import type {
  BotModels,
  BotModelSettings,
  FleetMember,
  LiveBot,
  ModelOption,
} from "@/lib/chatprotocol";
import { api } from "@/lib/protocol";
import { Spend } from "@/components/usage";
import { Badge, Button, RunDot, SkeletonRows } from "@/components/ui";

const COLS = "grid grid-cols-[10rem_9rem_6rem_5rem_7rem_minmax(11rem,1fr)_minmax(20rem,1.4fr)]";

export function Fleet({ members, loaded }: { members: FleetMember[]; loaded: boolean }) {
  const bots = members.filter((m) => m.kind === "bot");
  const [models, setModels] = useState<BotModels | null>(null);
  const [modelError, setModelError] = useState<string | null>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [editorOpener, setEditorOpener] = useState<HTMLButtonElement | null>(null);
  const settings = useMemo(() => new Map(models?.bots.map((b) => [b.name, b]) ?? []), [models]);

  useEffect(() => {
    let active = true;
    api<BotModels>("/api/chat/bots/models")
      .then((value) => { if (active) setModels(value); })
      .catch((e: unknown) => { if (active) setModelError(e instanceof Error ? e.message : String(e)); });
    return () => { active = false; };
  }, []);

  const retryModels = () => {
    setModelError(null);
    api<BotModels>("/api/chat/bots/models")
      .then(setModels)
      .catch((e: unknown) => setModelError(e instanceof Error ? e.message : String(e)));
  };

  const edited = editing ? settings.get(editing) : undefined;
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="px-4 py-3">
        <h2 className="text-[15px] font-medium">Fleet</h2>
        <p className="mt-1 max-w-[68ch] text-[13px] text-muted">
          Selected is the saved model for the next start. Running is the model already open; changing
          a selection never hot-swaps a running bot.
        </p>
        {modelError && (
          <div className="mt-2 flex max-w-[68ch] items-center gap-2">
            <p role="status" className="min-w-0 flex-1 text-[12px] text-muted">
              Model editing is unavailable on this server.
            </p>
            <Button variant="outline" size="sm" onClick={retryModels}>Retry</Button>
          </div>
        )}
      </div>

      {!loaded ? (
        <div>
          <Header />
          <SkeletonRows rows={4} rowClass="h-[52px]" className="gap-0" />
        </div>
      ) : bots.length === 0 ? (
        <p className="px-4 text-[14px] text-muted">
          No bots are configured yet. <code className="font-mono">aigem bot create</code> adds one.
        </p>
      ) : (
        <div className="min-w-0 overflow-x-auto">
          <div className="w-max min-w-full" role="table" aria-label="Fleet">
            <Header />
            <div role="rowgroup">
              {bots.map((m) => (
                <FleetRow
                  key={m.id}
                  member={m}
                  settings={settings.get(m.name)}
                  editable={!!models}
                  onEdit={(opener) => { setEditorOpener(opener); setEditing(m.name); }}
                />
              ))}
            </div>
          </div>
        </div>
      )}

      {edited && models && (
        <ModelEditor
          settings={edited}
          options={models.options}
          opener={editorOpener}
          onSaved={(saved) => {
            setModels((held) => held ? { ...held, bots: held.bots.map((b) => b.name === saved.name ? saved : b) } : held);
          }}
          onClose={() => setEditing(null)}
        />
      )}

      <div className="mt-4"><Spend /></div>
    </div>
  );
}

function Header() {
  return (
    <div role="rowgroup">
      <div role="row" className={`${COLS} items-center gap-3 border-y border-line bg-raised px-4 py-1.5 text-[11px] text-muted`}>
        {["bot", "role", "state", "threads", "heartbeat", "next job", "models"].map((c) => (
          <span key={c} role="columnheader" className={c === "threads" ? "text-right" : undefined}>{c}</span>
        ))}
      </div>
    </div>
  );
}

function FleetRow({
  member,
  settings,
  editable,
  onEdit,
}: {
  member: FleetMember;
  settings?: BotModelSettings;
  editable: boolean;
  onEdit: (opener: HTMLButtonElement) => void;
}) {
  const live = member.live;
  return (
    <div role="row" className={`${COLS} items-center gap-3 border-b border-line-faint px-4 py-2`}>
      <span role="cell" className="truncate text-[14px]">{member.name}</span>
      <span role="cell" className="truncate text-[13px] text-muted">{member.role || "-"}</span>
      <span role="cell"><State member={member} /></span>
      <span role="cell" className="text-right font-mono text-[12px] text-muted">{member.threads}</span>
      <span role="cell" className="font-mono text-[12px] text-muted">{heartbeatOf(live)}</span>
      <span role="cell" className="truncate font-mono text-[12px] text-muted">{nextJobOf(live)}</span>
      <span role="cell" className="min-w-0 text-[12px]">
        <span className="block truncate font-mono text-muted">running: {settings?.running || live?.model || "-"}</span>
        <span className="block truncate font-mono">selected: {settings?.selected || "-"}</span>
        {settings && (
          <span className="mt-0.5 flex items-center gap-2">
            <span className="text-muted">{settings.source === "role-default" ? "role default" : "configured"}</span>
            {settings.restart_required && <Badge className="border-accent/40 text-accent">restart required</Badge>}
          </span>
        )}
        {editable && settings && <Button variant="outline" size="sm" className="mt-1" onClick={(event) => onEdit(event.currentTarget)}>Change model</Button>}
      </span>
    </div>
  );
}

function ModelEditor({
  settings,
  options,
  opener,
  onSaved,
  onClose,
}: {
  settings: BotModelSettings;
  options: ModelOption[];
  opener: HTMLButtonElement | null;
  onSaved: (saved: BotModelSettings) => void;
  onClose: () => void;
}) {
  const ROLE_DEFAULT = "__role_default__";
  const [value, setValue] = useState(settings.configured || ROLE_DEFAULT);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const modal = useRef<HTMLElement>(null);
  const select = useRef<HTMLSelectElement>(null);
  const pendingRef = useRef(pending);
  const closeRef = useRef(onClose);

  useEffect(() => {
    pendingRef.current = pending;
    closeRef.current = onClose;
  }, [pending, onClose]);

  useEffect(() => {
    select.current?.focus();
    const onKey = (event: KeyboardEvent) => {
      const root = modal.current;
      if (!root) return;
      if (event.key === "Escape") {
        if (!pendingRef.current) closeRef.current();
        return;
      }
      if (event.key !== "Tab") return;
      const focusable = Array.from(root.querySelectorAll<HTMLElement>(
        'button:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])',
      ));
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (!first || !last) {
        event.preventDefault();
        root.focus();
      } else if (event.shiftKey && (document.activeElement === first || !root.contains(document.activeElement))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !root.contains(document.activeElement))) {
        event.preventDefault();
        first.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      opener?.focus();
    };
  }, [opener]);

  const submit = async () => {
    if (pending) return;
    setPending(true);
    setError(null);
    setSaved(false);
    try {
      const updated = await api<BotModelSettings>(
        `/api/chat/bots/${encodeURIComponent(settings.name)}/model`,
        {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ model: value === ROLE_DEFAULT ? null : value }),
        },
      );
      onSaved(updated);
      setValue(updated.configured || ROLE_DEFAULT);
      setSaved(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setPending(false);
    }
  };

  const defaultName = settings.role === "architect" ? "Sol" : "Luna";
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-canvas/75 p-3">
      <section ref={modal} tabIndex={-1} role="dialog" aria-modal="true" aria-labelledby="model-editor-title" className="w-full max-w-md rounded-lg border border-line bg-panel p-4 shadow-xl outline-none">
        <h3 id="model-editor-title" className="text-[15px] font-medium">Change model for {settings.name}</h3>
        <label htmlFor="model-selection" className="mt-3 block text-[12px] text-muted">Model selection</label>
        <select
          ref={select}
          id="model-selection"
          value={value}
          disabled={pending}
          onChange={(e) => { setValue(e.target.value); setSaved(false); }}
          className="mt-1 h-10 w-full max-w-full rounded-md border border-line bg-canvas px-2 font-mono text-[12px] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60"
        >
          <option value={ROLE_DEFAULT}>Role default ({defaultName})</option>
          {options.map((option) => (
            <option key={option.ref} value={option.ref} disabled={!option.usable}>
              {option.ref} — {option.name}{option.usable ? "" : ` (unavailable: ${option.reason || "not authenticated"})`}
            </option>
          ))}
        </select>
        {error && <p role="alert" className="mt-2 break-words font-mono text-[12px] text-bad">{error}</p>}
        {saved && (
          <p role="status" className="mt-2 text-[12px] text-good">
            {settings.restart_required ? "Saved. Restart the bot for this change to take effect." : "Saved."}
          </p>
        )}
        <div className="mt-4 flex justify-end gap-2">
          <Button variant="ghost" disabled={pending} onClick={onClose}>Cancel</Button>
          <Button disabled={pending} onClick={() => void submit()}>{pending ? "Saving…" : "Save"}</Button>
        </div>
      </section>
    </div>
  );
}

function State({ member }: { member: FleetMember }) {
  switch (member.state) {
    case "working": return <span className="inline-flex items-center gap-1 text-[12px]"><RunDot /> working</span>;
    case "stopped": return <Badge className="justify-self-start border-bad/40 text-bad">stopped</Badge>;
    case "idle": return <span className="text-[12px] text-muted">idle</span>;
    default: return <span className="text-[12px] text-muted">-</span>;
  }
}

function heartbeatOf(live?: LiveBot): string {
  if (!live?.running || !live.heartbeat) return "-";
  return `${live.heartbeat} (t${live.tier})`;
}

function nextJobOf(live?: LiveBot): string {
  if (!live?.next_job || !live.next_run) return "-";
  const at = new Date(live.next_run);
  if (Number.isNaN(at.getTime())) return live.next_job;
  return `${live.next_job} ${when(at, new Date())}`;
}

const WEEK_MS = 7 * 24 * 60 * 60 * 1000;

function when(at: Date, now: Date): string {
  if (at.getTime() <= now.getTime()) return "overdue";
  const clock = at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  const sameDay = at.getFullYear() === now.getFullYear() && at.getMonth() === now.getMonth() && at.getDate() === now.getDate();
  if (sameDay) return clock;
  if (at.getTime() - now.getTime() < WEEK_MS) return `${at.toLocaleDateString([], { weekday: "short" })} ${clock}`;
  return `${at.toLocaleDateString([], { month: "short", day: "numeric" })} ${clock}`;
}
