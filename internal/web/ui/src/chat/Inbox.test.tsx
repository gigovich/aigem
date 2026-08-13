import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { chatReducer, emptyChat, type ChatState } from "@/lib/chat";
import type { ThreadState, ThreadView } from "@/lib/chatprotocol";
import { Inbox } from "./Inbox";

afterEach(cleanup);

function view(id: string, over: Partial<ThreadView> = {}): ThreadView {
  return {
    id,
    title: id,
    created: "2026-08-13T09:00:00Z",
    created_by: "human:operator",
    last_seq: 1,
    last_at: "2026-08-13T09:00:00Z",
    state: "idle",
    participants: ["human:operator", "bot:amiran"],
    unread: 0,
    working: false,
    ...over,
  };
}

const STATES: ThreadState[] = ["needs_you", "working", "waiting", "idle"];

function stateWith(views: ThreadView[]): ChatState {
  return chatReducer(emptyChat, { t: "inbox", views });
}

function renderInbox(state: ChatState, loaded = true) {
  const onSelect = vi.fn();
  const onLoadDone = vi.fn(() => Promise.resolve());
  render(
    <Inbox
      state={state}
      fleet={[]}
      activeID={null}
      maxUnread={99}
      states={STATES}
      operator="human:operator"
      loaded={loaded}
      onSelect={onSelect}
      onCreate={vi.fn(() => Promise.resolve())}
      onLoadDone={onLoadDone}
      maxTitleChars={200}
    />,
  );
  return { onSelect, onLoadDone };
}

/** The chips, scoped to their group: a row for a thread that needs the operator
 *  carries the same words in its accessible name. */
function chip(name: RegExp) {
  return within(screen.getByRole("group", { name: "Filter threads" })).getByRole("button", { name });
}

function rowNames(): string[] {
  return screen
    .getAllByRole("button")
    .map((b) => b.getAttribute("title"))
    .filter((t): t is string => !!t);
}

describe("Inbox", () => {
  it("draws a chip per state the daemon reports, with its count", () => {
    // The chips come from the daemon's own list, so a state added there is not
    // invisible here until the bundle is rebuilt.
    renderInbox(
      stateWith([
        view("t_1", { state: "needs_you" }),
        view("t_2", { state: "needs_you" }),
        view("t_3", { state: "working" }),
      ]),
    );

    const filters = screen.getByRole("group", { name: "Filter threads" });
    expect(filters).toHaveTextContent("needs you2");
    expect(filters).toHaveTextContent("working1");
    // A chip with nothing behind it carries no number rather than a zero.
    expect(filters).not.toHaveTextContent("waiting0");
  });

  it("narrows to one state, and the same chip clears it", () => {
    renderInbox(
      stateWith([view("t_1", { state: "needs_you" }), view("t_2", { state: "idle" })]),
    );
    const needsYou = chip(/needs you/);

    fireEvent.click(needsYou);
    expect(needsYou).toHaveAttribute("aria-pressed", "true");
    expect(rowNames()).toEqual(["t_1"]);

    fireEvent.click(needsYou);
    expect(needsYou).toHaveAttribute("aria-pressed", "false");
    expect(rowNames()).toEqual(["t_1", "t_2"]);
  });

  it("says what is empty rather than showing an empty list", () => {
    renderInbox(stateWith([view("t_1", { state: "idle" })]));

    fireEvent.click(chip(/needs you/));

    expect(screen.getByText("Nothing is needs you.")).toBeInTheDocument();
  });

  it("names what the rail will hold when there is nothing at all", () => {
    renderInbox(stateWith([]));
    expect(screen.getByText(/A thread is one task/)).toBeInTheDocument();
  });

  it("shows the skeleton in place of the rows, not below them", () => {
    const { container } = render(
      <Inbox
        state={stateWith([])}
        fleet={[]}
        activeID={null}
        maxUnread={99}
        states={STATES}
        operator="human:operator"
        loaded={false}
        onSelect={vi.fn()}
        onCreate={vi.fn(() => Promise.resolve())}
        onLoadDone={vi.fn(() => Promise.resolve())}
        maxTitleChars={200}
      />,
    );

    const scroller = container.querySelector(".overflow-y-auto");
    expect(scroller?.querySelector("[aria-hidden]")).not.toBeNull();
    expect(screen.queryByText(/A thread is one task/)).toBeNull();
  });

  it("fetches the archived threads only when the done block is opened", async () => {
    // They are not in the inbox response, so without this the list is empty
    // whatever is actually archived.
    const { onLoadDone } = renderInbox(stateWith([view("t_1")]));
    expect(onLoadDone).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: /done/ }));

    await waitFor(() => expect(onLoadDone).toHaveBeenCalledTimes(1));
  });

  it("keeps archived threads out of the inbox and inside the done block", () => {
    renderInbox(stateWith([view("t_live"), view("t_done", { archived: true })]));
    expect(rowNames()).toEqual(["t_live"]);

    fireEvent.click(screen.getByRole("button", { name: /done/ }));

    expect(rowNames()).toEqual(["t_live", "t_done"]);
  });
});
