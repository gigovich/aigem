import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Actor } from "@/lib/chatprotocol";
import { NewThread } from "./NewThread";

afterEach(cleanup);

const fleet: Actor[] = [
  { id: "bot:amiran", kind: "bot", name: "amiran", role: "developer", present: true, created: "" },
  { id: "bot:demetre", kind: "bot", name: "demetre", role: "tester", present: false, created: "" },
  { id: "human:operator", kind: "human", name: "operator", present: false, created: "" },
];

function renderNew(onCreate = vi.fn(() => Promise.resolve())) {
  const onCancel = vi.fn();
  render(<NewThread fleet={fleet} maxTitleChars={200} onCancel={onCancel} onCreate={onCreate} />);
  return {
    onCreate,
    onCancel,
    title: screen.getByLabelText("Title, optional"),
    text: screen.getByLabelText("What needs doing"),
    open: screen.getByRole("button", { name: "Open thread" }),
  };
}

describe("NewThread", () => {
  it("offers the bots and not the operator, and says which can answer", () => {
    renderNew();

    expect(screen.getByRole("button", { name: /amiran/ })).toHaveTextContent("ready");
    // A bot the fleet has not started cannot answer, and picking it without
    // knowing that is how a thread sits waiting on nobody.
    expect(screen.getByRole("button", { name: /demetre/ })).toHaveTextContent("stopped");
    expect(screen.queryByRole("button", { name: /operator/ })).toBeNull();
  });

  it("will not open a thread with nobody in it", () => {
    // Naming who is in it is the whole difference from a channel: a thread with
    // no bot wakes no one and can never be answered.
    const { text, open } = renderNew();
    fireEvent.change(text, { target: { value: "fix the retries" } });

    expect(open).toBeDisabled();
  });

  it("will not open a thread with nothing to say", () => {
    const { open } = renderNew();
    fireEvent.click(screen.getByRole("button", { name: /amiran/ }));

    expect(open).toBeDisabled();
  });

  it("opens with the picked bots and trims what it sends", async () => {
    const { onCreate, title, text, open } = renderNew();
    fireEvent.click(screen.getByRole("button", { name: /amiran/ }));
    fireEvent.click(screen.getByRole("button", { name: /demetre/ }));
    fireEvent.change(title, { target: { value: "  refresh-token rotation  " } });
    fireEvent.change(text, { target: { value: "  the logout at 03:00 is back  " } });

    fireEvent.click(open);

    await waitFor(() =>
      expect(onCreate).toHaveBeenCalledWith(
        "refresh-token rotation",
        ["bot:amiran", "bot:demetre"],
        "the logout at 03:00 is back",
      ),
    );
  });

  it("un-picks a bot that was picked", () => {
    const { text, open } = renderNew();
    const amiran = screen.getByRole("button", { name: /amiran/ });
    fireEvent.change(text, { target: { value: "fix the retries" } });

    fireEvent.click(amiran);
    expect(open).toBeEnabled();
    fireEvent.click(amiran);

    expect(amiran).toHaveAttribute("aria-pressed", "false");
    expect(open).toBeDisabled();
  });

  it("bounds the title to what the daemon accepts", () => {
    // Enforced from the daemon's own number, so a title is not refused after
    // it has been composed.
    const { title } = renderNew();
    expect(title).toHaveAttribute("maxLength", "200");
  });

  it("keeps what was typed when the daemon refuses", async () => {
    const onCreate = vi.fn(() => Promise.reject(new Error("400: bot:ghost is not in the fleet")));
    const { text, open } = renderNew(onCreate);
    fireEvent.click(screen.getByRole("button", { name: /amiran/ }));
    fireEvent.change(text, { target: { value: "fix the retries" } });

    fireEvent.click(open);

    expect(await screen.findByRole("alert")).toHaveTextContent("is not in the fleet");
    expect((text as HTMLTextAreaElement).value).toBe("fix the retries");
  });

  it("submits on Enter, and takes a newline with a modifier", () => {
    const { onCreate, text } = renderNew();
    fireEvent.click(screen.getByRole("button", { name: /amiran/ }));
    fireEvent.change(text, { target: { value: "fix the retries" } });

    fireEvent.keyDown(text, { key: "Enter", shiftKey: true });
    expect(onCreate).not.toHaveBeenCalled();

    fireEvent.keyDown(text, { key: "Enter" });
    expect(onCreate).toHaveBeenCalledTimes(1);
  });
});
