import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { SessionView } from "@/lib/protocol";
import { Sidebar } from "./sidebar";

afterEach(cleanup);

const list: SessionView[] = [
  { id: "a", title: "Rename the flag", started: "", running: true, seq: 4 },
  { id: "b", title: "", started: "", running: false, seq: 1 },
];

function renderSidebar() {
  const onSelect = vi.fn();
  const onCreate = vi.fn();
  const onCloseConversation = vi.fn();
  render(
    <Sidebar
      list={list}
      activeID="a"
      onSelect={onSelect}
      onCreate={onCreate}
      onCloseConversation={onCloseConversation}
    />,
  );
  return { onSelect, onCreate, onCloseConversation };
}

describe("Sidebar", () => {
  it("marks the open conversation and names an untitled one", () => {
    renderSidebar();

    expect(screen.getByRole("button", { name: "Rename the flag" })).toHaveAttribute(
      "aria-current",
      "true",
    );
    expect(screen.getByRole("button", { name: "new conversation" })).not.toHaveAttribute(
      "aria-current",
    );
  });

  it("selects and closes without one gesture doing the other", () => {
    const { onSelect, onCloseConversation, onCreate } = renderSidebar();

    fireEvent.click(screen.getByRole("button", { name: "Rename the flag" }));
    expect(onSelect).toHaveBeenCalledWith("a");
    expect(onCloseConversation).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Close Rename the flag" }));
    expect(onCloseConversation).toHaveBeenCalledWith("a");

    fireEvent.click(screen.getByRole("button", { name: "New conversation" }));
    expect(onCreate).toHaveBeenCalledOnce();
  });
});
