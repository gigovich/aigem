import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ChangedFiles } from "./files";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function stubFetch(bodies: unknown[]) {
  const calls: string[] = [];
  let i = 0;
  vi.stubGlobal("fetch", vi.fn((url: string) => {
    calls.push(url);
    const body = bodies[Math.min(i++, bodies.length - 1)];
    if (body instanceof Error) return Promise.reject(body);
    return Promise.resolve({
      ok: true, status: 200, statusText: "OK", json: () => Promise.resolve(body),
    });
  }));
  return calls;
}

beforeEach(() => sessionStorage.setItem("aigem-token", "t"));

describe("ChangedFiles", () => {
  it("refetches when a file changes again, not only when a new one appears", async () => {
    const calls = stubFetch([
      [{ path: "/w/a.go", created: true }],
      [{ path: "/w/a.go", created: true }],
    ]);
    const { rerender } = render(
      <ChangedFiles sessionID="s1" version={1} onOpen={vi.fn()} />,
    );
    await waitFor(() => expect(calls).toHaveLength(1));

    // Same path, second write: the counter moves even though the path set does not.
    rerender(<ChangedFiles sessionID="s1" version={2} onOpen={vi.fn()} />);
    await waitFor(() => expect(calls).toHaveLength(2));

    rerender(<ChangedFiles sessionID="s1" version={2} onOpen={vi.fn()} />);
    expect(calls).toHaveLength(2);
  });

  it("keeps the last good list when the daemon cannot be reached", async () => {
    stubFetch([[{ path: "/w/a.go", created: true }], new Error("offline")]);
    const { rerender } = render(
      <ChangedFiles sessionID="s1" version={1} onOpen={vi.fn()} />,
    );
    await waitFor(() => expect(screen.getByText("w/a.go")).toBeInTheDocument());

    rerender(<ChangedFiles sessionID="s1" version={2} onOpen={vi.fn()} />);

    await waitFor(() => expect(screen.getByText(/may be stale/)).toBeInTheDocument());
    expect(screen.getByText("w/a.go")).toBeInTheDocument();
    expect(screen.queryByText("Nothing written yet.")).not.toBeInTheDocument();
  });

  it("marks the file whose diff is open", async () => {
    stubFetch([[{ path: "/w/a.go", created: false }, { path: "/w/b.go", created: false }]]);
    render(
      <ChangedFiles sessionID="s1" version={1} openPath="/w/b.go" onOpen={vi.fn()} />,
    );

    await waitFor(() => expect(screen.getByText("w/b.go")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: /w\/b\.go/ })).toHaveAttribute("aria-current", "true");
    expect(screen.getByRole("button", { name: /w\/a\.go/ })).not.toHaveAttribute("aria-current");
  });
});
