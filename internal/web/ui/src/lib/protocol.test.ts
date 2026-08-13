import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, authenticate, hasCookie, resetAuth, socketURL } from "./protocol";

beforeEach(() => {
  window.history.replaceState({}, "", "/?token=t0p53cr3t");
  sessionStorage.clear();
  resetAuth();
});

afterEach(() => vi.unstubAllGlobals());

/** A daemon that answers the exchange, and records what it was asked. */
function daemon(exchange: Response) {
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init });
      if (String(input).includes("/api/auth/session")) return Promise.resolve(exchange);
      return Promise.resolve(new Response("{}", { status: 200 }));
    }),
  );
  return calls;
}

function authOf(init?: RequestInit): string | undefined {
  return (init?.headers as Record<string, string> | undefined)?.Authorization;
}

describe("the token-for-cookie exchange", () => {
  it("sends the URL token once, and then stops sending it", async () => {
    const calls = daemon(new Response(null, { status: 204 }));

    expect(await authenticate()).toBe(true);
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toContain("/api/auth/session");
    expect(authOf(calls[0].init)).toBe("Bearer t0p53cr3t");

    await api("/api/chat/meta");
    // The cookie travels on its own; a page still sending the token would be
    // putting it back in the logs the exchange exists to keep it out of.
    expect(authOf(calls[1].init)).toBeUndefined();
  });

  it("keeps the socket URL free of the token once there is a cookie", async () => {
    daemon(new Response(null, { status: 204 }));
    await authenticate();

    const url = socketURL("/api/chat/socket", { since: "7" });
    expect(url).toContain("since=7");
    expect(url).not.toContain("token");
  });

  // A daemon too old to know the route, or one that refused: the page still has
  // to work, and it works the way it did before the cookie existed.
  it("falls back to the bearer token when the daemon refuses", async () => {
    const calls = daemon(new Response("nope", { status: 404 }));

    expect(await authenticate()).toBe(false);
    expect(hasCookie()).toBe(false);

    await api("/api/chat/meta");
    expect(authOf(calls[1].init)).toBe("Bearer t0p53cr3t");
    expect(socketURL("/api/chat/socket", { since: "0" })).toContain("token=t0p53cr3t");
  });

  it("exchanges once no matter how many callers ask", async () => {
    const calls = daemon(new Response(null, { status: 204 }));

    await Promise.all([authenticate(), authenticate(), authenticate()]);

    expect(calls.filter((c) => c.url.includes("/api/auth/session"))).toHaveLength(1);
  });

  // The exchange is a request like any other, and a network that is down must
  // not leave the page unable to render at all.
  it("survives a fetch that throws", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.reject(new Error("offline"))),
    );
    expect(await authenticate()).toBe(false);
  });
});
