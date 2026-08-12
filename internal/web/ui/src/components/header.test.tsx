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
      navOpen={false}
      railOpen={false}
      onToggleConversations={vi.fn()}
      onToggleFiles={onToggleFiles}
      onToggleProviders={onToggleProviders}
    />,
  );
  return { onToggleFiles, onToggleProviders };
}

describe("Header", () => {
  it("reveals compact controls from the mobile overflow button", () => {
    const { onToggleProviders } = renderHeader();
    const more = screen.getByRole("button", { name: "More controls" });

    expect(more).toHaveAttribute("aria-expanded", "false");
    expect(more).toHaveClass("md:hidden");
    expect(screen.getByRole("group", { name: "Desktop session controls" })).toHaveClass(
      "hidden",
      "md:flex",
    );
    expect(document.querySelector("#mobile-header-controls")).not.toBeInTheDocument();

    fireEvent.click(more);

    expect(more).toHaveAttribute("aria-expanded", "true");
    const controls = document.querySelector("#mobile-header-controls");
    expect(controls).toBeInTheDocument();
    expect(controls).toHaveClass("md:hidden");
    fireEvent.click(within(controls as HTMLElement).getByRole("button", { name: "Providers" }));

    expect(onToggleProviders).toHaveBeenCalledOnce();
    expect(document.querySelector("#mobile-header-controls")).not.toBeInTheDocument();
  });

  it("carries the plan's progress at every width, the phone's only view of it", () => {
    render(
      <Header
        conversationCount={1}
        title=""
        model=""
        tokens={0}
        ctx={0}
        clientCount={1}
        connected
        navOpen={false}
        railOpen={false}
        planDone={2}
        planTotal={6}
        onToggleConversations={vi.fn()}
        onToggleFiles={vi.fn()}
        onToggleProviders={vi.fn()}
      />,
    );

    expect(screen.getByLabelText("Plan 2 of 6 done")).toHaveTextContent("2/6");
  });

  it("shows no plan badge when there is no plan", () => {
    renderHeader();
    expect(screen.queryByText("0/0")).not.toBeInTheDocument();
  });

  it("keeps both rails one tap away at every width", () => {
    const { onToggleFiles } = renderHeader();

    const session = screen.getByRole("button", { name: "Session" });
    expect(session).not.toHaveClass("md:flex");
    fireEvent.click(session);

    expect(onToggleFiles).toHaveBeenCalledOnce();
    expect(session).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByRole("button", { name: "Conversations" })).toHaveAttribute(
      "aria-expanded",
      "false",
    );
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
        navOpen={false}
        railOpen={false}
        onToggleConversations={onToggleConversations}
        onToggleFiles={vi.fn()}
        onToggleProviders={vi.fn()}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Conversations" }));
    expect(onToggleConversations).toHaveBeenCalledOnce();
  });
});
