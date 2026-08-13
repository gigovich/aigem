import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { installFakeSocket } from "@/test/socket";
import App from "./App";

/** The daemon, answering only what the shell asks before it picks a screen.
 *  Everything past that is the screens' own business and is stubbed thin. */
function stubDaemon(modes: { sessions: boolean; chat: boolean }) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      const body = url.includes("/api/modes")
        ? modes
        : url.includes("/api/chat/meta")
          ? {
              operator: "human:operator",
              states: ["needs_you", "working", "waiting", "idle"],
              max_body_bytes: 1000,
              max_title_chars: 200,
              max_unread: 99,
            }
          : [];
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
    }),
  );
}

beforeEach(() => {
  installFakeSocket();
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: true,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.replaceState({}, "", "/");
  document.title = "aigem";
});

describe("App", () => {
  it("offers no screen switch on a daemon that serves one screen", async () => {
    // `aigem bot start` creates no sessions. A switch to a screen whose every
    // request 404s reads as a broken product rather than as the wrong address.
    window.history.replaceState({}, "", "/chat");
    stubDaemon({ sessions: false, chat: true });
    render(<App />);

    expect(await screen.findByRole("navigation", { name: "Threads" })).toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Screen" })).toBeNull();
  });

  it("corrects a link into the screen this daemon does not serve", async () => {
    stubDaemon({ sessions: false, chat: true });
    window.history.replaceState({}, "", "/");
    render(<App />);

    await screen.findByRole("navigation", { name: "Threads" });
    await waitFor(() => expect(window.location.pathname).toBe("/chat"));
  });

  it("leaves a shared thread link alone on a chat-only daemon", async () => {
    // Correcting by path rather than by mode used to rewrite /chat/<id> down to
    // /chat here, which lost the thread on reload and left the back arrow with
    // nothing to do.
    stubDaemon({ sessions: false, chat: true });
    window.history.replaceState({}, "", "/chat/t_945dde0c47180ba8");
    render(<App />);

    await screen.findByRole("navigation", { name: "Threads" });
    expect(window.location.pathname).toBe("/chat/t_945dde0c47180ba8");
  });

  it("offers the switch when the daemon serves both", async () => {
    stubDaemon({ sessions: true, chat: true });
    window.history.replaceState({}, "", "/chat");
    render(<App />);

    const group = await screen.findByRole("group", { name: "Screen" });
    expect(within(group).getByRole("button", { name: "Bots" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(within(group).getByRole("button", { name: "Sessions" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });

  it("says so when the daemon will not say what it serves", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.resolve(new Response("unauthorized", { status: 401 }))),
    );
    render(<App />);

    expect(await screen.findByText(/401/)).toBeInTheDocument();
  });
});
