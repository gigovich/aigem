import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Composer } from "./Composer";

afterEach(cleanup);

function renderComposer(connected: boolean, accepted: boolean) {
  const onSend = vi.fn(() => accepted);
  render(<Composer maxBytes={64} connected={connected} onSend={onSend} />);
  const box = screen.getByRole("textbox", { name: "Message" });
  return { onSend, box };
}

function type(box: HTMLElement, text: string) {
  fireEvent.change(box, { target: { value: text } });
}

describe("Composer", () => {
  it("clears the draft once the message is on the wire", () => {
    const { onSend, box } = renderComposer(true, true);
    type(box, "the logout at 03:00 is back");

    fireEvent.keyDown(box, { key: "Enter" });

    expect(onSend).toHaveBeenCalledWith("the logout at 03:00 is back");
    expect((box as HTMLTextAreaElement).value).toBe("");
  });

  it("keeps the draft and says so when the socket did not take it", () => {
    // The socket drops writes while it reconnects. Clearing the box for a
    // message that never left is how a reply disappears with nothing said.
    const { box } = renderComposer(false, false);
    type(box, "please look at the retries");

    fireEvent.keyDown(box, { key: "Enter" });

    expect((box as HTMLTextAreaElement).value).toBe("please look at the retries");
    expect(screen.getByRole("alert")).toHaveTextContent("Not connected");
  });

  it("refuses a message the daemon would refuse, before spending it", () => {
    const { onSend, box } = renderComposer(true, true);
    type(box, "x".repeat(65));

    fireEvent.keyDown(box, { key: "Enter" });

    expect(onSend).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("65 bytes; this daemon accepts 64");
    expect((box as HTMLTextAreaElement).value).toHaveLength(65);
  });

  it("does not send on Enter with a modifier, which is a newline", () => {
    const { onSend, box } = renderComposer(true, true);
    type(box, "line one");

    fireEvent.keyDown(box, { key: "Enter", shiftKey: true });

    expect(onSend).not.toHaveBeenCalled();
  });
});
