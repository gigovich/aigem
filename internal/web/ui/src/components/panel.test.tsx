import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SidePanel } from "./panel";

afterEach(cleanup);

function renderPanel(props: Partial<Parameters<typeof SidePanel>[0]> = {}) {
  const onDismiss = vi.fn();
  render(
    <SidePanel side="left" open layout="drawer" title="Conversations" onDismiss={onDismiss} {...props}>
      <p>contents</p>
    </SidePanel>,
  );
  return { onDismiss };
}

describe("SidePanel", () => {
  it("renders nothing when closed", () => {
    renderPanel({ open: false });
    expect(screen.queryByText("contents")).not.toBeInTheDocument();
  });

  it("offers three ways out of a drawer that covers the page", () => {
    const { onDismiss } = renderPanel();

    fireEvent.click(document.querySelector("[aria-hidden]") as HTMLElement);
    fireEvent.click(screen.getByRole("button", { name: "Close Conversations" }));
    fireEvent.keyDown(window, { key: "Escape" });

    expect(onDismiss).toHaveBeenCalledTimes(3);
  });

  it("traps Tab and Shift+Tab inside a modal drawer", () => {
    render(
      <SidePanel side="left" open layout="drawer" title="Conversations" onDismiss={vi.fn()}>
        <button>First item</button>
        <button>Last item</button>
      </SidePanel>,
    );
    const drawer = screen.getByRole("dialog", { name: "Conversations" });
    const close = screen.getByRole("button", { name: "Close Conversations" });
    const last = screen.getByRole("button", { name: "Last item" });

    expect(drawer).toHaveFocus();
    fireEvent.keyDown(window, { key: "Tab" });
    expect(close).toHaveFocus();

    fireEvent.keyDown(window, { key: "Tab", shiftKey: true });
    expect(last).toHaveFocus();

    fireEvent.keyDown(window, { key: "Tab" });
    expect(close).toHaveFocus();
  });

  it("keeps focus on the drawer when it has no focusable descendants", () => {
    renderPanel();
    const drawer = screen.getByRole("dialog", { name: "Conversations" });
    const close = screen.getByRole("button", { name: "Close Conversations" });
    close.setAttribute("disabled", "");

    fireEvent.keyDown(window, { key: "Tab" });

    expect(drawer).toHaveFocus();
  });

  it("returns focus to the opener when the modal drawer closes", () => {
    const opener = document.createElement("button");
    document.body.appendChild(opener);
    opener.focus();
    const { rerender } = render(
      <SidePanel side="left" open layout="drawer" title="Conversations" onDismiss={vi.fn()}>
        <p>contents</p>
      </SidePanel>,
    );

    rerender(
      <SidePanel side="left" open={false} layout="drawer" title="Conversations" onDismiss={vi.fn()}>
        <p>contents</p>
      </SidePanel>,
    );

    expect(opener).toHaveFocus();
    opener.remove();
  });

  it("is a dialog only while it covers the page", () => {
    renderPanel();
    expect(screen.getByRole("dialog", { name: "Conversations" })).toBeInTheDocument();

    cleanup();
    const { onDismiss } = renderPanel({ layout: "docked" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    // Escape belongs to the drawer; a standing column is not something to escape.
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onDismiss).not.toHaveBeenCalled();
  });
});
