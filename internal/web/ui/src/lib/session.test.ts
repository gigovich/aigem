import { describe, expect, it } from "vitest";
import type { Event, Kind } from "./protocol";
import { empty, sessionReducer, type State } from "./session";

function fresh(): State {
  return { ...empty, items: [], todos: [], clients: [] };
}

function event(kind: Kind, seq: number, values: Partial<Event> = {}): Event {
  return { kind, seq, time: "2026-08-10T00:00:00Z", ...values };
}

describe("sessionReducer", () => {
  it("counts every file change, not the files that changed", () => {
    let state = sessionReducer(fresh(), { t: "event", ev: event("file_changed", 1, { path: "a.go" }) });
    state = sessionReducer(state, { t: "event", ev: event("file_changed", 2, { path: "a.go" }) });

    // A second write to the same file has to move this, or the rail that
    // refetches on a change never sees it.
    expect(state.fileEvents).toBe(2);
  });

  it("joins streamed content and commits it at the end of a turn", () => {
    let state = sessionReducer(fresh(), { t: "event", ev: event("turn_start", 1) });
    state = sessionReducer(state, { t: "event", ev: event("content", 2, { text: "hello " }) });
    state = sessionReducer(state, { t: "event", ev: event("content", 3, { text: "world" }) });
    state = sessionReducer(state, { t: "event", ev: event("turn_end", 4) });

    expect(state.running).toBe(false);
    expect(state.lastSeq).toBe(4);
    expect(state.items).toEqual([
      { kind: "assistant", seq: 2, text: "hello world", streaming: false },
    ]);
  });

  it("attaches an out-of-order tool result to the matching call", () => {
    let state = sessionReducer(fresh(), {
      t: "event",
      ev: event("tool_start", 1, { id: "first", name: "read_file" }),
    });
    state = sessionReducer(state, {
      t: "event",
      ev: event("tool_start", 2, { id: "second", name: "grep" }),
    });
    state = sessionReducer(state, {
      t: "event",
      ev: event("tool_end", 3, { id: "first", text: "contents" }),
    });

    expect(state.items).toMatchObject([
      { kind: "tool", id: "first", done: true, result: "contents" },
      { kind: "tool", id: "second", done: false },
    ]);
  });

  it("opens and clears approval state from the event stream", () => {
    const approval = {
      kind: "tool" as const,
      tool: "bash",
      options: [
        { value: "once" as const, label: "Once" },
        { value: "deny" as const, label: "Deny" },
      ],
    };
    let state = sessionReducer(fresh(), {
      t: "event",
      ev: event("approval_request", 1, { id: "approval-1", approval }),
    });

    expect(state.approval).toEqual({ id: "approval-1", req: approval });

    state = sessionReducer(state, {
      t: "event",
      ev: event("approval_resolved", 2, { id: "approval-1", decision: "once" }),
    });
    expect(state.approval).toBeNull();
  });
});
