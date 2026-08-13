import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Turn } from "@/lib/chatprotocol";
import type { Event } from "@/lib/protocol";
import type { TurnTrace } from "@/lib/trace";
import { AgentTrace } from "./AgentTrace";

afterEach(cleanup);

const turn: Turn = {
  seq: 7,
  thread: "t_1",
  actor: "bot:amiran",
  started: "2026-08-13T14:01:00Z",
  ended: "2026-08-13T14:02:00Z",
  steps: 14,
  tools: 6,
  files: 2,
};

function held(events: Event[], over: Partial<TurnTrace> = {}): TurnTrace {
  return { events, loaded: true, cursor: 0, more: false, ...over };
}

function draw(t: Turn, over: Partial<Parameters<typeof AgentTrace>[0]> = {}) {
  return render(
    <AgentTrace
      turn={t}
      held={undefined}
      open={false}
      blobURL={(seq) => `/api/chat/threads/t_1/blobs/${seq}`}
      onToggle={vi.fn()}
      onMore={vi.fn()}
      {...over}
    />,
  );
}

describe("AgentTrace", () => {
  it("summarises the run in one line from the row", () => {
    // This line is the point of the whole migration: a chat product could show
    // the answer and nothing else, so a bot that spent fourteen steps getting
    // there looked exactly like one that guessed.
    draw(turn);
    expect(screen.getByRole("button")).toHaveTextContent("14 steps · 6 tools · 2 files");
  });

  it("leaves out a count of nothing", () => {
    draw({ ...turn, tools: 0, files: 0 });
    const line = screen.getByRole("button").textContent ?? "";
    expect(line).toContain("14 steps");
    expect(line).not.toContain("0 tools");
  });

  it("says nothing at all for a finished run that recorded nothing", () => {
    // A disclosure that opens onto an empty panel is worse than no control.
    const { container } = draw({ ...turn, steps: 0, tools: 0, files: 0 });
    expect(container.querySelector("button")).toBeNull();
  });

  it("still shows for a run that is going, before it has recorded a step", () => {
    draw({ ...turn, ended: undefined, steps: 0, tools: 0, files: 0 });
    expect(screen.getByRole("button")).toHaveTextContent("working");
  });

  it("counts what has arrived while the run is being watched", () => {
    // The daemon's counters are re-read when a turn ends and not before, so a
    // running turn drawn from the row would freeze mid-run.
    draw(
      { ...turn, ended: undefined, steps: 0, tools: 0, files: 0 },
      {
        held: held([
          { seq: 1, kind: "tool_start", id: "c1", name: "grep" } as Event,
          { seq: 2, kind: "tool_end", id: "c1", name: "grep" } as Event,
        ]),
      },
    );
    // One step, not two: tool_end completes the row tool_start opened rather
    // than adding one, so it is not counted.
    expect(screen.getByRole("button")).toHaveTextContent("1 step · 1 tool");
  });

  it("draws the timeline only once it is opened", () => {
    const events: Event[] = [
      { seq: 1, kind: "tool_start", id: "c1", name: "grep" } as Event,
      { seq: 2, kind: "tool_end", id: "c1", name: "grep", text: "flow.go:88" } as Event,
    ];
    const { rerender } = draw(turn, { held: held(events) });
    expect(screen.queryByText("grep")).toBeNull();

    rerender(
      <AgentTrace
        turn={turn}
        held={held(events)}
        open
        blobURL={(seq) => `/api/chat/threads/t_1/blobs/${seq}`}
        onToggle={vi.fn()}
        onMore={vi.fn()}
      />,
    );
    expect(screen.getByText("grep")).toBeInTheDocument();
    expect(screen.getByRole("button", { expanded: true })).toBeInTheDocument();
  });

  it("names the run it is toggling, so one handler can serve every trace", () => {
    // Every trace on screen is handed the same callbacks; an inline arrow per
    // message would defeat the memo and re-render the whole transcript per
    // frame. Asserting the argument is what keeps that contract.
    const onToggle = vi.fn();
    const onMore = vi.fn();
    draw(turn, { onToggle, onMore, held: held([], { more: true }), open: true });
    fireEvent.click(screen.getByRole("button", { expanded: true }));
    expect(onToggle).toHaveBeenCalledWith(7);
    fireEvent.click(screen.getByRole("button", { name: "More steps" }));
    expect(onMore).toHaveBeenCalledWith(7);
  });

  it("draws a skeleton on the first expand, before anything is held", () => {
    // `held` is undefined until a page lands, and `undefined === 0` is false -
    // which left the reader looking at an empty bordered box for the length of
    // the fetch, on every historical run.
    const { container } = render(
      <AgentTrace
        turn={turn}
        held={undefined}
        open
        blobURL={(seq) => `/api/chat/threads/t_1/blobs/${seq}`}
        onToggle={vi.fn()}
        onMore={vi.fn()}
      />,
    );
    expect(container.querySelector(".animate-shimmer")).not.toBeNull();
  });

  it("offers the rest of a run that did not fit in one page", () => {
    render(
      <AgentTrace
        turn={turn}
        held={held([], { loaded: false, more: true, cursor: 40 })}
        open
        blobURL={(seq) => `/api/chat/threads/t_1/blobs/${seq}`}
        onToggle={vi.fn()}
        onMore={vi.fn()}
      />,
    );
    // Named for the direction the daemon actually pages: forwards, from the
    // oldest event on, so the next page is the rest of the run.
    expect(screen.getByRole("button", { name: "More steps" })).toBeInTheDocument();
  });
});
