import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeSocket, installFakeSocket } from "@/test/socket";
import { useSession } from "./session";

/** The socket is opened from an effect and reopened from a timer, so every
 *  assertion here is about the URL of the newest socket: `since` is the only
 *  place the resume point is observable. */
function latest(): FakeSocket {
  const s = FakeSocket.opened[FakeSocket.opened.length - 1];
  if (!s) throw new Error("no socket was opened");
  return s;
}

function reconnect() {
  act(() => latest().close());
  act(() => void vi.runOnlyPendingTimers());
}

beforeEach(() => {
  installFakeSocket();
  vi.useFakeTimers();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("useSession", () => {
  it("resumes from the highest sequence number it received", () => {
    renderHook(() => useSession("s1"));
    act(() => latest().open());
    expect(latest().since).toBe(0);

    act(() => latest().deliver({ kind: "turn_start", seq: 7 }));
    reconnect();

    expect(latest().since).toBe(7);
  });

  it("starts a different conversation from the beginning", () => {
    const { rerender } = renderHook(({ id }) => useSession(id), {
      initialProps: { id: "s1" },
    });
    act(() => latest().open());
    act(() => latest().deliver({ kind: "turn_start", seq: 7 }));

    rerender({ id: "s2" });

    expect(latest().url).toContain("/api/sessions/s2/socket");
    expect(latest().since).toBe(0);
  });

  it("rewinds to the point a desync says was actually delivered", () => {
    const { result } = renderHook(() => useSession("s1"));
    act(() => latest().open());
    act(() => latest().deliver({ kind: "turn_start", seq: 9 }));

    // The daemon drops a subscriber that fell behind, naming the last event it
    // is sure of. Anything the client counted past that point is a lie.
    act(() => latest().deliver({ kind: "desync", from: 4 }));
    act(() => void vi.runOnlyPendingTimers());

    expect(latest().since).toBe(4);
    // The reconnect re-renders the hook; the resume point must survive that.
    expect(result.current.state.connected).toBe(false);
    act(() => latest().open());
    expect(latest().since).toBe(4);
  });

  it("keeps a rejected op out of the timeline and out of the resume point", () => {
    const { result } = renderHook(() => useSession("s1"));
    act(() => latest().open());
    act(() => latest().deliver({ kind: "turn_start", seq: 3 }));
    act(() => latest().deliver({ kind: "client_error", op: "resolve", error: "already decided" }));

    expect(result.current.state.items).toEqual([]);
    reconnect();
    expect(latest().since).toBe(3);
  });

  it("only writes ops to a socket that is open", () => {
    const { result } = renderHook(() => useSession("s1"));
    const ws = latest();

    act(() => result.current.submit("dropped while connecting"));
    expect(ws.sent).toEqual([]);

    act(() => ws.open());
    act(() => result.current.submit("hello"));

    expect(ws.sent.map((s) => JSON.parse(s))).toEqual([{ op: "submit", text: "hello" }]);
  });

  it("does not let the conversation just left reconnect over the new one", () => {
    const { result, rerender } = renderHook(({ id }) => useSession(id), {
      initialProps: { id: "s1" },
    });
    const first = latest();
    act(() => first.open());

    rerender({ id: "s2" });
    const second = latest();
    act(() => second.open());

    // A browser delivers the old socket's close after the new socket is up.
    act(() => first.onclose?.());
    act(() => void vi.runOnlyPendingTimers());

    expect(latest().url).toContain("/api/sessions/s2/socket");
    act(() => result.current.submit("meant for s2"));
    expect(second.sent.map((s) => JSON.parse(s))).toEqual([
      { op: "submit", text: "meant for s2" },
    ]);
    expect(first.sent).toEqual([]);
  });

  it("ignores a late open from the conversation just left", () => {
    const { result, rerender } = renderHook(({ id }) => useSession(id), {
      initialProps: { id: "s1" },
    });
    const first = latest();

    rerender({ id: "s2" });
    expect(result.current.state.connected).toBe(false);

    act(() => first.open());
    expect(result.current.state.connected).toBe(false);
  });

  it("ignores late messages and keeps the new conversation's resume point", () => {
    const { result, rerender } = renderHook(({ id }) => useSession(id), {
      initialProps: { id: "s1" },
    });
    const first = latest();

    rerender({ id: "s2" });
    const second = latest();
    act(() => second.open());
    act(() => first.deliver({ kind: "user_message", seq: 99, text: "from s1" }));
    act(() => second.deliver({ kind: "user_message", seq: 4, text: "from s2" }));

    expect(result.current.state.items).toHaveLength(1);
    expect(result.current.state.items[0]).toMatchObject({ text: "from s2", seq: 4 });
    reconnect();
    expect(latest().url).toContain("/api/sessions/s2/socket");
    expect(latest().since).toBe(4);
  });

  it("drops the timeline when the conversation changes", () => {
    const { result, rerender } = renderHook(({ id }) => useSession(id), {
      initialProps: { id: "s1" },
    });
    act(() => latest().open());
    act(() => latest().deliver({ kind: "user_message", seq: 1, text: "hello" }));
    expect(result.current.state.items).toHaveLength(1);

    rerender({ id: "s2" });

    expect(result.current.state.items).toEqual([]);
  });
});
