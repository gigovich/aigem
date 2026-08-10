import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { DiffView } from "./files";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

beforeEach(() => sessionStorage.setItem("aigem-token", "t"));

function stubFetch(bodies: unknown[]) {
  const calls: string[] = [];
  let i = 0;
  vi.stubGlobal("fetch", vi.fn((url: string) => {
    calls.push(url);
    return Promise.resolve({
      ok: true, status: 200, statusText: "OK",
      json: () => Promise.resolve(bodies[Math.min(i++, bodies.length - 1)]),
    });
  }));
  return calls;
}

const stub = { path: "a/b.txt", created: false };

describe("DiffView", () => {
  it("refetches when the file it is showing is written again", async () => {
    const calls = stubFetch([
      [{ path: "a/b.txt", created: false, old: "one", new: "two" }],
      [{ path: "a/b.txt", created: false, old: "one", new: "three" }],
    ]);
    const { rerender } = render(
      <DiffView sessionID="s1" artifact={stub} version={1} onClose={vi.fn()} />,
    );
    await waitFor(() => expect(screen.getByText("two")).toBeInTheDocument());

    rerender(<DiffView sessionID="s1" artifact={stub} version={2} onClose={vi.fn()} />);

    await waitFor(() => expect(screen.getByText("three")).toBeInTheDocument());
    expect(calls).toHaveLength(2);
  });

  it("leaves on Escape, as the rails do", async () => {
    stubFetch([[{ path: "a/b.txt", created: false, old: "one", new: "two" }]]);
    const onClose = vi.fn();
    render(<DiffView sessionID="s1" artifact={stub} version={1} onClose={onClose} />);

    fireEvent.keyDown(window, { key: "Escape" });

    expect(onClose).toHaveBeenCalledOnce();
  });

  it("does not take the caret back when the app re-renders", async () => {
    stubFetch([[{ path: "a/b.txt", created: false, old: "one", new: "two" }]]);
    const outside = document.createElement("input");
    document.body.appendChild(outside);
    const { rerender } = render(
      <DiffView sessionID="s1" artifact={stub} version={1} onClose={vi.fn()} />,
    );
    await waitFor(() => expect(screen.getByText("two")).toBeInTheDocument());

    // The composer sits outside this overlay on purpose; a streamed token must
    // not pull focus out of it.
    outside.focus();
    rerender(<DiffView sessionID="s1" artifact={stub} version={1} onClose={vi.fn()} />);

    expect(document.activeElement).toBe(outside);
    outside.remove();
  });

  it("leaves Escape to a drawer opened over it", async () => {
    stubFetch([[{ path: "a/b.txt", created: false, old: "one", new: "two" }]]);
    const onClose = vi.fn();
    render(<DiffView sessionID="s1" artifact={stub} version={1} onClose={onClose} />);

    const drawer = document.createElement("aside");
    drawer.setAttribute("role", "dialog");
    document.body.appendChild(drawer);
    fireEvent.keyDown(window, { key: "Escape" });

    expect(onClose).not.toHaveBeenCalled();
    drawer.remove();
  });

  it("shows a missing trailing newline as a change with no invented line number", async () => {
    stubFetch([[
      { path: "a/b.txt", created: false, old: "one\ntwo", new: "one\ntwo\n" },
    ]]);
    const { container } = render(
      <DiffView sessionID="s1" artifact={stub} version={1} onClose={vi.fn()} />,
    );

    await waitFor(() => {
      expect(screen.getByText("\\ No newline at end of file")).toBeInTheDocument();
    });
    expect(screen.queryByText("No line changed.")).not.toBeInTheDocument();
    const marker = screen.getByText("\\ No newline at end of file");
    const number = marker.previousElementSibling;
    expect(number).toHaveTextContent("");
    expect(container.querySelectorAll(".grid > div")).toHaveLength(12);
  });

  it("says a file is empty rather than showing a blank pane", async () => {
    stubFetch([[{ path: "a/b.txt", created: true, new: "" }]]);
    render(<DiffView sessionID="s1" artifact={stub} version={1} onClose={vi.fn()} />);

    await waitFor(() => expect(screen.getByText("This file is empty.")).toBeInTheDocument());
  });

  it("drops the empty half for a file that has no previous version", async () => {
    stubFetch([[{ path: "a/b.txt", created: true, new: "one\ntwo" }]]);
    const { container } = render(
      <DiffView sessionID="s1" artifact={{ ...stub, created: true }} version={1} onClose={vi.fn()} />,
    );

    await waitFor(() => expect(screen.getByText("one")).toBeInTheDocument());
    // Two columns, not four: a created file has no old side to leave blank.
    expect(container.querySelector(".grid")).toHaveClass("grid-cols-[auto_1fr]");
  });
});
