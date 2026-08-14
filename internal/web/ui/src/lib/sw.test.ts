import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { beforeEach, describe, expect, it, vi } from "vitest";

/** The service worker is a classic script served from the origin root, not a
 *  module this bundle imports, so it is loaded the way the browser loads it:
 *  evaluated against a `self` that this file supplies. That is the only way to
 *  reach the handlers - they are what the whole feature is, and jsdom cannot
 *  host a worker to register them in. */
// From the project root, not from import.meta.url: these tests run in jsdom,
// where that is an http URL rather than a file one.
const source = readFileSync(resolve(process.cwd(), "public/sw.js"), "utf8");

type Listener = (event: Record<string, unknown>) => void;

interface Worker {
  fire: (type: string, event?: Record<string, unknown>) => Promise<void>;
  /** What the daemon answers the worker's one request with. */
  answer: number;
  shown: { title: string; options: Record<string, unknown> }[];
  windows: FakeClient[];
  opened: string[];
  fetches: { url: string; method: string; body: unknown; credentials?: RequestCredentials }[];
  skipWaiting: ReturnType<typeof vi.fn>;
  claim: ReturnType<typeof vi.fn>;
  subscribed: Record<string, unknown>[];
}

interface FakeClient {
  url: string;
  visibilityState: string;
  /** Whether this worker controls the window. A page loaded before the worker
   *  was registered is visible to matchAll only with includeUncontrolled. */
  controlled: boolean;
  focus: ReturnType<typeof vi.fn>;
  navigate?: (url: string) => Promise<FakeClient | null>;
}

const ORIGIN = "https://aigem.example.ts.net";

function client(url: string, opts: Partial<FakeClient> = {}): FakeClient {
  const c: FakeClient = {
    url,
    visibilityState: "hidden",
    controlled: true,
    focus: vi.fn(() => Promise.resolve(c)),
    navigate: vi.fn((to: string) => {
      c.url = to;
      return Promise.resolve(c);
    }),
    ...opts,
  };
  return c;
}

/** load evaluates sw.js and returns a handle on what it did. */
function load(): Worker {
  const listeners = new Map<string, Listener>();
  const worker: Worker = {
    skipWaiting: vi.fn(),
    claim: vi.fn(),
    answer: 204,
    shown: [],
    windows: [],
    opened: [],
    fetches: [],
    subscribed: [],
    fire: async (type, event = {}) => {
      const listener = listeners.get(type);
      if (!listener) throw new Error(`sw.js registered no ${type} handler`);
      let pending: Promise<unknown> | null = null;
      listener({ ...event, waitUntil: (p: Promise<unknown>) => (pending = p) });
      // A handler that does its work without waitUntil is one the browser is
      // free to kill halfway through.
      // install is the exception: skipWaiting() needs no lifetime extension.
      if (pending === null && type !== "install") {
        throw new Error(`the ${type} handler did not call waitUntil`);
      }
      await pending;
    },
  };

  const skipWaiting = vi.fn();
  const claim = vi.fn(() => Promise.resolve());
  worker.skipWaiting = skipWaiting;
  worker.claim = claim;

  const self = {
    addEventListener: (type: string, fn: Listener) => listeners.set(type, fn),
    skipWaiting,
    location: { origin: ORIGIN },
    clients: {
      claim,
      // Honours its argument, because whether a handler passes
      // includeUncontrolled is exactly the sort of thing that regresses
      // silently: an uncontrolled window is one this worker did not load.
      matchAll: vi.fn((opts: { includeUncontrolled?: boolean } = {}) =>
        Promise.resolve(
          opts.includeUncontrolled ? worker.windows : worker.windows.filter((c) => c.controlled),
        ),
      ),
      openWindow: vi.fn((url: string) => {
        worker.opened.push(url);
        return Promise.resolve(client(url));
      }),
    },
    registration: {
      showNotification: vi.fn((title: string, options: Record<string, unknown>) => {
        worker.shown.push({ title, options });
        return Promise.resolve();
      }),
      pushManager: {
        subscribe: vi.fn((options: Record<string, unknown>) => {
          worker.subscribed.push(options);
          return Promise.resolve(subscription("https://push.example.net/send/new"));
        }),
      },
    },
  };

  const fetcher = vi.fn((url: string, init: RequestInit) => {
    worker.fetches.push({
      url,
      method: init.method ?? "GET",
      credentials: init.credentials,
      body: JSON.parse(String(init.body)) as unknown,
    });
    return Promise.resolve(new Response(null, { status: worker.answer }));
  });

  new Function("self", "fetch", source)(self, fetcher);
  return worker;
}

function subscription(endpoint: string, key: string = "the-key") {
  return {
    endpoint,
    options: { applicationServerKey: key },
    toJSON: () => ({ endpoint, keys: { p256dh: "p256dh-value", auth: "auth-value" } }),
  };
}

function pushEvent(payload: unknown) {
  return { data: { json: () => payload } };
}

let sw: Worker;
beforeEach(() => {
  sw = load();
});

describe("taking over", () => {
  // A worker that waits for every old tab to close is a worker that does not
  // notify anyone today. There is no cached asset for the new one to be
  // incompatible with.
  it("activates without waiting, and claims the pages already open", async () => {
    await sw.fire("install");
    expect(sw.skipWaiting).toHaveBeenCalled();
    await sw.fire("activate");
    expect(sw.claim).toHaveBeenCalled();
  });
});

describe("a push", () => {
  it("shows the thread that is asking, tagged so it cannot stack", async () => {
    await sw.fire("push", pushEvent({
      thread: "t_abc",
      title: "the deploy",
      body: "amiran needs you",
      url: "/chat/t_abc",
    }));

    expect(sw.shown).toHaveLength(1);
    expect(sw.shown[0].title).toBe("the deploy");
    expect(sw.shown[0].options.body).toBe("amiran needs you");
    expect(sw.shown[0].options.tag).toBe("t_abc");
    expect(sw.shown[0].options.data).toEqual({ url: "/chat/t_abc" });
  });

  // A worker that receives a push and shows nothing has its subscription
  // revoked by the browser, so an unreadable payload must still raise
  // something.
  it("still shows something when the payload cannot be read", async () => {
    await sw.fire("push", {
      data: {
        json: () => {
          throw new SyntaxError("not JSON");
        },
      },
    });
    expect(sw.shown).toHaveLength(1);
    expect(sw.shown[0].title).toBe("aigem");
    expect(sw.shown[0].options.body).toBe("needs you");
    // And it still opens something rather than nothing when tapped.
    expect(sw.shown[0].options.tag).toBe("aigem");
    expect(sw.shown[0].options.data).toEqual({ url: "/chat" });
  });

  it("stays quiet while a window of this application is on screen", async () => {
    // Uncontrolled: a page loaded before this worker existed is still a page
    // the operator is looking at.
    sw.windows.push(client(ORIGIN + "/chat", { visibilityState: "visible", controlled: false }));
    await sw.fire("push", pushEvent({ thread: "t_abc", title: "the deploy" }));
    expect(sw.shown).toHaveLength(0);
  });
});

describe("clicking a notification", () => {
  const closed = vi.fn();
  const clicked = (url: string) => ({
    notification: { close: closed, data: { url } },
  });

  it("navigates the window the operator already has", async () => {
    // Uncontrolled, which is the ordinary case: the page was loaded before this
    // worker existed. A click that only looks at controlled clients opens a
    // second window beside the one the operator is holding.
    const open = client(ORIGIN + "/chat", { controlled: false });
    sw.windows.push(open);

    await sw.fire("notificationclick", clicked("/chat/t_abc"));
    expect(open.navigate).toHaveBeenCalledWith(ORIGIN + "/chat/t_abc");
    expect(open.focus).toHaveBeenCalled();
    expect(sw.opened).toEqual([]);
    // Left on screen, it is a notification for a thread that is now open.
    expect(closed).toHaveBeenCalled();
  });

  it("opens one when there is none", async () => {
    await sw.fire("notificationclick", clicked("/chat/t_abc"));
    expect(sw.opened).toEqual([ORIGIN + "/chat/t_abc"]);
  });

  // navigate() rejects for a client this worker does not control. The click
  // must still land somewhere.
  it("focuses the window when navigating it is refused", async () => {
    const open = client(ORIGIN + "/chat", {
      navigate: vi.fn(() => Promise.reject(new TypeError("not controlled"))),
    });
    sw.windows.push(open);

    await sw.fire("notificationclick", clicked("/chat/t_abc"));
    expect(open.focus).toHaveBeenCalled();
    expect(sw.opened).toEqual([]);
  });

  it("refuses to open another origin", async () => {
    await sw.fire("notificationclick", clicked("https://evil.test/steal"));
    expect(sw.opened).toEqual([]);
  });
});

describe("a rotated subscription", () => {
  it("re-subscribes with the old key and tells the daemon about both ends", async () => {
    await sw.fire("pushsubscriptionchange", {
      oldSubscription: subscription("https://push.example.net/send/old"),
    });

    expect(sw.subscribed).toHaveLength(1);
    expect(sw.subscribed[0].applicationServerKey).toBe("the-key");
    // Chrome rejects a subscription without it, which would kill the
    // re-subscription silently and for good.
    expect(sw.subscribed[0].userVisibleOnly).toBe(true);
    expect(sw.fetches).toEqual([
      {
        url: "/api/chat/push/subs",
        method: "POST",
        // The cookie the page traded its token for: the worker has no
        // credential of its own, and without this the daemon answers 401.
        credentials: "same-origin",
        body: {
          endpoint: "https://push.example.net/send/new",
          p256dh: "p256dh-value",
          auth: "auth-value",
        },
      },
      {
        url: "/api/chat/push/subs",
        method: "DELETE",
        credentials: "same-origin",
        body: { endpoint: "https://push.example.net/send/old" },
      },
    ]);
  });

  // The daemon revokes every cookie when it restarts, so the worker's one
  // request can be refused. The old endpoint is then the only one the daemon
  // has, and forgetting it would leave nothing for the next page load to
  // repair from.
  // Firefox hands the new subscription to the event rather than making the
  // worker ask for one.
  it("uses the subscription the browser supplies, when it supplies one", async () => {
    await sw.fire("pushsubscriptionchange", {
      oldSubscription: subscription("https://push.example.net/send/old"),
      newSubscription: subscription("https://push.example.net/send/fresh"),
    });
    expect(sw.subscribed).toEqual([]);
    expect(sw.fetches[0].body).toMatchObject({ endpoint: "https://push.example.net/send/fresh" });
  });

  // Nothing to delete: the endpoint did not move, and deleting it would forget
  // the subscription that was just stored.
  it("does not forget an endpoint that did not change", async () => {
    const same = subscription("https://push.example.net/send/same");
    await sw.fire("pushsubscriptionchange", { oldSubscription: same, newSubscription: same });
    expect(sw.fetches.map((f) => f.method)).toEqual(["POST"]);
  });

  it("keeps the old endpoint when the daemon refuses the new one", async () => {
    sw.answer = 401;
    await sw.fire("pushsubscriptionchange", {
      oldSubscription: subscription("https://push.example.net/send/old"),
    });
    expect(sw.fetches.map((f) => f.method)).toEqual(["POST"]);
  });

  it("does nothing when the browser did not say what the old key was", async () => {
    await sw.fire("pushsubscriptionchange", {});
    expect(sw.subscribed).toEqual([]);
    expect(sw.fetches).toEqual([]);
  });
});
