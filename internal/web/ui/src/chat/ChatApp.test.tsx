import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeSocket, installFakeSocket } from "@/test/socket";
import type { Actor, ChatMeta, Frame, Message, ThreadView } from "@/lib/chatprotocol";
import { ChatApp } from "./ChatApp";

const meta: ChatMeta = {
  operator: "human:operator",
  states: ["needs_you", "working", "waiting", "idle"],
  max_body_bytes: 262144,
  max_title_chars: 200,
  max_unread: 99,
};

const fleet: Actor[] = [
  { id: "bot:amiran", kind: "bot", name: "amiran", role: "developer", present: true, created: "" },
  { id: "human:operator", kind: "human", name: "operator", present: false, created: "" },
];

const thread: ThreadView = {
  id: "t_1",
  title: "Refresh-token rotation drops sessions",
  created: "2026-08-13T14:00:00Z",
  created_by: "human:operator",
  last_seq: 4,
  last_at: "2026-08-13T14:02:00Z",
  last_author: "bot:amiran",
  last_text: "Reproduced on staging.",
  state: "needs_you",
  participants: ["human:operator", "bot:amiran"],
  unread: 1,
  working: false,
};

const said: Message = {
  seq: 4,
  thread: "t_1",
  author: "bot:amiran",
  body: "Reproduced on staging.",
  kind: "message",
  await: true,
  created: "2026-08-13T14:02:00Z",
};

/** The daemon, as far as this screen is concerned. Only the routes it actually
 *  calls are answered; anything else is a failure worth seeing. */
function stubDaemon(messages: Message[] = [said]) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      const body = url.includes("/api/chat/meta")
        ? meta
        : url.includes("/api/chat/fleet")
          ? fleet
          : url.includes("/messages")
            ? { items: messages }
            : url.endsWith("/api/chat/threads")
              ? [thread]
              : null;
      if (body === null) throw new Error(`unexpected request: ${url}`);
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    }),
  );
}

function latest(): FakeSocket {
  const s = FakeSocket.opened[FakeSocket.opened.length - 1];
  if (!s) throw new Error("no socket was opened");
  return s;
}

/** Renders the screen with its socket connected. The hook opens the socket from
 *  an effect and only writes ops to one that is OPEN, so a test that skipped
 *  this would be asserting against a screen that is still connecting. */
function renderChat() {
  const rendered = render(<ChatApp />);
  act(() => latest().open());
  return rendered;
}

function threadRow() {
  return screen.findByRole("button", { name: /Refresh-token rotation drops sessions/ });
}

beforeEach(() => {
  // The open thread lives in the URL now, so a test that left one there would
  // hand the next one a screen already opening a thread.
  window.history.replaceState({}, "", "/chat");
  installFakeSocket();
  // Wide enough that the rail stands, which is the arrangement with both zones
  // on screen at once.
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: true,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
  stubDaemon();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  document.title = "aigem";
});

describe("ChatApp", () => {
  it("draws the inbox the daemon answered with", async () => {
    renderChat();

    const row = await threadRow();
    // The state is on the row in words, not only in the marker's colour.
    expect(row).toHaveTextContent("needs you");
    expect(screen.getByText(/^1 bot$/)).toBeInTheDocument();
    expect(screen.getByText("1 need you")).toBeInTheDocument();
  });

  it("counts what is asking for the operator in the tab title", async () => {
    renderChat();
    await waitFor(() => expect(document.title).toBe("(1) aigem"));
  });

  it("opens a thread and shows what was said in it", async () => {
    renderChat();
    const row = await threadRow();

    act(() => row.click());

    expect(await screen.findByText("Reproduced on staging.")).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Message" })).toBeInTheDocument();
    // Watching the thread is what the timeline follows, and it has to go up
    // before the page is fetched or anything said in between is lost.
    await waitFor(() =>
      expect(latest().sent.map((s) => JSON.parse(s))).toContainEqual({
        op: "watch",
        thread: "t_1",
      }),
    );
  });

  it("shows a message that arrives live in the open thread", async () => {
    renderChat();
    const row = await threadRow();
    act(() => row.click());
    await screen.findByText("Reproduced on staging.");

    const frame: Frame = {
      seq: 9,
      stream: "message",
      thread: "t_1",
      msg: { ...said, seq: 9, body: "And here is the patch." },
    };
    act(() => latest().deliver(frame));

    // Asserted by query rather than by holding the node: the message body is
    // rendered through dangerouslySetInnerHTML, so a re-render swaps the
    // element for an equal one and a held reference goes stale.
    await waitFor(() => expect(screen.queryByText("And here is the patch.")).not.toBeNull());
  });

  it("says so plainly when the daemon refuses the page", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.resolve(new Response("unauthorized", { status: 401 }))),
    );
    render(<ChatApp />);

    expect(await screen.findByText(/401/)).toBeInTheDocument();
  });
});

describe("ChatApp across a reconnect", () => {
  it("watches the open thread again on the new socket", async () => {
    // The daemon builds a fresh client watching nothing on every attach, and
    // this client closes its own socket on a desync - so a reconnect is the
    // ordinary path. Without the re-watch, the thread on screen goes quiet.
    renderChat();
    const row = await threadRow();
    act(() => row.click());
    await screen.findByText("Reproduced on staging.");
    const first = latest();

    act(() => first.close());
    await act(async () => {
      await new Promise((r) => setTimeout(r, 400));
    });
    const second = latest();
    expect(second).not.toBe(first);
    act(() => second.open());

    expect(second.sent.map((s) => JSON.parse(s))).toContainEqual({ op: "watch", thread: "t_1" });
  });
});

describe("ChatApp when one request fails", () => {
  it("keeps the screen and says so inline, rather than taking everything down", async () => {
    // The socket layer is built to survive a blip. A thread page landing on the
    // same blip must not undo that by replacing the inbox, the socket and every
    // other thread with an error screen.
    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) => {
        const url = String(input);
        if (url.includes("/messages")) {
          return Promise.resolve(new Response("no such thread", { status: 404 }));
        }
        const body = url.includes("/api/chat/meta")
          ? meta
          : url.includes("/api/chat/fleet")
            ? fleet
            : [thread];
        return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
      }),
    );
    renderChat();
    const row = await threadRow();

    act(() => row.click());

    expect(await screen.findByRole("alert")).toHaveTextContent("404");
    expect(screen.getByRole("navigation", { name: "Threads" })).toBeInTheDocument();
    act(() => screen.getByRole("button", { name: "Dismiss" }).click());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("marks the thread read once the socket comes back", async () => {
    // The mark travels over the socket, so one written while it was down was
    // dropped and never retried - leaving a thread the operator had read
    // sitting unread until someone said something else in it.
    renderChat();
    const row = await threadRow();
    act(() => row.click());
    await screen.findByText("Reproduced on staging.");
    const first = latest();

    act(() => first.close());
    await act(async () => {
      await new Promise((r) => setTimeout(r, 400));
    });
    const second = latest();
    act(() => second.open());

    await waitFor(() =>
      expect(second.sent.map((s) => JSON.parse(s))).toContainEqual({
        op: "read",
        thread: "t_1",
        seq: 4,
      }),
    );
  });
});

describe("ChatApp on a phone", () => {
  /** Narrow: the rail does not dock, which is the arrangement every other test
   *  here misses because it stubs matchMedia to true for every query. */
  function phone() {
    vi.stubGlobal("matchMedia", (query: string) => ({
      matches: !query.includes("768") && !query.includes("1280"),
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
    }));
  }

  it("gives the inbox the whole column when no thread is open", async () => {
    // The thread zone is still in the tree at this width. Left as flex-1 it
    // took half a 375px viewport to show nothing, and the inbox drew its rows
    // in the other half.
    phone();
    renderChat();
    await threadRow();

    const main = document.querySelector("main");
    expect(main?.className).toContain("hidden");
    expect(main?.className).not.toContain("flex-1");
  });

  it("swaps the inbox for the thread, and back again", async () => {
    phone();
    renderChat();
    const row = await threadRow();

    act(() => row.click());
    await screen.findByText("Reproduced on staging.");
    // One column at a time: the list is gone while the thread is up, rather
    // than rendered a second time inside a drawer over it.
    expect(screen.queryByRole("navigation", { name: "Threads" })).toBeNull();

    act(() => screen.getByRole("button", { name: "Threads" }).click());

    expect(await screen.findByRole("navigation", { name: "Threads" })).toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "Message" })).toBeNull();
  });

  it("opens the thread a shared link names", async () => {
    phone();
    window.history.replaceState({}, "", "/chat/t_1");
    renderChat();

    expect(await screen.findByText("Reproduced on staging.")).toBeInTheDocument();
    // The link survives arrival: a daemon serving only this mode used to
    // rewrite it away, which lost the thread on reload and left the back
    // button doing nothing.
    expect(window.location.pathname).toBe("/chat/t_1");
  });

  it("says so when the link names a thread that is gone", async () => {
    // No thread means no thread pane, and the notice used to live inside one.
    phone();
    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) => {
        const url = String(input);
        if (url.includes("/messages")) {
          return Promise.resolve(new Response("no such thread", { status: 404 }));
        }
        const body = url.includes("/api/chat/meta")
          ? meta
          : url.includes("/api/chat/fleet")
            ? fleet
            : [];
        return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
      }),
    );
    window.history.replaceState({}, "", "/chat/t_deleted");
    renderChat();

    expect(await screen.findByRole("alert")).toHaveTextContent("404");
  });
});

describe("ChatApp paging back", () => {
  it("asks for the older page from the cursor the daemon named", async () => {
    // The envelope's whole point: a client that took the lowest seq it saw
    // would be right until the last page, which is where it loses history.
    const fetches: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) => {
        const url = String(input);
        fetches.push(url);
        if (url.includes("/messages")) {
          const older = url.includes("before=");
          const body = older
            ? { items: [{ ...said, seq: 2, body: "the first thing said" }] }
            : { items: [said], cursor: 4, more: true };
          return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
        }
        const body = url.includes("/api/chat/meta")
          ? meta
          : url.includes("/api/chat/fleet")
            ? fleet
            : [thread];
        return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
      }),
    );
    renderChat();
    const row = await threadRow();
    act(() => row.click());
    await screen.findByText("Reproduced on staging.");

    const older = await screen.findByRole("button", { name: "Older messages" });
    act(() => older.click());

    expect(await screen.findByText("the first thing said")).toBeInTheDocument();
    expect(fetches.some((u) => u.includes("before=4"))).toBe(true);
    // The last page ended the thread, so the button goes.
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Older messages" })).toBeNull(),
    );
  });
});
