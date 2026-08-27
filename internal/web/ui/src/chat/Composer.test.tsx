import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Actor } from "@/lib/chatprotocol";
import { Composer } from "./Composer";

afterEach(cleanup);

function renderComposer(connected: boolean, accepted: boolean) {
  const onSend = vi.fn(() => accepted);
  render(
    <Composer
      maxBytes={64}
      connected={connected}
      fleet={[]}
      participants={[]}
      onSend={onSend}
      onAdd={vi.fn()}
    />,
  );
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

describe("Composer images", () => {
  function imageEvent(box: HTMLElement, name = "shot.png") {
    const file = new File([new Uint8Array([1, 2, 3])], name, { type: "image/png" });
    fireEvent.paste(box, { clipboardData: { items: [{ kind: "file", type: "image/png", getAsFile: () => file }] } });
    return file;
  }

  it("previews and removes a pasted image", () => {
    const onSend = vi.fn(() => true);
    render(<Composer maxBytes={64000} connected fleet={[]} participants={[]} onSend={onSend} onAdd={vi.fn()} />);
    const box = screen.getByRole("textbox", { name: "Message" });
    imageEvent(box);
    expect(screen.getByRole("img", { name: /shot.png/ })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Remove image 1" }));
    expect(screen.queryByRole("img", { name: /shot.png/ })).toBeNull();
  });

  it("uploads sequentially and sends an image-only message", async () => {
    const order: string[] = [];
    const onUpload = vi.fn(async (file: File) => { order.push(file.name); return { id: `a${order.length}` }; });
    const onSend = vi.fn(() => true);
    render(<Composer maxBytes={64000} connected fleet={[]} participants={[]} onSend={onSend} onAdd={vi.fn()} onUpload={onUpload} />);
    const box = screen.getByRole("textbox", { name: "Message" });
    imageEvent(box);
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(onSend).toHaveBeenCalledWith("", ["a1"]));
    expect(order).toEqual(["shot.png"]);
    expect((box as HTMLTextAreaElement).value).toBe("");
  });

  it("keeps uploaded IDs and does not duplicate upload on retry", async () => {
    let attempts = 0;
    const onUpload = vi.fn(async () => { attempts += 1; if (attempts === 1) throw new Error("temporary"); return { id: "a1" }; });
    const onSend = vi.fn(() => true);
    render(<Composer maxBytes={64000} connected fleet={[]} participants={[]} onSend={onSend} onAdd={vi.fn()} onUpload={onUpload} />);
    const box = screen.getByRole("textbox", { name: "Message" });
    imageEvent(box);
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("temporary"));
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(onSend).toHaveBeenCalledWith("", ["a1"]));
    expect(onUpload).toHaveBeenCalledTimes(2);
  });

  it("rejects unsupported, oversized, and excessive images", () => {
    const onSend = vi.fn(() => true);
    render(<Composer maxBytes={64000} maxAttachmentBytes={2} maxAttachments={1} connected fleet={[]} participants={[]} onSend={onSend} onAdd={vi.fn()} />);
    const box = screen.getByRole("textbox", { name: "Message" });
    const large = new File([new Uint8Array([1, 2, 3])], "large.png", { type: "image/png" });
    fireEvent.paste(box, { clipboardData: { items: [{ kind: "file", type: "image/png", getAsFile: () => large }] } });
    expect(screen.getByRole("alert")).toHaveTextContent("larger than 2");
    const svg = new File(["<svg />"], "bad.svg", { type: "image/svg+xml" });
    fireEvent.paste(box, { clipboardData: { items: [{ kind: "file", type: "image/svg+xml", getAsFile: () => svg }] } });
    expect(screen.getByRole("alert")).toHaveTextContent("Unsupported image type");
  });

  it("sends mixed text and image after all uploads in order", async () => {
    const order: string[] = [];
    const onUpload = vi.fn(async (file: File) => { order.push(`upload:${file.name}`); return { id: file.name }; });
    const onSend = vi.fn(() => { order.push("send"); return true; });
    render(<Composer maxBytes={64000} maxAttachments={2} connected fleet={[]} participants={[]} onSend={onSend} onAdd={vi.fn()} onUpload={onUpload} />);
    const box = screen.getByRole("textbox", { name: "Message" });
    fireEvent.change(box, { target: { value: "look" } });
    imageEvent(box, "one.png");
    const two = new File([new Uint8Array([1])], "two.png", { type: "image/png" });
    fireEvent.paste(box, { clipboardData: { items: [{ kind: "file", type: "image/png", getAsFile: () => two }] } });
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(onSend).toHaveBeenCalledWith("look", ["one.png", "two.png"]));
    expect(order).toEqual(["upload:one.png", "upload:two.png", "send"]);
  });

  it("keeps draft and attachment IDs when socket send fails", async () => {
    const onSend = vi.fn(() => false);
    render(<Composer maxBytes={64000} connected fleet={[]} participants={[]} onSend={onSend} onAdd={vi.fn()} onUpload={vi.fn(async () => ({ id: "a1" }))} />);
    const box = screen.getByRole("textbox", { name: "Message" });
    fireEvent.change(box, { target: { value: "keep this" } });
    imageEvent(box);
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("did not send"));
    expect((box as HTMLTextAreaElement).value).toBe("keep this");
    expect(screen.getByRole("img", { name: /shot.png/ })).toBeInTheDocument();
  });

  it("revokes an image URL when its preview is removed", () => {
    const revoke = vi.spyOn(URL, "revokeObjectURL");
    render(<Composer maxBytes={64000} connected fleet={[]} participants={[]} onSend={vi.fn(() => true)} onAdd={vi.fn()} />);
    const box = screen.getByRole("textbox", { name: "Message" });
    imageEvent(box);
    fireEvent.click(screen.getByRole("button", { name: "Remove image 1" }));
    expect(revoke).toHaveBeenCalledTimes(1);
    revoke.mockRestore();
  });

  it("blocks duplicate submit while an upload is pending", async () => {
    let resolve!: (value: { id: string }) => void;
    const onUpload = vi.fn(() => new Promise<{ id: string }>((r) => { resolve = r; }));
    const onSend = vi.fn(() => true);
    render(<Composer maxBytes={64000} connected fleet={[]} participants={[]} onSend={onSend} onAdd={vi.fn()} onUpload={onUpload} />);
    const box = screen.getByRole("textbox", { name: "Message" });
    imageEvent(box);
    fireEvent.keyDown(box, { key: "Enter" });
    fireEvent.keyDown(box, { key: "Enter" });
    expect(onUpload).toHaveBeenCalledTimes(1);
    resolve({ id: "a1" });
    await waitFor(() => expect(onSend).toHaveBeenCalledTimes(1));
  });
});

describe("Composer @mention", () => {
  const fleet: Actor[] = [
    { id: "bot:demetre", kind: "bot", name: "demetre", role: "tester", present: true, created: "" },
    { id: "bot:kate", kind: "bot", name: "kate", role: "architect", present: false, created: "" },
    { id: "human:operator", kind: "human", name: "operator", present: false, created: "" },
  ];

  function renderMentions(participants: string[] = []) {
    const onSend = vi.fn(() => true);
    const onAdd = vi.fn();
    render(
      <Composer
        maxBytes={64000}
        connected
        fleet={fleet}
        participants={participants}
        onSend={onSend}
        onAdd={onAdd}
      />,
    );
    const box = screen.getByRole("textbox", { name: "Message" });
    return { onSend, onAdd, box };
  }

  /** Typing sets the caret where a browser would: at the end of what was typed.
   *  The list is anchored to the caret, not to the draft. */
  function typeAt(box: HTMLElement, text: string) {
    fireEvent.change(box, { target: { value: text, selectionStart: text.length } });
  }

  it("offers the bots whose name starts with what was typed", () => {
    const { box } = renderMentions();
    typeAt(box, "@dem");
    const list = screen.getByRole("listbox", { name: "Bots you can name" });
    expect(list).toHaveTextContent("demetre");
    expect(list).not.toHaveTextContent("kate");
  });

  it("does not offer the operator, who is always already here", () => {
    const { box } = renderMentions();
    typeAt(box, "@op");
    expect(screen.queryByRole("listbox", { name: "Bots you can name" })).toBeNull();
  });

  it("does not treat an address as a mention", () => {
    const { box } = renderMentions();
    typeAt(box, "mail me at someone@dem");
    expect(screen.queryByRole("listbox", { name: "Bots you can name" })).toBeNull();
  });

  it("completes the name and closes the list", () => {
    const { box } = renderMentions();
    typeAt(box, "@dem");
    fireEvent.keyDown(box, { key: "Enter" });
    expect((box as HTMLTextAreaElement).value).toBe("@demetre ");
    expect(screen.queryByRole("listbox", { name: "Bots you can name" })).toBeNull();
  });

  it("moves through the list with the arrow keys", () => {
    const { box } = renderMentions();
    typeAt(box, "@");
    fireEvent.keyDown(box, { key: "ArrowDown" });
    fireEvent.keyDown(box, { key: "Enter" });
    expect((box as HTMLTextAreaElement).value).toBe("@kate ");
  });

  it("says a name will add a bot before the message goes", () => {
    // A membership change that happens as a side effect of a word in a sentence
    // has to be visible while there is still time to delete it.
    const { box } = renderMentions();
    typeAt(box, "@demetre can you take the QA");
    expect(screen.getByText(/adds demetre to this thread/)).toBeInTheDocument();
  });

  it("says nothing about a bot that is already in the thread", () => {
    const { box } = renderMentions(["bot:demetre"]);
    typeAt(box, "@demetre can you take the QA");
    expect(screen.queryByText(/adds demetre/)).toBeNull();
  });

  it("adds the named bot only once the message is on the wire", () => {
    const { box, onAdd } = renderMentions();
    typeAt(box, "@demetre can you take the QA");
    expect(onAdd).not.toHaveBeenCalled();
    fireEvent.keyDown(box, { key: "Enter" });
    expect(onAdd).toHaveBeenCalledWith("bot:demetre");
  });

  it("adds the bot before sending, so the message can actually name it", () => {
    // The daemon applies socket ops in order and resolves "@name" only against
    // actors already in the thread. Sending first means the mention is dropped
    // and the bot joins to a membership note it ignores - it never wakes, and
    // the message that named it was addressed to nobody.
    const order: string[] = [];
    render(
      <Composer
        maxBytes={64000}
        connected
        fleet={fleet}
        participants={[]}
        onSend={() => {
          order.push("send");
          return true;
        }}
        onAdd={() => order.push("add")}
      />,
    );
    const box = screen.getByRole("textbox", { name: "Message" });
    typeAt(box, "@demetre can you take the QA");
    fireEvent.keyDown(box, { key: "Enter" });
    expect(order).toEqual(["add", "send"]);
  });

  it("does not add a mentioned bot when an image upload fails", async () => {
    const onAdd = vi.fn();
    const onUpload = vi.fn(async () => { throw new Error("upload failed"); });
    render(<Composer maxBytes={64000} connected fleet={fleet} participants={[]} onSend={vi.fn(() => true)} onAdd={onAdd} onUpload={onUpload} />);
    const box = screen.getByRole("textbox", { name: "Message" });
    fireEvent.change(box, { target: { value: "@demetre please inspect this", selectionStart: 30 } });
    const file = new File([new Uint8Array([1])], "shot.png", { type: "image/png" });
    fireEvent.paste(box, { clipboardData: { items: [{ kind: "file", type: "image/png", getAsFile: () => file }] } });
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(onUpload).toHaveBeenCalled());
    expect(onAdd).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("upload failed");
  });

  it("names a bot however it was capitalised", () => {
    // The autocomplete matches case-insensitively, so the list offers demetre
    // for "@Dem" - and typing the rest by hand rather than picking from it used
    // to add nobody, silently.
    const { box, onAdd } = renderMentions();
    typeAt(box, "@Demetre can you take the QA");
    fireEvent.keyDown(box, { key: "Enter" });
    expect(onAdd).toHaveBeenCalledWith("bot:demetre");
  });

  it("closes the list on Escape, and reopens it on the next keystroke", () => {
    // The list is anchored on the caret, and while a mention is being typed the
    // caret is already at the end of the draft - so nudging it there was a
    // no-op that also swallowed the keypress.
    const { box } = renderMentions();
    typeAt(box, "@dem");
    fireEvent.keyDown(box, { key: "Escape" });
    expect(screen.queryByRole("listbox", { name: "Bots you can name" })).toBeNull();
    typeAt(box, "@deme");
    expect(screen.getByRole("listbox", { name: "Bots you can name" })).toBeInTheDocument();
  });
});
