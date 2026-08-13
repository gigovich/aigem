import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Turn } from "@/lib/chatprotocol";
import { ThreadPanel, planOf, tokens } from "./ThreadPanel";

afterEach(cleanup);

function turn(over: Partial<Turn>): Turn {
  return { seq: 1, thread: "t_1", actor: "bot:amiran", started: "", ...over };
}

describe("planOf", () => {
  it("takes the newest run that wrote a plan", () => {
    // Carried forward on read, not at write time: a bot revises its plan in the
    // turn it revises it and not in the twelve heartbeats after, so copying the
    // last one onto every turn would fill the table with a plan nobody set.
    const turns = [
      turn({ seq: 9 }),
      turn({ seq: 8, plan: [{ text: "patch internal/auth", status: "in_progress" }] }),
      turn({ seq: 7, plan: [{ text: "reproduce on staging", status: "completed" }] }),
    ];
    // And whose it is: a thread holds several bots, and a plan with no name on
    // it is one bot's plan presented as the thread's.
    expect(planOf(turns)).toEqual({
      plan: [{ text: "patch internal/auth", status: "in_progress" }],
      whose: "bot:amiran",
    });
  });

  it("is empty when no run has written one", () => {
    expect(planOf([turn({ seq: 9 })])).toEqual({ plan: [] });
  });
});

describe("tokens", () => {
  it("is exact below ten thousand and rounded above", () => {
    expect(tokens(0)).toBe("0");
    expect(tokens(9999)).toBe("9999");
    expect(tokens(48200)).toBe("48k");
  });
});

describe("ThreadPanel", () => {
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify([{ path: "internal/auth/flow.go", created: false }]), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
        ),
      ),
    );
  });

  it("asks for the newest run that changed a file, by number", async () => {
    // By number rather than letting the daemon pick, so the list and the diff
    // behind it stay on the same run while a later turn is starting.
    render(
      <ThreadPanel
        thread="t_1"
        operator="human:operator"
        turns={[turn({ seq: 9 }), turn({ seq: 8, files: 2 }), turn({ seq: 7, files: 1 })]}
        spend={{ usage: { input_tokens: 48000, output_tokens: 200 }, turns: 3, runs: 3 }}
        loaded
        version={0}
        onOpenDiff={vi.fn()}
      />,
    );
    await waitFor(() => {
      const calls = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.map((c) => String(c[0]));
      expect(calls.some((u) => u.includes("/artifacts?turn=8"))).toBe(true);
    });
  });

  it("counts each bot's runs and the thread's tokens", async () => {
    render(
      <ThreadPanel
        thread="t_1"
        operator="human:operator"
        turns={[
          turn({ seq: 9, actor: "bot:amiran" }),
          turn({ seq: 8, actor: "bot:amiran" }),
          turn({ seq: 7, actor: "bot:demetre" }),
        ]}
        spend={{ usage: { input_tokens: 48000, output_tokens: 200 }, turns: 3, runs: 3 }}
        loaded
        version={0}
        onOpenDiff={vi.fn()}
      />,
    );
    const panel = screen.getByRole("region", { name: "This thread" });
    expect(panel).toHaveTextContent("amiran");
    expect(panel).toHaveTextContent("2 turns");
    expect(panel).toHaveTextContent("demetre");
    expect(panel).toHaveTextContent("48k");
  });

  it("says nothing rather than zero while the runs are still loading", () => {
    // A confident "0 turns" over a thread that has eighty is worse than a
    // moment of silence, and it is the moment a thread switch always passes
    // through.
    render(
      <ThreadPanel
        thread="t_1"
        operator="human:operator"
        turns={[]}
        spend={null}
        loaded={false}
        version={0}
        onOpenDiff={vi.fn()}
      />,
    );
    expect(screen.getByRole("region", { name: "This thread" })).not.toHaveTextContent("0 runs");
  });

  it("reports its own failure inside the panel", () => {
    render(
      <ThreadPanel
        thread="t_1"
        operator="human:operator"
        turns={[]}
        spend={null}
        loaded={false}
        failed="500 reading the runs"
        version={0}
        onOpenDiff={vi.fn()}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent("500 reading the runs");
  });

  it("counts the thread's runs from the daemon, not from the page it fetched", () => {
    // The runs are fetched a hundred at a time for the summary lines; a thread
    // with more would otherwise report exactly a hundred, confidently, forever.
    render(
      <ThreadPanel
        thread="t_1"
        operator="human:operator"
        turns={[turn({ seq: 9 }), turn({ seq: 8 })]}
        spend={{ usage: {}, turns: 240, runs: 250 }}
        loaded
        version={0}
        onOpenDiff={vi.fn()}
      />,
    );
    const panel = screen.getByRole("region", { name: "This thread" });
    // Runs, not the turns that spent: a run killed before its first usage flush
    // spent nothing, and labelling that number "turns" over a list of who ran
    // what is how "240" appeared above rows summing to 250.
    expect(panel).toHaveTextContent("250 runs");
    expect(panel).toHaveTextContent("the newest 2 runs");
  });

  it("says a run's diffs were pruned rather than that it wrote nothing", async () => {
    // Retention reclaims the stored diffs and the timeline they belong to, but
    // nothing prunes the turns table - so a run keeps advertising files whose
    // content is gone. "No run in this thread has written a file", under a badge
    // reading 3, is the one reading that is simply false.
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } }),
        ),
      ),
    );
    render(
      <ThreadPanel
        thread="t_1"
        operator="human:operator"
        turns={[turn({ seq: 9, files: 3 })]}
        spend={{ usage: {}, turns: 1, runs: 1 }}
        loaded
        version={0}
        onOpenDiff={vi.fn()}
      />,
    );
    const files = await screen.findByRole("region", { name: "Changed files" });
    await waitFor(() => expect(files).toHaveTextContent(/pruned/));
    expect(files).not.toHaveTextContent("No run in this thread has written a file.");
  });

  it("says how many calls the provider reported nothing for", async () => {
    // A cost figure that quietly folds in the calls it could not count is the
    // one thing it must never do.
    render(
      <ThreadPanel
        thread="t_1"
        operator="human:operator"
        turns={[turn({ seq: 9 })]}
        spend={{ usage: { input_tokens: 100, calls: 6, uncounted: 2 }, turns: 1, runs: 1 }}
        loaded
        version={0}
        onOpenDiff={vi.fn()}
      />,
    );
    // The figures are their own monospaced spans, so the sentence is split
    // across elements; assert on the section's text rather than one node's.
    expect(screen.getByRole("region", { name: "This thread" })).toHaveTextContent(
      "2 of 6 calls reported no usage",
    );
  });
});
