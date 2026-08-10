import { useCallback, useEffect, useReducer, useRef } from "react";
import type { Approval, Client, Decision, Event, Todo } from "./protocol";
import { token } from "./protocol";

/** A rendered entry. The event stream is a log; this is what a reader sees. */
export type Item =
  | { kind: "user"; seq: number; text: string; images: number; injected: boolean }
  | { kind: "assistant"; seq: number; text: string; streaming: boolean }
  | { kind: "tool"; seq: number; id: string; name: string; args: unknown; result?: string; error?: string; bytes?: number; blob?: boolean; done: boolean }
  | { kind: "run"; seq: number; id: string; agent: string; prompt: string; done: boolean; failed: boolean; steps: RunStep[] }
  | { kind: "notice"; seq: number; text: string; tone: "note" | "error" };

export interface RunStep { id: string; name: string; result?: string; error?: string; done: boolean }

export interface State {
  items: Item[];
  todos: Todo[];
  tokens: number;
  ctx: number;
  title: string;
  model: string;
  running: boolean;
  approval: { id: string; req: Approval } | null;
  clients: Client[];
  lastSeq: number;
  connected: boolean;
  /** files touched this session, newest first */
  files: string[];
}

export const empty: State = {
  items: [], todos: [], tokens: 0, ctx: 0, title: "", model: "", running: false,
  approval: null, clients: [], lastSeq: 0, connected: false, files: [],
};

type Action =
  | { t: "event"; ev: Event }
  | { t: "connected"; on: boolean }
  | { t: "reset" };

/** find the last item matching a predicate, for the streaming tail and for
 *  attaching a result to the call that produced it. */
function patch(items: Item[], i: number, next: Item): Item[] {
  const out = items.slice();
  out[i] = next;
  return out;
}

function indexOfTool(items: Item[], id: string): number {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i];
    if (it.kind === "tool" && it.id === id && !it.done) return i;
  }
  return -1;
}

function indexOfRun(items: Item[], id: string): number {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i];
    if (it.kind === "run" && it.id === id) return i;
  }
  return -1;
}

function reduce(s: State, a: Action): State {
  if (a.t === "connected") return { ...s, connected: a.on };
  if (a.t === "reset") return { ...empty, connected: s.connected };

  const ev = a.ev;
  const s2: State = { ...s, lastSeq: Math.max(s.lastSeq, ev.seq ?? 0) };

  switch (ev.kind) {
    case "user_message":
      return {
        ...s2,
        items: [...s2.items, {
          kind: "user", seq: ev.seq, text: ev.text ?? "",
          images: ev.images ?? 0, injected: !!ev.injected,
        }],
      };

    case "turn_start":
      return { ...s2, running: true };

    case "content": {
      const last = s2.items[s2.items.length - 1];
      if (last?.kind === "assistant" && last.streaming) {
        return { ...s2, items: patch(s2.items, s2.items.length - 1, { ...last, text: last.text + (ev.text ?? "") }) };
      }
      return { ...s2, items: [...s2.items, { kind: "assistant", seq: ev.seq, text: ev.text ?? "", streaming: true }] };
    }

    // An intermediate answer is committed where it happened, so it does not
    // linger under the tool output that follows it.
    case "assistant_message": {
      const items = s2.items.slice();
      const last = items[items.length - 1];
      if (last?.kind === "assistant" && last.streaming) items.pop();
      const text = (ev.text ?? "").trim();
      if (!text) return { ...s2, items };
      return { ...s2, items: [...items, { kind: "assistant", seq: ev.seq, text, streaming: false }] };
    }

    case "turn_end": {
      const items = s2.items.slice();
      const last = items[items.length - 1];
      if (last?.kind === "assistant" && last.streaming) {
        items[items.length - 1] = { ...last, streaming: false, text: ev.text?.trim() ? ev.text : last.text };
      } else if (ev.text?.trim()) {
        items.push({ kind: "assistant", seq: ev.seq, text: ev.text, streaming: false });
      }
      if (ev.interrupted) items.push({ kind: "notice", seq: ev.seq, text: "interrupted", tone: "note" });
      else if (ev.error) items.push({ kind: "notice", seq: ev.seq, text: ev.error, tone: "error" });
      return { ...s2, items, running: false };
    }

    case "tool_start":
      return {
        ...s2,
        items: [...s2.items, {
          kind: "tool", seq: ev.seq, id: ev.id ?? "", name: ev.name ?? "",
          args: ev.args, done: false,
        }],
      };

    case "tool_end": {
      const i = indexOfTool(s2.items, ev.id ?? "");
      if (i < 0) return s2;
      const it = s2.items[i];
      if (it.kind !== "tool") return s2;
      return {
        ...s2,
        items: patch(s2.items, i, {
          ...it, done: true, result: ev.text, error: ev.error,
          bytes: ev.bytes, blob: ev.blob,
        }),
      };
    }

    case "agent_start":
      return {
        ...s2,
        items: [...s2.items, {
          kind: "run", seq: ev.seq, id: ev.id ?? "", agent: ev.agent ?? "",
          prompt: ev.text ?? "", done: false, failed: false, steps: [],
        }],
      };

    case "agent_end": {
      const i = indexOfRun(s2.items, ev.id ?? "");
      if (i < 0) return s2;
      const it = s2.items[i];
      if (it.kind !== "run") return s2;
      return { ...s2, items: patch(s2.items, i, { ...it, done: true, failed: !!ev.error }) };
    }

    // Nested calls are identified by run id and call id together: a call id is
    // only unique inside its own run.
    case "sub_tool_start": {
      const i = indexOfRun(s2.items, ev.run_id ?? "");
      if (i < 0) return s2;
      const it = s2.items[i];
      if (it.kind !== "run") return s2;
      return {
        ...s2,
        items: patch(s2.items, i, {
          ...it, steps: [...it.steps, { id: ev.id ?? "", name: ev.name ?? "", done: false }],
        }),
      };
    }

    case "sub_tool_end": {
      const i = indexOfRun(s2.items, ev.run_id ?? "");
      if (i < 0) return s2;
      const it = s2.items[i];
      if (it.kind !== "run") return s2;
      const steps = it.steps.slice();
      for (let k = steps.length - 1; k >= 0; k--) {
        if (steps[k].id === ev.id && !steps[k].done) {
          steps[k] = { ...steps[k], done: true, result: ev.text, error: ev.error };
          break;
        }
      }
      return { ...s2, items: patch(s2.items, i, { ...it, steps }) };
    }

    case "sub_notice":
      return s2;

    case "notice":
      return { ...s2, items: [...s2.items, { kind: "notice", seq: ev.seq, text: ev.text ?? "", tone: "note" }] };

    case "error":
    case "budget_exhausted":
      return { ...s2, items: [...s2.items, { kind: "notice", seq: ev.seq, text: ev.text ?? ev.error ?? "", tone: "error" }] };

    case "usage":
      return { ...s2, tokens: ev.tokens ?? 0 };

    case "todo":
      return { ...s2, todos: ev.todos ?? [] };

    case "session_meta":
      return { ...s2, title: ev.text ?? s2.title, model: ev.name ?? s2.model, ctx: ev.ctx ?? s2.ctx };

    case "approval_request":
      return ev.approval ? { ...s2, approval: { id: ev.id ?? "", req: ev.approval } } : s2;

    case "approval_resolved":
      return { ...s2, approval: null };

    case "presence":
      return { ...s2, clients: ev.clients ?? [] };

    case "file_changed": {
      const p = ev.path ?? "";
      return { ...s2, files: [p, ...s2.files.filter((f) => f !== p)] };
    }

    default:
      return s2;
  }
}

/** useSession keeps one socket open and reconnects with the last sequence
 *  number it saw. A dropped connection is normal on a phone; losing the middle
 *  of a conversation to one is not, so the daemon replays the gap. */
export function useSession(id: string | null) {
  const [state, dispatch] = useReducer(reduce, empty);
  const sock = useRef<WebSocket | null>(null);
  const seq = useRef(0);
  const gone = useRef(false);

  seq.current = state.lastSeq;

  useEffect(() => {
    if (!id) return;
    gone.current = false;
    let timer: number | undefined;
    let attempt = 0;

    const connect = () => {
      if (gone.current) return;
      const proto = window.location.protocol === "https:" ? "wss" : "ws";
      const url = `${proto}://${window.location.host}/api/sessions/${id}/socket` +
        `?token=${encodeURIComponent(token())}&since=${seq.current}&kind=web`;
      const ws = new WebSocket(url);
      sock.current = ws;

      ws.onopen = () => { attempt = 0; dispatch({ t: "connected", on: true }); };
      ws.onmessage = (m) => {
        const ev = JSON.parse(m.data as string) as Event;
        // A server-side error reply is about this client's request, not about
        // the conversation, so it is surfaced without joining the timeline.
        if ((ev as unknown as { error?: string }).error && !ev.kind) return;
        if (ev.kind === "desync") {
          seq.current = ev.from ?? seq.current;
          ws.close();
          return;
        }
        dispatch({ t: "event", ev });
      };
      ws.onclose = () => {
        dispatch({ t: "connected", on: false });
        if (gone.current) return;
        // Back off, but never so far that a phone waking up sits disconnected.
        const wait = Math.min(5000, 250 * 2 ** attempt++);
        timer = window.setTimeout(connect, wait);
      };
      ws.onerror = () => ws.close();
    };

    connect();
    return () => {
      gone.current = true;
      if (timer) window.clearTimeout(timer);
      sock.current?.close();
      sock.current = null;
      dispatch({ t: "reset" });
    };
  }, [id]);

  const send = useCallback((op: Record<string, unknown>) => {
    const ws = sock.current;
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(op));
  }, []);

  const submit = useCallback((text: string) => send({ op: "submit", text }), [send]);
  const interrupt = useCallback(() => send({ op: "interrupt" }), [send]);
  const resolve = useCallback(
    (approvalID: string, decision: Decision) => send({ op: "resolve", id: approvalID, decision, label: "web" }),
    [send],
  );
  const command = useCallback((name: string, args = "") => send({ op: "command", name, args }), [send]);

  return { state, submit, interrupt, resolve, command };
}
