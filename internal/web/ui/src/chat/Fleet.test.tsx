import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { FleetMember } from "@/lib/chatprotocol";
import { Fleet } from "./Fleet";

beforeEach(() => {
  // The screen's spend block asks the daemon for the provider's quota. It is
  // not what these tests are about, and a provider that reports none renders
  // nothing - which is what an empty list means here.
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve(
        new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } }),
      ),
    ),
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
  expect(within(row).getByText("xai/grok-4.3")).toBeInTheDocument();
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
