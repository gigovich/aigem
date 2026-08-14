import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { disable, enable, pushSupported, ready, resetPush, useWebPush } from "./push";
import { renderHook } from "@testing-library/react";
import type { Permission } from "./notify";
import { resetAuth } from "./protocol";

/** A stand-in for one browser's subscription. */
function fakeSubscription(endpoint: string, key: ArrayBuffer) {
  return {
    endpoint,
    options: { applicationServerKey: key },
    toJSON: () => ({ endpoint, keys: { p256dh: "p256dh-value", auth: "auth-value" } }),
    unsubscribe: vi.fn(() => Promise.resolve(true)),
  };
}

// A real application server key: 65 bytes of uncompressed P-256 point in
// base64url, which means it contains "-" and "_". Anything shorter and
// alphabet-free lets a broken base64url mapping pass every test and then fail
// for every user.
const KEY =
  "BEl62iUYgUivxIkv69yViEuiBIa-Ib9-SkvMeAtA3LFgDzkrxZJjSgSnfckjBJuBkr3qBUYIHBQFLXYp5Nksh8U";

function keyBytes(key: string): ArrayBuffer {
  const padded = key.replace(/-/g, "+").replace(/_/g, "/");
  const raw = atob(padded.padEnd(padded.length + ((4 - (padded.length % 4)) % 4), "="));
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes.buffer;
}

/** browser installs a service worker and a push manager on this jsdom window,
 *  which has neither. existing is the subscription the browser already holds;
 *  worker is whether this browser already has a registration from an earlier
 *  page load. */
function browser(
  existing: ReturnType<typeof fakeSubscription> | null = null,
  worker = true,
) {
  const subscribe = vi.fn((opts: PushSubscriptionOptionsInit) =>
    Promise.resolve(
      fakeSubscription("https://push.example.net/send/new", opts.applicationServerKey as ArrayBuffer),
    ),
  );
  const registration = {
    pushManager: {
      getSubscription: vi.fn(() => Promise.resolve(existing)),
      subscribe,
    },
  };
  const register = vi.fn(() => Promise.resolve(registration));
  const getRegistration = vi.fn(() => Promise.resolve(worker ? registration : undefined));
  Object.defineProperty(navigator, "serviceWorker", {
    value: { register, getRegistration },
    configurable: true,
  });
  Object.defineProperty(window, "PushManager", { value: class {}, configurable: true });
  return { register, getRegistration, registration, subscribe };
}

/** daemon answers the two routes push.ts talks to, and records every call. */
function daemon(key: { available: boolean; key?: string } = { available: true, key: KEY }) {
  const calls: { url: string; method: string; body?: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, method: init?.method ?? "GET", body: init?.body as string | undefined });
      if (url.endsWith("/api/chat/push")) {
        return Promise.resolve(
          new Response(JSON.stringify(key), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
        );
      }
      return Promise.resolve(new Response(null, { status: 204 }));
    }),
  );
  return calls;
}

beforeEach(() => {
  window.history.replaceState({}, "", "/?token=t0p53cr3t");
  sessionStorage.clear();
  resetAuth();
  resetPush();
});

afterEach(() => {
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, "serviceWorker");
  Reflect.deleteProperty(window, "PushManager");
});

describe("subscribing", () => {
  it("registers the worker and hands the daemon the endpoint and both keys", async () => {
    const { register, subscribe } = browser();
    const calls = daemon();

    expect(await enable()).toBe("on");
    expect(register).toHaveBeenCalledWith("/sw.js");
    expect(subscribe.mock.calls[0][0].userVisibleOnly).toBe(true);
    // The key as bytes, decoded from base64url: 65 bytes beginning with the
    // 0x04 that marks an uncompressed point. A mapping that treated "-" and
    // "_" as ordinary base64 would throw before reaching here.
    const applied = new Uint8Array(subscribe.mock.calls[0][0].applicationServerKey as ArrayBuffer);
    expect(applied).toHaveLength(65);
    expect(applied[0]).toBe(4);

    const post = calls.find((c) => c.method === "POST");
    expect(post?.url).toBe("/api/chat/push/subs");
    expect(JSON.parse(post?.body ?? "{}")).toEqual({
      endpoint: "https://push.example.net/send/new",
      p256dh: "p256dh-value",
      auth: "auth-value",
    });
  });

  it("keeps a subscription made against the key the daemon still signs with", async () => {
    const existing = fakeSubscription("https://push.example.net/send/old", keyBytes(KEY));
    const { subscribe } = browser(existing);
    const calls = daemon();

    expect(await enable()).toBe("on");
    expect(subscribe).not.toHaveBeenCalled();
    expect(existing.unsubscribe).not.toHaveBeenCalled();
    // Re-sent anyway: the daemon's store may have been moved or restored.
    const post = calls.find((c) => c.method === "POST");
    expect(JSON.parse(post?.body ?? "{}").endpoint).toBe("https://push.example.net/send/old");
  });

  // A daemon that lost its vapid.json signs with a new key, and the browser
  // refuses to subscribe with a different one while the old subscription
  // exists. Left alone, the phone stays silent forever.
  it("replaces a subscription made against a key the daemon no longer has", async () => {
    const existing = fakeSubscription("https://push.example.net/send/old", keyBytes("BOtherKeyBytes"));
    const { subscribe } = browser(existing);
    const calls = daemon();

    expect(await enable()).toBe("on");
    expect(existing.unsubscribe).toHaveBeenCalled();
    expect(subscribe).toHaveBeenCalled();

    const gone = calls.find((c) => c.method === "DELETE");
    expect(JSON.parse(gone?.body ?? "{}")).toEqual({
      endpoint: "https://push.example.net/send/old",
    });
    const post = calls.find((c) => c.method === "POST");
    expect(JSON.parse(post?.body ?? "{}").endpoint).toBe("https://push.example.net/send/new");
  });

  it("does not register a worker for a daemon that has no keys", async () => {
    const { register } = browser();
    daemon({ available: false });

    expect(await enable()).toBe("unavailable");
    expect(register).not.toHaveBeenCalled();
  });

  it("reports a browser that cannot do this at all", async () => {
    daemon();
    expect(pushSupported()).toBe(false);
    expect(await enable()).toBe("unsupported");
  });

  // Safari in an ordinary tab: it has a service worker and no push manager.
  // That is the documented iOS case, and it is not the same as having neither.
  it("reports a browser with a worker but no push manager", async () => {
    browser();
    Reflect.deleteProperty(window, "PushManager");
    daemon();

    expect(pushSupported()).toBe(false);
    expect(await enable()).toBe("unsupported");
  });
});

describe("unsubscribing", () => {
  it("drops the subscription in the browser and in the daemon", async () => {
    const existing = fakeSubscription("https://push.example.net/send/old", keyBytes(KEY));
    browser(existing);
    const calls = daemon();
    await enable();

    await disable();
    expect(existing.unsubscribe).toHaveBeenCalled();
    const gone = calls.filter((c) => c.method === "DELETE");
    expect(JSON.parse(gone[0]?.body ?? "{}")).toEqual({
      endpoint: "https://push.example.net/send/old",
    });
  });

  // The load that discovers a revoked permission is a load on which nothing
  // called enable(), so there is no registration in this module. The
  // subscription is still there, and the daemon still pushes to it.
  it("finds the worker from an earlier page load", async () => {
    const existing = fakeSubscription("https://push.example.net/send/old", keyBytes(KEY));
    browser(existing);
    const calls = daemon();

    await disable();
    expect(existing.unsubscribe).toHaveBeenCalled();
    expect(calls.some((c) => c.method === "DELETE")).toBe(true);
  });

  it("does nothing on a browser that has no worker at all", async () => {
    browser(null, false);
    const calls = daemon();
    await disable();
    expect(calls).toHaveLength(0);
  });
});

describe("the registration the in-page notification path reads", () => {
  it("is nothing until this page has subscribed", async () => {
    browser();
    daemon();
    expect(await ready()).toBeNull();
    await enable();
    expect(await ready()).not.toBeNull();
  });
});

describe("the hook the screen calls", () => {
  it("subscribes once the permission has been granted", async () => {
    const { register } = browser();
    const calls = daemon();

    renderHook(() => useWebPush("granted"));
    await vi.waitFor(() => expect(calls.some((c) => c.method === "POST")).toBe(true));
    expect(register).toHaveBeenCalledWith("/sw.js");
  });

  // Revoked from the browser's own UI. A subscription that survives that is one
  // the daemon keeps pushing into a void.
  it("unsubscribes when the permission has been refused", async () => {
    const existing = fakeSubscription("https://push.example.net/send/old", keyBytes(KEY));
    browser(existing);
    const calls = daemon();

    renderHook(() => useWebPush("denied"));
    await vi.waitFor(() => expect(existing.unsubscribe).toHaveBeenCalled());
    expect(calls.some((c) => c.method === "DELETE")).toBe(true);
  });

  // The bell earns the permission after the page has mounted, so the effect
  // has to run again when it changes.
  it("subscribes when the permission is granted while the page is open", async () => {
    const { register } = browser();
    const calls = daemon();

    const { rerender } = renderHook(({ p }: { p: Permission }) => useWebPush(p), {
      initialProps: { p: "default" as Permission },
    });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(register).not.toHaveBeenCalled();

    rerender({ p: "granted" });
    await vi.waitFor(() => expect(calls.some((c) => c.method === "POST")).toBe(true));
  });

  it("does nothing until the operator has been asked", async () => {
    const { register } = browser();
    const calls = daemon();

    renderHook(() => useWebPush("default"));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(register).not.toHaveBeenCalled();
    expect(calls).toHaveLength(0);
  });
});

// `??=` only tests for null, so a rejected registration used to be cached and
// handed to every later caller - including the one that has to unsubscribe a
// browser whose permission was revoked, which then could not even look the
// worker up.
describe("a registration that failed once", () => {
  it("is not remembered", async () => {
    const existing = fakeSubscription("https://push.example.net/send/old", keyBytes(KEY));
    const { getRegistration } = browser(existing);
    const register = vi.fn(() => Promise.reject(new Error("network")));
    Object.defineProperty(navigator, "serviceWorker", {
      value: { register, getRegistration },
      configurable: true,
    });
    const calls = daemon();

    await expect(enable()).rejects.toThrow("network");
    // The later unsubscribe finds the worker the browser already had.
    await disable();
    expect(existing.unsubscribe).toHaveBeenCalled();
    expect(calls.some((c) => c.method === "DELETE")).toBe(true);
  });
});
