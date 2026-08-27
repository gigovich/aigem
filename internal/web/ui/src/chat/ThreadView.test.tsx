import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Message, ThreadView as Thread } from "@/lib/chatprotocol";
import type { ThreadMessages } from "@/lib/chat";
import { ThreadPane } from "./ThreadView";

afterEach(cleanup);

const thread: Thread = {
  id: "t_1", title: "Images", created: "", created_by: "human:operator", last_seq: 1,
  last_at: "", state: "waiting", participants: ["human:operator"], unread: 0, working: false,
};

const message: Message = {
  seq: 1, thread: "t_1", author: "human:operator", body: "see this", kind: "message", created: "",
  attachments: ["att_1"],
};

function renderPane(items: Message[] = [message]) {
  const held: ThreadMessages = { items, cursor: 0, more: false, loaded: true };
  return render(<ThreadPane thread={thread} operator="human:operator" held={held} onOlder={vi.fn()} />);
}

describe("ThreadPane attachments", () => {
  it("renders attachment images with a stable endpoint and accessible label", () => {
    renderPane();
    const image = screen.getByRole("img", { name: "Attached image 1" });
    expect(image).toHaveAttribute("src", "/api/chat/attachments/att_1");
    expect(screen.getByRole("link")).toHaveAttribute("href", "/api/chat/attachments/att_1");
  });

  it("shows a visible fallback when an attachment cannot load", () => {
    renderPane();
    fireEvent.error(screen.getByRole("img", { name: "Attached image 1" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Attachment unavailable");
  });
});
