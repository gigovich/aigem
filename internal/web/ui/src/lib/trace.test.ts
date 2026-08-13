import { describe, expect, it } from "vitest";
import type { Event } from "./protocol";
import type { Frame } from "./chatprotocol";
import {
  emptyTrace,
  summaryOf,
  summarise,
  traceItems,
  traceReducer,
  type TraceState,
} from "./trace";

function ev(seq: number, kind: string, extra: Partial<Event> = {}): Event {
  return { seq, kind, ...extra } as Event;
}

function fold(actions: Parameters<typeof traceReducer>[1][]): TraceState {
  return actions.reduce(traceReducer, emptyTrace);
}

describe("traceReducer", () => {
  it("files a live event under the run the frame named", () => {
    const s = fold([{ t: "live", turn: 7, ev: ev(10, "tool_start") }]);
    expect(s.turns[7].events).toHaveLength(1);
  });

  it("drops an event that names no run", () => {
    // Filing it under "the newest turn" would attribute one bot's work to
    // another the moment two of them are working in a thread at once.
    const s = fold([{ t: "live", turn: 0, ev: ev(10, "tool_start") }]);
    expect(s.turns).toEqual({});
  });

  it("deduplicates by sequence across the socket and the backfill", () => {
    // Both deliver the same event, and that is the ordinary case: the fetch is
    // issued while the socket is already live.
    const page: Frame = { seq: 10, stream: "event", event: ev(10, "tool_start") };
    const s = fold([
      { t: "live", turn: 7, ev: ev(10, "tool_start") },
      { t: "page", turn: 7, page: { items: [page] } },
    ]);
    expect(s.turns[7].events).toHaveLength(1);
  });

  it("returns the same state for an event it already holds", () => {
    // Identity matters: a new object per frame is a re-render, and a fleet
    // mid-turn produces hundreds a minute.
    const first = fold([{ t: "live", turn: 7, ev: ev(10, "tool_start") }]);
    const again = traceReducer(first, { t: "live", turn: 7, ev: ev(10, "tool_start") });
    expect(again).toBe(first);
  });

  it("sorts a backfill under live events that arrived first", () => {
    const older: Frame[] = [
      { seq: 1, stream: "event", event: ev(1, "turn_start") },
      { seq: 2, stream: "event", event: ev(2, "tool_start") },
    ];
    const s = fold([
      { t: "live", turn: 7, ev: ev(9, "tool_end") },
      { t: "page", turn: 7, page: { items: older } },
    ]);
    expect(s.turns[7].events.map((e) => e.seq)).toEqual([1, 2, 9]);
  });

  it("is only loaded once the last page has landed, and stops offering more", () => {
    const s = fold([
      { t: "page", turn: 7, page: { items: [], cursor: 40, more: true } },
    ]);
    expect(s.turns[7].loaded).toBe(false);
    expect(s.turns[7].cursor).toBe(40);
    expect(s.turns[7].more).toBe(true);

    // A page saying there is no more is authoritative: it read to the end of
    // the stream. Leaving `more` set left a "More steps" button that refetched
    // a page already held and appeared to do nothing.
    const done = traceReducer(s, { t: "page", turn: 7, page: { items: [] } });
    expect(done.turns[7].loaded).toBe(true);
    expect(done.turns[7].more).toBe(false);
  });

  it("does not put the button back when page one is refetched", () => {
    // Collapsing and re-opening refetches from the start, and that page still
    // says there is more - but only for a reader at its own cursor.
    const paged = fold([
      { t: "page", turn: 7, page: { items: [], cursor: 40, more: true } },
      { t: "page", turn: 7, page: { items: [], cursor: 90, more: false } },
    ]);
    expect(paged.turns[7].more).toBe(false);
    const again = traceReducer(paged, {
      t: "page", turn: 7, page: { items: [], cursor: 40, more: true },
    });
    expect(again.turns[7].more).toBe(false);
    expect(again.turns[7].cursor).toBe(90);
  });

  it("drops everything held on a resume, so what is on screen refetches", () => {
    // A resume replays messages and threads but never timeline events, so a
    // trace held across one may have a gap nothing will ever fill: a tool card
    // stuck "running" inside a run that ended, with no button to load the rest.
    const s = fold([
      { t: "live", turn: 7, ev: ev(10, "tool_start", { name: "grep" }) },
      { t: "toggle", turn: 7 },
      { t: "live", turn: 8, ev: ev(11, "tool_start", { name: "bash" }) },
      { t: "resumed" },
    ]);
    expect(s.turns).toEqual({});
    // The open one stays open - useTraces refetches it, which is what puts the
    // skeleton back rather than leaving a stale stream up.
    expect(s.open).toEqual([7]);
  });

  it("drops a finished run nobody is reading, and keeps an open one", () => {
    const base = fold([
      { t: "live", turn: 7, ev: ev(10, "tool_start") },
      { t: "live", turn: 8, ev: ev(11, "tool_start") },
      { t: "toggle", turn: 8 },
    ]);
    const after = fold([
      { t: "live", turn: 7, ev: ev(10, "tool_start") },
      { t: "live", turn: 8, ev: ev(11, "tool_start") },
      { t: "toggle", turn: 8 },
      { t: "ended", turn: 7 },
      { t: "ended", turn: 8 },
    ]);
    expect(base.turns[7]).toBeDefined();
    expect(after.turns[7]).toBeUndefined();
    expect(after.turns[8]).toBeDefined();
  });

  it("evicts the oldest runs past the cap but never an open one", () => {
    const actions: Parameters<typeof traceReducer>[1][] = [{ t: "toggle", turn: 1 }];
    for (let turn = 1; turn <= 20; turn++) {
      actions.push({ t: "live", turn, ev: ev(turn, "tool_start") });
    }
    const s = fold(actions);
    expect(Object.keys(s.turns).length).toBeLessThanOrEqual(12);
    // The one being read survives, however old it is.
    expect(s.turns[1]).toBeDefined();
    expect(s.turns[20]).toBeDefined();
  });
});

describe("summarise", () => {
  it("counts only the events that become a row in the trace", () => {
    // Not "everything except the brackets": usage fires twice per model round
    // and tool_batch once, and neither draws anything, so counting by exception
    // announced roughly three times the rows the reader can actually see.
    const got = summarise([
      ev(1, "turn_start"),
      ev(2, "usage", { tokens: 100 }),
      ev(3, "tool_batch", { round: 1 }),
      ev(4, "tool_start", { name: "grep" }),
      ev(5, "tool_end", { name: "grep" }),
      ev(6, "sub_tool_start", { name: "bash" }),
      ev(7, "assistant_message", { text: "looking at the rotation" }),
      ev(8, "turn_end", { text: "Reproduced." }),
    ]);
    // Three: the two tool calls and the one assistant message that said
    // something. turn_end is the run's closing bracket, and the answer it
    // carries is drawn as the message the trace hangs under, not twice.
    expect(got).toEqual({ steps: 3, tools: 2, files: 0 });
  });

  it("does not count an assistant message that says nothing", () => {
    // The agent fires one before every tool batch, and on a round that produced
    // tool calls and no prose the text is empty and nothing is drawn - which
    // announced two steps per round for a turn that took one.
    const got = summarise([
      ev(1, "assistant_message", { text: "" }),
      ev(2, "tool_start", { name: "grep" }),
      ev(3, "assistant_message", { text: "   " }),
      ev(4, "tool_start", { name: "bash" }),
    ]);
    expect(got).toEqual({ steps: 2, tools: 2, files: 0 });
  });

  it("counts a plan write as a step but not as a tool", () => {
    // timeline.tsx renders those calls as a plan rather than as tool cards, so
    // counting one promises the reader a card that is never drawn.
    const got = summarise([
      ev(1, "tool_start", { name: "todo_write" }),
      ev(2, "todo"),
      ev(3, "tool_end", { name: "todo_write" }),
    ]);
    expect(got).toEqual({ steps: 1, tools: 0, files: 0 });
  });

  it("counts a file once however often the run rewrote it", () => {
    const got = summarise([
      ev(1, "file_changed", { path: "internal/auth/flow.go" }),
      ev(2, "file_changed", { path: "internal/auth/flow.go" }),
      ev(3, "file_changed", { path: "internal/auth/store.go" }),
    ]);
    expect(got.files).toBe(2);
  });
});

describe("summaryOf", () => {
  const turn = {
    seq: 7, thread: "t_1", actor: "bot:amiran", started: "",
    steps: 14, tools: 6, files: 2,
  };

  it("reads the row when nothing has been watched", () => {
    expect(summaryOf(turn, undefined)).toEqual({ steps: 14, tools: 6, files: 2 });
  });

  it("counts what has arrived once it exceeds the row", () => {
    // The row's counters are only re-read when a turn starts or ends, so a
    // running turn's row is stale-low and the live count is what is true.
    const events = Array.from({ length: 40 }, (_, i) => ev(i + 1, "tool_start", { name: "grep" }));
    expect(summaryOf(turn, { events, loaded: false, cursor: 0, more: false })).toEqual({
      steps: 40, tools: 40, files: 2,
    });
  });

  it("keeps the row when only a fraction of the run has arrived", () => {
    // The other direction, and the one a partial trace hits: a reader who
    // attached mid-run holds two events of a forty-step turn, and reporting
    // "2 steps" over a row that says 14 is confidently wrong.
    const held = {
      events: [ev(1, "tool_start", { name: "grep" }), ev(2, "tool_end")],
      loaded: false, cursor: 0, more: false,
    };
    expect(summaryOf(turn, held)).toEqual({ steps: 14, tools: 6, files: 2 });
  });
});

describe("traceItems", () => {
  it("folds a turn's events into what the timeline draws", () => {
    const items = traceItems([
      ev(1, "turn_start"),
      ev(2, "tool_start", { id: "c1", name: "grep" }),
      ev(3, "tool_end", { id: "c1", name: "grep", text: "internal/auth/flow.go:88" }),
      ev(4, "turn_end", { text: "Reproduced." }),
    ]);
    // The answer is left out: it is the message this trace hangs under, and
    // drawing it here put the same paragraph on screen twice, a line apart.
    expect(items.map((i) => i.kind)).toEqual(["tool"]);
    const tool = items[0];
    expect(tool.kind === "tool" && tool.done).toBe(true);
  });

  it("keeps the run thinking out loud, and only drops its answer", () => {
    const items = traceItems([
      ev(1, "assistant_message", { text: "checking the rotation first" }),
      ev(2, "tool_start", { id: "c1", name: "grep" }),
      ev(3, "tool_end", { id: "c1", name: "grep", text: "flow.go:88" }),
      ev(4, "turn_end", { text: "Reproduced." }),
    ]);
    expect(items.map((i) => i.kind)).toEqual(["assistant", "tool"]);
  });
});
