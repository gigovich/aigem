import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useNotifications, useTitleBadge } from "./notify";
import { register, resetPush } from "./push";

const titleOf = (id: string) => `thread ${id}`;

/** settled waits for the promise chain raise() dispatches through. Waiting on
 *  `drain` instead proves nothing: it fires synchronously in the effect's
 *  finally, before any notification could have been raised, so a negative
 *  assertion made after it can never fail. */
async function settled() {
  await new Promise((resolve) => setTimeout(resolve, 20));
}

/** hidden is the state the whole feature is for: the operator is somewhere
 *  else. jsdom reports "visible" and has no way to set it. */
function hide(state: "visible" | "hidden") {
  Object.defineProperty(document, "visibilityState", { value: state, configurable: true });
}

/** worker installs a service worker registration for this page, the way a page
 *  that has subscribed to push has one. */
function worker() {
  const showNotification = vi.fn(() => Promise.resolve());
  Object.defineProperty(navigator, "serviceWorker", {
    value: { register: vi.fn(() => Promise.resolve({ showNotification })) },
    configurable: true,
  });
  register();
  return showNotification;
}

/** constructor stubs the Notification global, which jsdom does not implement.
 *  throws makes it behave the way Chrome on Android does: permission granted,
 *  and then a TypeError from the constructor. */
function constructor(throws = false) {
  const made: { title: string; options: NotificationOptions; onclick?: () => void }[] = [];
  const Fake = vi.fn(function (this: Record<string, unknown>, title: string,
    options: NotificationOptions) {
    if (throws) throw new TypeError("Illegal constructor. Use ServiceWorkerRegistration.");
    const entry: { title: string; options: NotificationOptions; onclick?: () => void } = {
      title,
      options,
    };
    made.push(entry);
    // The page sets onclick after construction; keep whatever it assigns.
    Object.defineProperty(this, "onclick", {
      set(fn: () => void) {
        entry.onclick = fn;
      },
      configurable: true,
    });
  });
  vi.stubGlobal("Notification", Object.assign(Fake, { permission: "granted" }));
  return made;
}

beforeEach(() => {
  resetPush();
  hide("hidden");
});

afterEach(() => {
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, "serviceWorker");
});

describe("raising a notification for a thread that is asking", () => {
  it("goes through the service worker when the page has one", async () => {
    const showNotification = worker();
    const made = constructor();

    const drain = vi.fn();
    renderHook(() => useNotifications(["t_abc"], titleOf, drain));
    await vi.waitFor(() => expect(showNotification).toHaveBeenCalled());

    expect(showNotification).toHaveBeenCalledWith("thread t_abc", {
      body: "needs you",
      tag: "t_abc",
      // Clicking it is handled by the worker, which opens what is in data.
      data: { url: "/chat/t_abc" },
    });
    // Not both: two notifications for one conversation is what the shared tag
    // exists to prevent.
    expect(made).toEqual([]);
    expect(drain).toHaveBeenCalled();
  });

  it("falls back to the constructor on a page with no worker", async () => {
    const made = constructor();
    const drain = vi.fn();

    renderHook(() => useNotifications(["t_abc"], titleOf, drain));
    await vi.waitFor(() => expect(made).toHaveLength(1));

    expect(made[0].title).toBe("thread t_abc");
    expect(made[0].options.tag).toBe("t_abc");
  });

  // A notification raised by the constructor never reaches the service worker,
  // so this page has to handle its own click. Without that, tapping it does
  // nothing at all.
  it("opens the thread when its own notification is clicked", async () => {
    const made = constructor();
    const assign = vi.fn();
    Object.defineProperty(window, "location", {
      value: { assign, href: "http://localhost/chat" },
      configurable: true,
    });
    const focus = vi.spyOn(window, "focus").mockImplementation(() => undefined);

    renderHook(() => useNotifications(["t_abc"], titleOf, vi.fn()));
    await vi.waitFor(() => expect(made).toHaveLength(1));

    made[0].onclick?.();
    expect(assign).toHaveBeenCalledWith("/chat/t_abc");
    expect(focus).toHaveBeenCalled();
  });

  // Chrome on Android grants the permission and then throws from the
  // constructor. Unguarded, that throw escaped the effect and React unmounted
  // the whole screen, so a phone lost the inbox the moment a bot asked it
  // something.
  it("survives a browser that grants the permission and refuses the constructor", async () => {
    constructor(true);
    const drain = vi.fn();

    expect(() =>
      renderHook(() => useNotifications(["t_abc"], titleOf, drain)),
    ).not.toThrow();
    await vi.waitFor(() => expect(drain).toHaveBeenCalled());
  });

  it("stays quiet while the operator is looking at the page", async () => {
    hide("visible");
    const showNotification = worker();
    const made = constructor();
    const drain = vi.fn();

    renderHook(() => useNotifications(["t_abc"], titleOf, drain));
    await vi.waitFor(() => expect(drain).toHaveBeenCalled());
    await settled();

    expect(showNotification).not.toHaveBeenCalled();
    expect(made).toEqual([]);
  });

  it("does nothing at all without the permission", async () => {
    const showNotification = worker();
    vi.stubGlobal("Notification", Object.assign(vi.fn(), { permission: "default" }));
    const drain = vi.fn();

    renderHook(() => useNotifications(["t_abc"], titleOf, drain));
    await vi.waitFor(() => expect(drain).toHaveBeenCalled());
    await settled();
    expect(showNotification).not.toHaveBeenCalled();
  });
});

describe("asking for the permission", () => {
  // From a click, never on load: a page that asks the moment it opens is one
  // most browsers now refuse on the reader's behalf.
  it("reports what the browser answered", async () => {
    const request = vi.fn(() => Promise.resolve("granted"));
    vi.stubGlobal("Notification", Object.assign(vi.fn(), {
      permission: "default",
      requestPermission: request,
    }));

    const { result } = renderHook(() => useNotifications([], titleOf, vi.fn()));
    expect(result.current.permission).toBe("default");
    await act(() => result.current.ask());
    expect(request).toHaveBeenCalled();
    expect(result.current.permission).toBe("granted");
  });
});

describe("the tab title", () => {
  it("carries the count, and gives it back", () => {
    document.title = "something else";
    const { rerender, unmount } = renderHook(({ n }) => useTitleBadge(n), {
      initialProps: { n: 2 },
    });
    expect(document.title).toBe("(2) aigem");
    rerender({ n: 0 });
    expect(document.title).toBe("aigem");

    // Set back to a badge, so unmounting has something to undo: asserting the
    // restore from a title that is already "aigem" asserts nothing.
    rerender({ n: 3 });
    expect(document.title).toBe("(3) aigem");
    unmount();
    expect(document.title).toBe("aigem");
  });
});
