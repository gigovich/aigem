import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { FleetMember } from "@/lib/chatprotocol";
import { Fleet } from "./Fleet";

beforeEach(() => {
  // The screen's spend block asks the daemon for the provider's quota. It is
  // not what these tests are about, and a provider that reports none renders
  // nothing - which is what an empty list means here.
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const body = String(input).includes("/api/chat/bots/models") ? { options: [], bots: [] } : [];
      return Promise.resolve(
        new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }),
      );
    }),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  cleanup();
});

function member(name: string, over: Partial<FleetMember> = {}): FleetMember {
  return {
    id: `bot:${name}`,
    kind: "bot",
    name,
    role: "developer",
    present: true,
    created: "2026-08-13T09:00:00Z",
    threads: 0,
    working: false,
    ...over,
  };
}

function rowFor(name: string): HTMLElement {
  const cell = screen.getByText(name);
  const row = cell.closest('[role="row"]');
  if (!(row instanceof HTMLElement)) throw new Error(`no row for ${name}`);
  return row;
}

it("draws every operational column for a running bot", () => {
  const next = new Date();
  next.setHours(next.getHours() + 1, 10, 0, 0);
  render(
    <Fleet
      loaded
      members={[
        member("amiran", {
          threads: 3,
          state: "idle",
          live: {
            running: true,
            model: "xai/grok-4.3",
            heartbeat: "30m",
            tier: 0,
            next_job: "memory-review",
            next_run: next.toISOString(),
          },
        }),
      ]}
    />,
  );
  const row = rowFor("amiran");
  expect(within(row).getByText("3")).toBeInTheDocument();
  expect(within(row).getByText("30m (t0)")).toBeInTheDocument();
  expect(within(row).getByText("running: xai/grok-4.3")).toBeInTheDocument();
  expect(within(row).getByText(/memory-review \d{2}:10/)).toBeInTheDocument();
  expect(within(row).getByText("idle")).toBeInTheDocument();
});

// The state an operator would otherwise read journalctl for, which is the whole
// point of the column.
it("says a bot the daemon could not start is stopped", () => {
  render(
    <Fleet loaded members={[member("lisa", { state: "stopped", live: { running: false, tier: 0 } })]} />,
  );
  expect(within(rowFor("lisa")).getByText("stopped")).toBeInTheDocument();
});

// A daemon that runs no bots knows nothing about them. Rendering that absence
// as "stopped" would be a confident answer nobody gave.
it("says nothing about a bot no daemon reported on", () => {
  render(<Fleet loaded members={[member("kate")]} />);
  const row = rowFor("kate");
  expect(within(row).queryByText("stopped")).not.toBeInTheDocument();
  expect(within(row).queryByText("idle")).not.toBeInTheDocument();
  // Three unknown columns - state, heartbeat and model - plus a next job the
  // daemon did not supply either.
  expect(within(row).getAllByText("-").length).toBeGreaterThanOrEqual(3);
});

// Working is read from the same turns table the inbox reads, so it wins over
// anything the process thinks it is doing.
it("shows a bot mid-run as working", () => {
  render(
    <Fleet
      loaded
      members={[
        member("demetre", {
          working: true,
          state: "working",
          live: { running: true, heartbeat: "1h", tier: 1 },
        }),
      ]}
    />,
  );
  expect(within(rowFor("demetre")).getByText("working")).toBeInTheDocument();
});

it("leaves the operator out of the roster", () => {
  render(
    <Fleet
      loaded
      members={[
        member("amiran"),
        {
          id: "human:operator",
          kind: "human",
          name: "operator",
          present: false,
          created: "",
          threads: 9,
          working: false,
        },
      ]}
    />,
  );
  expect(screen.queryByText("operator")).not.toBeInTheDocument();
  expect(screen.getByText("amiran")).toBeInTheDocument();
});

// A skeleton whose shape differs from what replaces it moves the layout at the
// moment the reader starts reading, so the header is drawn during the load too.
it("draws a skeleton, and the header it will keep, while the roster is loading", () => {
  const { container } = render(<Fleet loaded={false} members={[]} />);
  expect(container.querySelectorAll('[role="row"]')).toHaveLength(1);
  expect(screen.getByText("heartbeat")).toBeInTheDocument();
  expect(container.querySelectorAll(".animate-shimmer").length).toBeGreaterThan(0);
  expect(screen.queryByText(/No bots are configured/)).not.toBeInTheDocument();
});

it("names what is missing when no bot is configured", () => {
  render(<Fleet loaded members={[]} />);
  expect(screen.getByText(/No bots are configured/)).toBeInTheDocument();
});

const settings = {
  name: "amiran",
  role: "developer",
  configured: "xai/grok-4.3",
  selected: "xai/grok-4.3",
  source: "configured" as const,
  running: "openai/gpt-5.6-luna",
  restart_required: true,
};

function modelFetch(put?: (body: unknown) => Response | Promise<Response>) {
  const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.includes("/api/chat/bots/models")) {
      return new Response(JSON.stringify({
        options: [
          { ref: "openai/gpt-5.6-luna", name: "GPT-5.6 Luna", provider: "openai", usable: true },
          { ref: "xai/grok-4.3", name: "Grok 4.3", provider: "xai", usable: true },
          { ref: "openai/no-auth", name: "No auth", provider: "openai", usable: false, reason: "not authenticated" },
        ],
        bots: [settings],
      }), { status: 200, headers: { "Content-Type": "application/json" } });
    }
    if (url.includes("/api/chat/bots/amiran/model")) {
      return put?.(JSON.parse(String(init?.body))) ?? new Response(JSON.stringify({
        ...settings,
        configured: "openai/gpt-5.6-luna",
        selected: "openai/gpt-5.6-luna",
        restart_required: false,
      }), { status: 200, headers: { "Content-Type": "application/json" } });
    }
    return new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } });
  });
  vi.stubGlobal("fetch", fetcher);
  return fetcher;
}

it("shows selected, running, source, and restart state for a running bot", async () => {
  modelFetch();
  render(<Fleet loaded members={[member("amiran", { state: "idle", live: { running: true, model: settings.running, tier: 0 } })]} />);
  const row = rowFor("amiran");
  expect(await within(row).findByText(`selected: ${settings.selected}`)).toBeInTheDocument();
  expect(within(row).getByText(`running: ${settings.running}`)).toBeInTheDocument();
  expect(within(row).getByText("configured")).toBeInTheDocument();
  expect(within(row).getByText("restart required")).toBeInTheDocument();
});

it("saves only on server confirmation and prevents a double submit", async () => {
  let resolve!: (response: Response) => void;
  const pending = new Promise<Response>((done) => { resolve = done; });
  const fetcher = modelFetch(() => pending);
  render(<Fleet loaded members={[member("amiran", { state: "idle", live: { running: true, model: settings.running, tier: 0 } })]} />);
  fireEvent.click(await screen.findByRole("button", { name: "Change model" }));
  const select = screen.getByLabelText("Model selection");
  expect(select).toHaveFocus();
  fireEvent.change(select, { target: { value: "openai/gpt-5.6-luna" } });
  const save = screen.getByRole("button", { name: "Save" });
  fireEvent.click(save);
  fireEvent.click(save);
  expect(screen.getByText(`selected: ${settings.selected}`)).toBeInTheDocument();
  expect(save).toBeDisabled();
  expect(fetcher.mock.calls.filter(([url]) => String(url).endsWith("/model"))).toHaveLength(1);
  resolve(new Response(JSON.stringify({
    ...settings,
    configured: "openai/gpt-5.6-luna",
    selected: "openai/gpt-5.6-luna",
    restart_required: false,
  }), { status: 200, headers: { "Content-Type": "application/json" } }));
  expect(await screen.findByText("Saved.")).toBeInTheDocument();
  expect(screen.getByText("selected: openai/gpt-5.6-luna")).toBeInTheDocument();
  expect(screen.queryByText("restart required")).not.toBeInTheDocument();
});

it("preserves the previous selection after failure and permits retry", async () => {
  let writes = 0;
  modelFetch(() => {
    writes++;
    if (writes === 1) return new Response(JSON.stringify({ error: "disk full" }), { status: 500 });
    return new Response(JSON.stringify({ ...settings, configured: undefined, selected: "openai/gpt-5.6-luna", source: "role-default", running: undefined, restart_required: false }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  });
  render(<Fleet loaded members={[member("amiran", { state: "stopped", live: { running: false, tier: 0 } })]} />);
  fireEvent.click(await screen.findByRole("button", { name: "Change model" }));
  fireEvent.change(screen.getByLabelText("Model selection"), { target: { value: "__role_default__" } });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("disk full");
  expect(screen.getByText(`selected: ${settings.selected}`)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText("selected: openai/gpt-5.6-luna")).toBeInTheDocument();
  expect(screen.getByText("Saved.")).toBeInTheDocument();
});

it("closes the keyboard-accessible mobile-sized editor with Escape", async () => {
  modelFetch();
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 375 });
  render(<Fleet loaded members={[member("amiran", { state: "stopped", live: { running: false, tier: 0 } })]} />);
  const opener = await screen.findByRole("button", { name: "Change model" });
  fireEvent.click(opener);
  const dialog = screen.getByRole("dialog");
  expect(dialog).toHaveClass("w-full", "max-w-md");
  fireEvent.keyDown(dialog.parentElement!, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  expect(opener).toHaveFocus();
});
