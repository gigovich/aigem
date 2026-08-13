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
      <DiffView artifactsURL="/api/sessions/s1/artifacts" artifact={stub} version={1} onClose={vi.fn()} />,
    );
    await waitFor(() => expect(screen.getByText("two")).toBeInTheDocument());

    rerender(<DiffView artifactsURL="/api/sessions/s1/artifacts" artifact={stub} version={2} onClose={vi.fn()} />);

    await waitFor(() => expect(screen.getByText("three")).toBeInTheDocument());
    expect(calls).toHaveLength(2);
  });

  it("leaves on Escape, as the rails do", async () => {
    stubFetch([[{ path: "a/b.txt", created: false, old: "one", new: "two" }]]);
    const onClose = vi.fn();
    render(<DiffView artifactsURL="/api/sessions/s1/artifacts" artifact={stub} version={1} onClose={onClose} />);

    fireEvent.keyDown(window, { key: "Escape" });

    expect(onClose).toHaveBeenCalledOnce();
  });

  it("does not take the caret back when the app re-renders", async () => {
    stubFetch([[{ path: "a/b.txt", created: false, old: "one", new: "two" }]]);
    const outside = document.createElement("input");
    document.body.appendChild(outside);
    const { rerender } = render(
      <DiffView artifactsURL="/api/sessions/s1/artifacts" artifact={stub} version={1} onClose={vi.fn()} />,
    );
    await waitFor(() => expect(screen.getByText("two")).toBeInTheDocument());

    // The composer sits outside this overlay on purpose; a streamed token must
    // not pull focus out of it.
    outside.focus();
    rerender(<DiffView artifactsURL="/api/sessions/s1/artifacts" artifact={stub} version={1} onClose={vi.fn()} />);

    expect(document.activeElement).toBe(outside);
    outside.remove();
  });

  it("leaves Escape to a drawer opened over it", async () => {
    stubFetch([[{ path: "a/b.txt", created: false, old: "one", new: "two" }]]);
    const onClose = vi.fn();
    render(<DiffView artifactsURL="/api/sessions/s1/artifacts" artifact={stub} version={1} onClose={onClose} />);

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
      <DiffView artifactsURL="/api/sessions/s1/artifacts" artifact={stub} version={1} onClose={vi.fn()} />,
    );

    await waitFor(() => {
      expect(screen.getByText("\\ No newline at end of file")).toBeInTheDocument();
    });
    expect(screen.queryByText("No line changed.")).not.toBeInTheDocument();
    // Four cells to a unified line - old number, new number, marker, text - and
    // the marker line carries neither number, being metadata about the line
    // above it rather than a line of the file.
    const text = screen.getByText("\\ No newline at end of file");
    const marker = text.previousElementSibling;
    const newNum = marker?.previousElementSibling;
    const oldNum = newNum?.previousElementSibling;
    expect(oldNum?.textContent).toBe("");
    expect(newNum?.textContent).toBe("");
    expect(container.querySelectorAll(".grid > div")).toHaveLength(12);
  });

  it("says a file is empty rather than showing a blank pane", async () => {
    stubFetch([[{ path: "a/b.txt", created: true, new: "" }]]);
    render(<DiffView artifactsURL="/api/sessions/s1/artifacts" artifact={stub} version={1} onClose={vi.fn()} />);

    await waitFor(() => expect(screen.getByText("This file is empty.")).toBeInTheDocument());
  });

  it("marks every line of a created file as an addition, with no old numbers", async () => {
    stubFetch([[{ path: "a/b.txt", created: true, new: "one\ntwo" }]]);
    const { container } = render(
      <DiffView artifactsURL="/api/sessions/s1/artifacts" artifact={{ ...stub, created: true }} version={1} onClose={vi.fn()} />,
    );

    await waitFor(() => expect(screen.getByText("one")).toBeInTheDocument());
    const cells = Array.from(container.querySelectorAll(".grid > div"));
    // Two lines, four cells each. A created file has no old side, so the first
    // gutter stays empty rather than inventing numbers for it.
    expect(cells).toHaveLength(8);
    expect(cells[0].textContent).toBe("");
    expect(cells[1].textContent).toBe("1");
    expect(cells[2].textContent).toBe("+");
    expect(cells[3].textContent).toBe("one");
  });

  it("reads as a diff with the colour taken away", async () => {
    stubFetch([[{ path: "a/b.txt", created: false, old: "one\nkeep", new: "two\nkeep" }]]);
    const { container } = render(
      <DiffView artifactsURL="/api/sessions/s1/artifacts" artifact={stub} version={1} onClose={vi.fn()} />,
    );

    await waitFor(() => expect(screen.getByText("two")).toBeInTheDocument());
    const markers = Array.from(container.querySelectorAll(".grid > div"))
      .filter((_, i) => i % 4 === 2)
      .map((el) => el.textContent);
    // Removed before added, then the untouched line: the order every other diff
    // prints, said in characters rather than in red and green.
    expect(markers).toEqual(["-", "+", " "]);
  });
});
