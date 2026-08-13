import { useCallback, useEffect, useMemo, useReducer, useRef } from "react";
import { api, token } from "./protocol";
import {
  isClientError,
  type ClientOp,
  type Frame,
  type Incoming,
  type Message,
  type Page,
  type ThreadState,
  type ThreadView,
} from "./chatprotocol";

/** A thread's messages as this client holds them: oldest first, deduplicated by
 *  sequence, with the point older ones are asked for. */
export interface ThreadMessages {
  items: Message[];
  /** What to pass as `before` for the next page of older messages. */
  cursor: number;
  /** Whether older messages exist at all. Told by the daemon rather than
   *  inferred: a page that filled its limit is indistinguishable from the start
   *  of the conversation, and a client that guessed would silently stop. */
  more: boolean;
  /** True once a page has landed. Until then the thread may hold nothing but
   *  whatever the socket delivered while it was being fetched, which is not the
   *  conversation and must not be drawn as it. */
  loaded: boolean;
}

/** A thread as this client holds it, with the point in the stream it was
 *  learned at. The two sources disagree in one direction only - a socket frame
 *  is newer than the HTTP snapshot a request in flight is about to return - so
 *  the seq is what settles it. A rename moves it without moving last_seq, which
 *  is exactly the case comparing last_seq alone would get wrong. */
interface Known {
  view: ThreadView;
  at: number;
}

export interface ChatState {
  threads: Record<string, Known>;
  messages: Record<string, ThreadMessages>;
  /** Threads whose tombstone has been seen. Kept because the HTTP inbox is a
   *  snapshot taken before the delete committed, and without this the response
   *  landing afterwards puts the dead thread back in the rail, where clicking
   *  it answers 404. */
  gone: string[];
  /** The resume point: what a reconnect asks the daemon to replay from. */
  lastSeq: number;
  connected: boolean;
  /** Threads that just began asking for the operator. Drained by whatever
   *  raises a notification, so that firing one is a side effect of reading this
   *  rather than something the reducer does. */
  alerts: string[];
}

export const emptyChat: ChatState = {
  threads: {},
  messages: {},
  gone: [],
  lastSeq: 0,
  connected: false,
  alerts: [],
};

export type ChatAction =
  | { t: "connected"; on: boolean }
  | { t: "frame"; f: Frame }
  | { t: "inbox"; views: ThreadView[] }
  | { t: "opening"; thread: string }
  | { t: "page"; thread: string; page: Page<Message> }
  | { t: "alerted" };

const NEEDS_YOU: ThreadState = "needs_you";

/** merge folds messages in by sequence. Both sources deliver the same message -
 *  the socket while a page is in flight, the page because it committed before
 *  the query - so arriving twice is the ordinary case, not the exception. */
function merge(items: Message[], incoming: Message[]): Message[] {
  if (incoming.length === 0) return items;
  const bySeq = new Map(items.map((m) => [m.seq, m]));
  let added = false;
  for (const m of incoming) {
    if (!bySeq.has(m.seq)) {
      bySeq.set(m.seq, m);
      added = true;
    }
  }
  if (!added) return items;
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

function withThread(s: ChatState, view: ThreadView, at: number): ChatState {
  // A deleted thread stays deleted. The inbox snapshot that still lists it was
  // read before the delete committed, so it is older news than the tombstone
  // however its sequence numbers compare.
  if (s.gone.includes(view.id)) return s;
  const prev = s.threads[view.id];
  if (prev && prev.at > at) return s;
  // Only a transition alerts, and only for a thread already on screen. A thread
  // arriving for the first time already needing an answer is the replay a fresh
  // page load gets, and notifying for each of those is how the notification
  // stops meaning anything.
  const began = !!prev && prev.view.state !== NEEDS_YOU && view.state === NEEDS_YOU;
  return {
    ...s,
    threads: { ...s.threads, [view.id]: { view, at } },
    alerts: began ? [...s.alerts, view.id] : s.alerts,
  };
}

function withoutThread(s: ChatState, id: string): ChatState {
  const threads = { ...s.threads };
  const messages = { ...s.messages };
  delete threads[id];
  delete messages[id];
  return {
    ...s,
    threads,
    messages,
    gone: s.gone.includes(id) ? s.gone : [...s.gone, id],
    alerts: s.alerts.filter((a) => a !== id),
  };
}

export function chatReducer(s: ChatState, a: ChatAction): ChatState {
  switch (a.t) {
    case "connected":
      return { ...s, connected: a.on };

    case "alerted":
      return s.alerts.length === 0 ? s : { ...s, alerts: [] };

    case "inbox": {
      let next = s;
      for (const view of a.views) next = withThread(next, view, view.last_seq);
      return next;
    }

    case "opening": {
      if (s.messages[a.thread]) return s;
      // The entry exists before the request is made, so a message committed
      // between the query and its answer is kept rather than dropped for
      // belonging to a thread this client had not admitted to watching yet.
      return {
        ...s,
        messages: { ...s.messages, [a.thread]: { items: [], cursor: 0, more: false, loaded: false } },
      };
    }

    case "page": {
      const held = s.messages[a.thread] ?? { items: [], cursor: 0, more: false, loaded: false };
      const items = merge(held.items, a.page.items);
      // Paging runs backwards, so the cursor only ever moves further back.
      // Compared rather than assumed: re-opening a thread refetches page one,
      // and taking its cursor would wind the reader forward past everything
      // they had already paged in - putting the "Older messages" button back
      // for messages that are already on screen.
      const deeper = !held.loaded || (a.page.cursor ?? 0) < held.cursor;
      return {
        ...s,
        messages: {
          ...s.messages,
          [a.thread]: {
            items,
            cursor: deeper ? (a.page.cursor ?? 0) : held.cursor,
            more: deeper ? (a.page.more ?? false) : held.more,
            loaded: true,
          },
        },
      };
    }

    case "frame": {
      const f = a.f;
      // A desync or a truncated backlog is an instruction about the cursor, not
      // something that happened in a conversation. It never reaches the state
      // beyond moving the resume point, which the socket reads back.
      if (f.stream === "desync" || f.stream === "truncated") {
        return { ...s, lastSeq: f.from ?? s.lastSeq };
      }
      // A frame this screen draws nothing from returns the state it was given,
      // identity and all. Anything else is a re-render of the whole screen per
      // frame, and a fleet mid-turn produces hundreds a minute - the timeline
      // events nothing renders until stage 7, and every message belonging to a
      // thread nobody has open.
      //
      // The resume point survives: the socket resumes from its own ref, which
      // is advanced for every frame including the ones ignored here.
      if (f.stream === "event") return s;
      if (f.stream === "thread") {
        const s2: ChatState = { ...s, lastSeq: Math.max(s.lastSeq, f.seq) };
        if (!f.thr) return f.thread ? withoutThread(s2, f.thread) : s2;
        return withThread(s2, f.thr, f.seq);
      }
      if (f.stream === "message" && f.msg) {
        const held = s.messages[f.msg.thread];
        // Only threads this client opened are held. The inbox row stays fresh
        // regardless: a thread frame rides with every message.
        if (!held) return s;
        return {
          ...s,
          lastSeq: Math.max(s.lastSeq, f.seq),
          messages: { ...s.messages, [f.msg.thread]: { ...held, items: merge(held.items, [f.msg]) } },
        };
      }
      return s;
    }
  }
}

/** inboxOf orders the threads for the rail: newest activity first, archived
 *  ones only when asked for. Sorting here rather than in the store is what lets
 *  a live frame reorder the list without a refetch. */
export function inboxOf(s: ChatState, state?: ThreadState, archived = false): ThreadView[] {
  return Object.values(s.threads)
    .map((k) => k.view)
    .filter((v) => !!v.archived === archived && (!state || v.state === state))
    .sort((a, b) => b.last_seq - a.last_seq);
}

/** countsOf tallies the filter chips from the same data the rows are drawn
 *  from, so a chip can never disagree with the list under it. */
export function countsOf(s: ChatState): Record<ThreadState, number> {
  const out: Record<ThreadState, number> = { needs_you: 0, working: 0, waiting: 0, idle: 0 };
  for (const { view } of Object.values(s.threads)) {
    if (!view.archived) out[view.state] += 1;
  }
  return out;
}

const MESSAGE_PAGE = 100;

/** useChat keeps one socket for the whole screen and reconnects with the last
 *  sequence number it saw.
 *
 *  One socket, not one per thread: a phone gets a reconnect storm per socket,
 *  and the inbox has to stay live while a thread is open. Messages and thread
 *  rows arrive for every conversation the operator is in; the agent timeline
 *  arrives only for the one the client says it is watching. */
export function useChat(onRefused?: (message: string) => void) {
  const [state, dispatch] = useReducer(chatReducer, emptyChat);
  // Held in a ref so the socket's callbacks, which are built once, always reach
  // the current one rather than the one that existed at mount.
  const refused = useRef(onRefused);
  useEffect(() => {
    refused.current = onRefused;
  }, [onRefused]);
  const sock = useRef<WebSocket | null>(null);
  // The resume point lives in a ref rather than in the reducer state the socket
  // could read: the socket is opened once and reconnects from a timer, and a
  // value closed over at mount would send every reconnect back to zero.
  const seq = useRef(0);
  // Likewise the watched thread. The daemon builds a fresh client watching
  // nothing on every attach, and this client closes its own socket on a desync
  // or a truncated backlog - so a reconnect is the ordinary path, not just what
  // happens when the network drops.
  const watching = useRef<string | null>(null);

  // A thread that was deleted is not something to keep asking to watch: every
  // later reconnect would re-send the claim for an id the daemon no longer has.
  const gone = state.gone;
  useEffect(() => {
    if (watching.current && gone.includes(watching.current)) watching.current = null;
  }, [gone]);

  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;
    let attempt = 0;

    const connect = () => {
      if (cancelled) return;
      const proto = window.location.protocol === "https:" ? "wss" : "ws";
      const url =
        `${proto}://${window.location.host}/api/chat/socket` +
        `?token=${encodeURIComponent(token())}&since=${seq.current}`;
      const ws = new WebSocket(url);
      sock.current = ws;

      ws.onopen = () => {
        if (cancelled || sock.current !== ws) return;
        attempt = 0;
        // The new connection watches nothing until it is told again, and
        // without this the thread on screen silently stops receiving the work
        // going on inside it.
        if (watching.current) {
          ws.send(JSON.stringify({ op: "watch", thread: watching.current } satisfies ClientOp));
        }
        dispatch({ t: "connected", on: true });
      };
      ws.onmessage = (m) => {
        if (cancelled || sock.current !== ws) return;
        const msg = JSON.parse(m.data as string) as Incoming;
        // A rejection of this client's own op did not happen in any
        // conversation, so it never joins one - but it is still the answer to
        // something the operator did. Saying nothing here is how a message the
        // daemon refused (the thread was deleted, the operator is no longer in
        // it) looks exactly like one it accepted.
        if (isClientError(msg)) {
          refused.current?.(msg.error);
          return;
        }
        if (msg.stream === "desync" || msg.stream === "truncated") {
          seq.current = msg.from ?? seq.current;
          dispatch({ t: "frame", f: msg });
          // Both mean "ask again from here". Reconnecting is how: the daemon
          // replays from the cursor, and everything it sends is deduplicated by
          // sequence on the way in.
          ws.close();
          return;
        }
        seq.current = Math.max(seq.current, msg.seq);
        dispatch({ t: "frame", f: msg });
      };
      ws.onclose = () => {
        if (cancelled || sock.current !== ws) return;
        dispatch({ t: "connected", on: false });
        // Back off, but never so far that a phone waking up sits disconnected.
        const wait = Math.min(5000, 250 * 2 ** attempt++);
        timer = window.setTimeout(connect, wait);
      };
      ws.onerror = () => {
        if (cancelled || sock.current !== ws) return;
        ws.close();
      };
    };

    connect();
    return () => {
      cancelled = true;
      if (timer) window.clearTimeout(timer);
      sock.current?.close();
      sock.current = null;
    };
  }, []);

  /** send writes an op, and reports whether it went. A socket mid-reconnect
   *  swallows a write, and a caller that assumed otherwise clears a composer
   *  for a message that never existed. */
  const send = useCallback((op: ClientOp): boolean => {
    const ws = sock.current;
    if (!ws || ws.readyState !== WebSocket.OPEN) return false;
    ws.send(JSON.stringify(op));
    return true;
  }, []);

  const refresh = useCallback(async () => {
    const views = await api<ThreadView[]>("/api/chat/threads");
    dispatch({ t: "inbox", views });
    return views;
  }, []);

  /** archived fetches the threads that are done. Separate from the inbox and
   *  only on request: they are the ones deliberately out of the way, and the
   *  rail exists to answer "what requires me". */
  const archived = useCallback(async () => {
    const views = await api<ThreadView[]>("/api/chat/threads?archived=true");
    dispatch({ t: "inbox", views });
    return views;
  }, []);

  /** open declares the thread being read: the timeline follows it, and the
   *  first page of messages is fetched. The declaration goes up before the
   *  fetch, so nothing said in between is lost. */
  const open = useCallback(
    async (thread: string) => {
      dispatch({ t: "opening", thread });
      watching.current = thread;
      send({ op: "watch", thread });
      const page = await api<Page<Message>>(
        `/api/chat/threads/${encodeURIComponent(thread)}/messages?limit=${MESSAGE_PAGE}`,
      );
      dispatch({ t: "page", thread, page });
    },
    [send],
  );

  const older = useCallback(async (thread: string, before: number) => {
    const page = await api<Page<Message>>(
      `/api/chat/threads/${encodeURIComponent(thread)}/messages` +
        `?limit=${MESSAGE_PAGE}&before=${before}`,
    );
    dispatch({ t: "page", thread, page });
  }, []);

  const say = useCallback(
    (thread: string, text: string, mentions?: string[]): boolean =>
      send({ op: "send", thread, text, mentions }),
    [send],
  );

  const markRead = useCallback((thread: string, at: number) => send({ op: "read", thread, seq: at }), [send]);

  const create = useCallback(
    async (title: string, participants: string[], text: string) => {
      const view = await api<ThreadView>("/api/chat/threads", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title, participants, text }),
      });
      dispatch({ t: "inbox", views: [view] });
      return view;
    },
    [],
  );

  const alerted = useCallback(() => dispatch({ t: "alerted" }), []);

  return useMemo(
    () => ({ state, refresh, archived, open, older, say, markRead, create, alerted }),
    [state, refresh, archived, open, older, say, markRead, create, alerted],
  );
}
