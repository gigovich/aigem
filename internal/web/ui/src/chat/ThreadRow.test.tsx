import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ThreadView } from "@/lib/chatprotocol";
import { ThreadRow } from "./ThreadRow";

afterEach(cleanup);

function view(over: Partial<ThreadView> = {}): ThreadView {
  return {
    id: "t_1",
    title: "Refresh-token rotation drops sessions",
    created: "2026-08-13T14:00:00Z",
    created_by: "human:operator",
    last_seq: 12,
    last_at: "2026-08-13T14:02:00Z",
    last_author: "bot:amiran",
    last_text: "Reproduced on staging.",
    state: "idle",
    participants: ["human:operator", "bot:amiran", "bot:demetre"],
    unread: 0,
    working: false,
    ...over,
  };
}

function renderRow(over: Partial<ThreadView> = {}, active = false) {
  const onSelect = vi.fn();
  const { container } = render(
    <ThreadRow
      thread={view(over)}
      active={active}
      maxUnread={99}
      operator="human:operator"
      onSelect={onSelect}
    />,
  );
  return { onSelect, container };
}

/** The marker slot is 2px of accent, and the accent carries one meaning in this
 *  product: you are the one who has to act. Spending it on the open row too
 *  would leave the reader no way to tell the two apart at a glance. */
function markerIsAccent(container: HTMLElement): boolean {
  // By test id, not by "the first aria-hidden span": RunDot is one of those
  // too, and it also carries bg-accent, so the loose selector only ever worked
  // by DOM order.
  const marker = container.querySelector('[data-testid="row-marker"]');
  return marker?.className.includes("bg-accent") ?? false;
}

describe("ThreadRow", () => {
  it("spends the accent marker on needs_you and on nothing else", () => {
    expect(markerIsAccent(renderRow({ state: "needs_you" }).container)).toBe(true);
    cleanup();
    expect(markerIsAccent(renderRow({ state: "working", working: true }).container)).toBe(false);
    cleanup();
    // Not even for the row being read: that is what the background says.
    expect(markerIsAccent(renderRow({ state: "idle" }, true).container)).toBe(false);
  });

  it("names every state in words, so colour is never the only cue", () => {
    for (const [state, label] of [
      ["needs_you", "needs you"],
      ["working", "working"],
      ["waiting", "waiting"],
      ["idle", "idle"],
    ] as const) {
      renderRow({ state });
      expect(screen.getByText(label)).toBeInTheDocument();
      cleanup();
    }
  });

  it("caps the unread count rather than widening the row", () => {
    renderRow({ unread: 140 });
    expect(screen.getByText("99+")).toBeInTheDocument();
    cleanup();

    renderRow({ unread: 3 });
    expect(screen.getByText("3")).toBeInTheDocument();
  });

  it("says nothing about unread when there is none", () => {
    renderRow({ unread: 0 });
    // A row that reads "0" is a row claiming something about a thread with
    // nothing outstanding, which is the noise the count exists to avoid.
    expect(screen.queryByText("0", { exact: true })).toBeNull();
  });

  it("labels an untitled thread with the last thing said in it", () => {
    // The row has no room for a preview line, so the preview becomes the label
    // in the one case the title was not carrying it.
    renderRow({ title: "" });
    expect(screen.getByRole("button", { name: /Reproduced on staging/ })).toBeInTheDocument();
  });

  it("falls back to the bots when a thread has neither title nor messages", () => {
    renderRow({ title: "", last_text: "" });
    expect(screen.getByRole("button", { name: /amiran · demetre/ })).toBeInTheDocument();
  });

  it("marks the row being read without spending the accent on it", () => {
    // Background alone cannot say it: hover paints the same colour, so on a
    // desktop the thread being read looked identical to whatever the pointer
    // was over.
    const { container } = renderRow({ state: "idle" }, true);
    const marker = container.querySelector('[data-testid="row-marker"]');
    expect(marker?.className).toContain("bg-fg");
    expect(marker?.className).not.toContain("bg-accent");
  });

  it("marks the open row and reports a pick once", () => {
    const { onSelect } = renderRow({}, true);
    const row = screen.getByRole("button");
    expect(row).toHaveAttribute("aria-current", "true");

    fireEvent.click(row);
    expect(onSelect).toHaveBeenCalledTimes(1);
  });
});
