import { useCallback, useEffect, useMemo, useReducer, useRef } from "react";
import { api } from "./protocol";
import type { Event } from "./protocol";
import { empty as emptySession, sessionReducer, type Item } from "./session";
import type { Frame, Page, Turn } from "./chatprotocol";

/** What a bot did inside one turn, as this client holds it.
 *
 *  The raw events are kept rather than a folded state, and the fold is done
 *  where a trace is actually drawn. A working thread produces hundreds of
 *  events a minute and almost all of them belong to a trace nobody has opened;
 *  pushing onto an array is what that costs, instead of a reducer pass and a
 *  new object per frame. */
export interface TurnTrace {
  /** Oldest first, deduplicated by sequence. The socket and a backfill both
   *  deliver the same event, which is the ordinary case rather than the
   *  exception: the fetch is issued while the socket is already live. */
  events: Event[];
  /** True once this turn's own timeline page has landed. Until then the events
   *  here are only whatever the socket happened to deliver, which is not the
   *  turn and must not be drawn as it. */
  loaded: boolean;
  /** What to pass as the next `since`, when the turn did not fit in one page. */
  cursor: number;
  more: boolean;
}

/** How many turns of raw events one thread keeps, of those nobody is reading.
 *
 *  A trace is dropped when its turn ends and nobody has it open; this bounds the
 *  other case, an operator who leaves a busy thread on screen for an hour.
 *  Dropping an unread trace costs nothing - reopening it refetches, which is
 *  what opening any historical turn already does.
 *
 *  It is not a bound on the whole store: an expanded trace is never evicted,
 *  because pulling the events out from under something on screen is worse than
 *  holding them. Twenty traces deliberately left open hold twenty traces. */
const MAX_HELD = 12;

export interface TraceState {
  /** Keyed by turn sequence. */
  turns: Record<number, TurnTrace>;
  /** The turns whose trace is expanded on screen, so they survive the cap and
   *  the drop on turn_end. */
  open: number[];
}

export const emptyTrace: TraceState = { turns: {}, open: [] };

export type TraceAction =
  | { t: "reset" }
  | { t: "resumed" }
  | { t: "live"; turn: number; ev: Event }
  | { t: "page"; turn: number; page: Page<Frame> }
  | { t: "toggle"; turn: number }
  | { t: "ended"; turn: number };

const emptyHeld: TurnTrace = { events: [], loaded: false, cursor: 0, more: false };

/** merge folds events in by sequence, oldest first.
 *
 *  The single-event append is special-cased because it is the hot path and the
 *  general one is not cheap: a live frame arrives in order, and rebuilding a Map
 *  of the whole turn and re-sorting it for each is O(n log n) per frame against
 *  a list that grows all run. */
function merge(items: Event[], incoming: Event[]): Event[] {
  if (incoming.length === 0) return items;
  if (incoming.length === 1) {
    const last = items[items.length - 1];
    const seq = incoming[0].seq ?? 0;
    if (!last || (last.seq ?? 0) < seq) return [...items, incoming[0]];
    // Out of order or already held: fall through to the general path, which
    // deduplicates. Neither happens on the socket, but a backfill overlapping
    // what the socket delivered is the ordinary case.
  }
  const bySeq = new Map(items.map((e) => [e.seq, e]));
  let added = false;
  for (const e of incoming) {
    if (!bySeq.has(e.seq)) {
      bySeq.set(e.seq, e);
      added = true;
    }
  }
  if (!added) return items;
  return [...bySeq.values()].sort((a, b) => (a.seq ?? 0) - (b.seq ?? 0));
}

/** evict keeps the newest MAX_HELD turns, never dropping one that is open. */
function evict(turns: Record<number, TurnTrace>, open: number[]): Record<number, TurnTrace> {
  const keys = Object.keys(turns).map(Number);
  if (keys.length <= MAX_HELD) return turns;
  const droppable = keys.filter((k) => !open.includes(k)).sort((a, b) => a - b);
  const out = { ...turns };
  for (const k of droppable) {
    if (Object.keys(out).length <= MAX_HELD) break;
    delete out[k];
  }
  return out;
}

export function traceReducer(s: TraceState, a: TraceAction): TraceState {
  switch (a.t) {
    case "reset":
      return emptyTrace;

    // A reconnect replays messages, threads and tombstones - never timeline
    // events. So a gap the socket dropped is gone for good, and a trace that
    // had already latched `loaded` would keep it forever: a tool card stuck
    // "running" inside a run that ended, with no button to load the rest.
    //
    // Everything held is dropped, including what is open - the events are what
    // has the hole in them, so keeping them is keeping the hole. useTraces
    // refetches whatever is still open, which is what puts the skeleton back
    // rather than leaving a stale stream on screen.
    case "resumed":
      return s.open.length === 0 && Object.keys(s.turns).length === 0
        ? s
        : { ...s, turns: {} };

    case "live": {
      // A frame that names no turn is not a step of one - nothing produces one
      // today, and filing it under "the newest turn" would attribute one bot's
      // work to another the moment two of them are working in a thread.
      if (!a.turn) return s;
      const held = s.turns[a.turn] ?? emptyHeld;
      const events = merge(held.events, [a.ev]);
      if (events === held.events) return s;
      const turns = { ...s.turns, [a.turn]: { ...held, events } };
      return { ...s, turns: evict(turns, s.open) };
    }

    case "page": {
      const held = s.turns[a.turn] ?? emptyHeld;
      const items = a.page.items.map((f) => f.event).filter((e): e is Event => !!e);
      // Paging runs forwards here, so the cursor only ever moves further on.
      // Compared rather than assigned: collapsing and re-opening a turn
      // refetches page one, and taking its cursor would wind the reader back
      // over everything they had already paged in - so the next "More steps"
      // would refetch a page they already hold and appear to do nothing.
      // chatReducer guards the mirror image of this for messages.
      const next = a.page.cursor ?? 0;
      const further = next > held.cursor;
      // A page saying there is no more is authoritative in that direction: it
      // read to the end of the stream from wherever it started. One saying
      // there *is* more only speaks for a reader at its own cursor, so a
      // page-one refetch cannot put the button back for pages already held.
      const ended = !a.page.more;
      return {
        ...s,
        turns: evict(
          {
            ...s.turns,
            [a.turn]: {
              events: merge(held.events, items),
              loaded: held.loaded || ended,
              cursor: further ? next : held.cursor,
              more: ended ? false : further || held.more,
            },
          },
          s.open,
        ),
      };
    }

    case "toggle": {
      const open = s.open.includes(a.turn)
        ? s.open.filter((t) => t !== a.turn)
        : [...s.open, a.turn];
      return { ...s, open };
    }

    // A turn that ended and that nobody is reading has said everything it is
    // going to; its counts come from its row from here on, and holding a
    // thousand events to redraw one summary line is what the row exists to
    // avoid.
    case "ended": {
      if (s.open.includes(a.turn) || !s.turns[a.turn]) return s;
      const turns = { ...s.turns };
      delete turns[a.turn];
      return { ...s, turns };
    }
  }
}

/** TRACE_PAGE is the timeline page size. One turn of a long developer run is a
 *  few hundred events; this fetches such a turn in one request and pages the
 *  exceptional ones. */
const TRACE_PAGE = 500;

/** summarise counts a trace the way its row does, for a turn being watched
 *  live. The row's own counters are written by the daemon and only re-read when
 *  the turn ends, so a running turn would otherwise sit at whatever it had
 *  reached when the page last asked. */
export function summarise(events: Event[]): { steps: number; tools: number; files: number } {
  let steps = 0;
  let tools = 0;
  const files = new Set<string>();
  for (const ev of events) {
    if (STEP_KINDS.has(ev.kind)) steps++;
    // An assistant message only when it says something. The agent fires one
    // before every tool batch, and on a round that produced tool calls and no
    // prose the text is empty and nothing is drawn for it.
    if (ev.kind === "assistant_message" && ev.text?.trim()) steps++;
    // The plan write is a step but not a tool, matching what the daemon counts
    // onto the row: `timeline.tsx` renders those calls as a plan rather than as
    // tool cards, so counting them promises a card that is never drawn.
    if ((ev.kind === "tool_start" || ev.kind === "sub_tool_start") && ev.name !== PLAN_TOOL) {
      tools++;
    }
    if (ev.kind === "file_changed" && ev.path) files.add(ev.path);
  }
  return { steps, tools, files: files.size };
}

/** The kinds that unconditionally become a row when a trace is expanded, and so
 *  the ones a step count may include. `assistant_message` is conditional and is
 *  handled beside this; a successful `todo_write` is counted as a step and drawn
 *  as the plan rather than as a tool card, which is the one place a step is not
 *  a row in this list.
 *
 *  It is the same whitelist the daemon writes onto the turn row (`contributes`
 *  in `internal/bot/chatlink/journal.go`), and the two have to agree: this
 *  counts a run being watched, the row counts every other one, and one trace
 *  must not change its own summary when it stops running.
 *
 *  Counting by exception instead put `usage`, `tool_batch` and the `*_end`
 *  events that complete a row into the total, roughly tripling it. */
const STEP_KINDS = new Set([
  "user_message", "tool_start", "sub_tool_start",
  "agent_start", "notice", "error", "budget_exhausted",
]);

/** The tool whose calls are drawn as a plan rather than as tool cards. Spelled
 *  the same in three places by necessity - here, in `timeline.tsx`, and in the
 *  daemon's own counter - because each is a different consumer of one fact. */
const PLAN_TOOL = "todo_write";

/** summaryOf is what a summary line reads: the larger, field by field, of what
 *  the row says and what has actually arrived.
 *
 *  Both are undercounts of the same climbing quantity, and each is short in a
 *  different case. The row is written as the work happens but only re-read when
 *  a turn starts or ends, so a running turn's row is stale. The held events are
 *  only whatever this client was sent - a reader who attached mid-run, or a turn
 *  whose backfill is still paging, holds a fraction of them. Taking the maximum
 *  is the only rule that is never confidently low: it was "1 step" the instant a
 *  reader attached to a run forty steps in. */
export function summaryOf(turn: Turn, held: TurnTrace | undefined) {
  const row = { steps: turn.steps ?? 0, tools: turn.tools ?? 0, files: turn.files ?? 0 };
  if (!held || held.events.length === 0) return row;
  const live = summarise(held.events);
  return {
    steps: Math.max(row.steps, live.steps),
    tools: Math.max(row.tools, live.tools),
    files: Math.max(row.files, live.files),
  };
}

/** traceItems folds a run's events into what the timeline draws. Memoised by the caller: it
 *  is a pass over the whole turn, and it is worth paying only for a trace that
 *  is actually open. */
export function traceItems(events: Event[]): Item[] {
  let state = emptySession;
  for (const ev of events) state = sessionReducer(state, { t: "event", ev });
  const items = state.items;
  // The run's final answer arrives as turn_end, which the session reducer turns
  // into a trailing assistant row. In a session that row is the answer; here the
  // answer is the message this trace hangs under, so drawing it would be the
  // same paragraph twice, a line apart. Intermediate assistant messages stay -
  // those are the run thinking out loud, and nothing else shows them.
  const last = items[items.length - 1];
  return last?.kind === "assistant" ? items.slice(0, -1) : items;
}

/** useTraces holds the open thread's agent timeline.
 *
 *  It is deliberately separate from the chat reducer. Timeline events are the
 *  highest-volume thing on the wire - a fleet mid-turn produces hundreds a
 *  minute - and folding them into the object the inbox re-renders from would
 *  redraw the whole screen for each one. The daemon only sends them for the
 *  thread this client says it is watching, so this state is per-thread and
 *  starts empty on every switch. */
export function useTraces(thread: string | null, onError?: (message: string) => void) {
  const [state, dispatch] = useReducer(traceReducer, emptyTrace);
  const fail = useRef(onError);
  useEffect(() => {
    fail.current = onError;
  }, [onError]);

  // A trace belongs to the thread it was read in. Without this the previous
  // thread's turns stay held, and a turn number from one thread would collide
  // with one from another - they come from a single global sequence, so they
  // cannot collide in practice, but the memory would never be given back.
  // Which thread these traces belong to, for a page still in flight to check
  // against. A ref rather than the closed-over prop: load is rebuilt per thread,
  // but a request issued by the previous one is still running with the old value.
  const open = useRef(thread);
  useEffect(() => {
    open.current = thread;
    dispatch({ t: "reset" });
  }, [thread]);

  // The state the callbacks below read is taken from a ref rather than closed
  // over. They are handed to every trace on screen, and a dependency on the
  // state itself is a new identity per websocket frame - which defeats the memo
  // on AgentTrace and re-renders the whole transcript for every step any bot
  // takes.
  const held = useRef(state);
  useEffect(() => {
    held.current = state;
  }, [state]);

  const live = useCallback((turn: number, ev: Event) => dispatch({ t: "live", turn, ev }), []);
  const ended = useCallback((turn: number) => dispatch({ t: "ended", turn }), []);
  /** load fetches one turn's timeline. It is called on expand rather than on
   *  open: a thread of a hundred turns is thousands of events, and the reader
   *  asked about one run. */
  const load = useCallback(
    async (turn: number, since = 0) => {
      if (!thread) return;
      try {
        const page = await api<Page<Frame>>(
          `/api/chat/threads/${encodeURIComponent(thread)}/timeline` +
            `?turn=${turn}&since=${since}&limit=${TRACE_PAGE}`,
        );
        // The reader may have left while this was in flight. Turn sequences are
        // globally unique, so the events could not be drawn under the wrong
        // thread today - but they would be held until eviction, and the guard
        // is what keeps that true if turns are ever numbered per thread.
        if (open.current !== thread) return;
        dispatch({ t: "page", turn, page });
      } catch (e) {
        if (open.current !== thread) return;
        fail.current?.(e instanceof Error ? e.message : String(e));
      }
    },
    [thread],
  );

  const toggle = useCallback(
    (turn: number) => {
      const trace = held.current.turns[turn];
      const opening = !held.current.open.includes(turn);
      dispatch({ t: "toggle", turn });
      // Always on the way in, even when live events are already held: those
      // start wherever the reader attached, and the beginning of the run is
      // exactly what they opened it to see.
      if (opening && !trace?.loaded) void load(turn);
    },
    [load],
  );

  /** resumed is called when the socket reconnects. It drops every held trace -
   *  a resume replays no timeline events, so whatever was held may now have a
   *  gap in it - and refetches the ones still on screen, which is what puts the
   *  skeleton back rather than leaving a stale stream up with no way to fix it. */
  const resumed = useCallback(() => {
    const open = held.current.open;
    dispatch({ t: "resumed" });
    for (const turn of open) void load(turn);
  }, [load]);

  const more = useCallback(
    (turn: number) => {
      const trace = held.current.turns[turn];
      if (trace?.more) void load(turn, trace.cursor);
    },
    [load],
  );

  return useMemo(
    // toggle and more are rebuilt when the state they read changes, which is
    // every frame; the two the socket calls are stable, and AgentTrace takes the
    // turn as an argument rather than closing over it, so a memoised trace is
    // not re-rendered by a frame belonging to a different run.
    () => ({ turns: state.turns, open: state.open, live, ended, resumed, toggle, more }),
    [state.turns, state.open, live, ended, resumed, toggle, more],
  );
}
