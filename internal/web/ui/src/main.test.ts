import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { resetAuth } from "./lib/protocol";

// The one line that decides how every request this page makes is authenticated.
//
// Everything under it is tested - the exchange, the fallback, the socket URL -
// and all of it passes with the call deleted, at which point every page quietly
// goes back to putting its token in the URL of every websocket and in every
// access log on the way. So the call itself is what is asserted here.
beforeEach(() => {
  vi.resetModules();
  resetAuth();
  document.body.innerHTML = '<div id="root"></div>';
  window.history.replaceState({}, "", "/?token=t0p53cr3t");
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = "";
});

it("trades the token for a cookie before it renders anything", async () => {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      calls.push(String(input));
      if (String(input).includes("/api/auth/session")) {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return Promise.resolve(
        new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } }),
      );
    }),
  );
  // The socket the app opens on mount is not what this test is about, and jsdom
  // has no WebSocket at all.
  vi.stubGlobal(
    "WebSocket",
    class {
      static readonly OPEN = 1;
      readyState = 0;
      close() {}
      send() {}
    },
  );

  await import("./main");
  // The dynamic import resolves once the module body has run; the render is
  // inside the exchange's own continuation, so settling it is enough.
  await vi.waitFor(() => expect(calls.length).toBeGreaterThan(0));

  expect(calls[0]).toContain("/api/auth/session");
});
