import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Header } from "./header";

afterEach(cleanup);

function renderHeader() {
  const onToggleFiles = vi.fn();
  const onToggleProviders = vi.fn();
  render(
    <Header
      conversationCount={3}
      title="Responsive header"
      model="test/model"
      tokens={12_000}
      ctx={64_000}
      clientCount={2}
      connected={false}
      onToggleConversations={vi.fn()}
      onToggleFiles={onToggleFiles}
      onToggleProviders={onToggleProviders}
    />,
  );
  return { onToggleFiles, onToggleProviders };
}

describe("Header", () => {
  it("reveals compact controls from the mobile overflow button", () => {
    const { onToggleFiles } = renderHeader();
    const more = screen.getByRole("button", { name: "More controls" });

    expect(more).toHaveAttribute("aria-expanded", "false");
    expect(more).toHaveClass("lg:hidden");
    expect(screen.getByRole("group", { name: "Desktop session controls" })).toHaveClass(
      "hidden",
      "lg:flex",
    );
    expect(document.querySelector("#mobile-header-controls")).not.toBeInTheDocument();

    fireEvent.click(more);

    expect(more).toHaveAttribute("aria-expanded", "true");
    const controls = document.querySelector("#mobile-header-controls");
    expect(controls).toBeInTheDocument();
    expect(controls).toHaveClass("lg:hidden");
    fireEvent.click(within(controls as HTMLElement).getByRole("button", { name: "Files" }));

    expect(onToggleFiles).toHaveBeenCalledOnce();
    expect(document.querySelector("#mobile-header-controls")).not.toBeInTheDocument();
  });

  it("keeps the conversation switcher directly available", () => {
    const onToggleConversations = vi.fn();
    render(
      <Header
        conversationCount={1}
        title=""
        model=""
        tokens={0}
        ctx={0}
        clientCount={1}
        connected
        onToggleConversations={onToggleConversations}
        onToggleFiles={vi.fn()}
        onToggleProviders={vi.fn()}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Conversations" }));
    expect(onToggleConversations).toHaveBeenCalledOnce();
  });
});
