import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeSocket, installFakeSocket } from "@/test/socket";
import { useSession } from "./session";
import { resetAuth } from "./protocol";

/** The recovery wire for the outage this whole path exists for.
 *
 *  Deploying is a restart, a restart revokes every cookie, and a browser cannot
 *  see a handshake's status - a 401 arrives as an ordinary close. So a socket
 *  that never opened has to re-run the exchange, or every open tab reconnects
 *  forever against a credential the daemon has forgotten.
 *
 *  Both socket hooks call `socketClosed()` for exactly that, and deleting the
 *  call from both of them used to leave the whole suite green. */
function daemon() {
  const exchanges: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/api/auth/session")) exchanges.push(url);
      return Promise.resolve(new Response(null, { status: 204 }));
    }),
  );
  return exchanges;
}

beforeEach(() => {
  installFakeSocket();
  resetAuth();
  window.history.replaceState({}, "", "/?token=t0p53cr3t");
  vi.useFakeTimers();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function latest(): FakeSocket {
  const s = FakeSocket.opened[FakeSocket.opened.length - 1];
  if (!s) throw new Error("no socket was opened");
  return s;
}

describe("a socket the daemon refused", () => {
  it("re-runs the token exchange before reconnecting", async () => {
    const exchanges = daemon();
    renderHook(() => useSession("s1"));

    // Refused: the browser sees a close, never an open.
    act(() => latest().close());
    await act(async () => {});

    expect(exchanges.length).toBeGreaterThan(0);
  });

  it("does not re-run it for a connection that ran and then ended", async () => {
    const exchanges = daemon();
    renderHook(() => useSession("s1"));

    act(() => latest().open());
    const before = exchanges.length;
    // The commonest close there is: this client dropping the socket on a
    // desync. Re-exchanging for that would put the token back in the next URL.
    act(() => latest().close());
    await act(async () => {});

    expect(exchanges.length).toBe(before);
  });
});
