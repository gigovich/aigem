import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Actor } from "@/lib/chatprotocol";
import { Participants } from "./Participants";

afterEach(cleanup);

const fleet: Actor[] = [
  { id: "bot:amiran", kind: "bot", name: "amiran", role: "developer", present: true, created: "" },
  { id: "bot:kate", kind: "bot", name: "kate", role: "architect", present: false, created: "" },
  { id: "human:operator", kind: "human", name: "operator", present: false, created: "" },
];

function draw(connected = true, participants = ["human:operator", "bot:amiran"]) {
  const onAdd = vi.fn();
  const onRemove = vi.fn();
  render(
    <Participants
      participants={participants}
      fleet={fleet}
      operator="human:operator"
      connected={connected}
      onAdd={onAdd}
      onRemove={onRemove}
    />,
  );
  return { onAdd, onRemove };
}

describe("Participants", () => {
  it("names the operator as you, and the bots by name", () => {
    draw();
    expect(screen.getByText("you")).toBeInTheDocument();
    expect(screen.getByText("amiran")).toBeInTheDocument();
  });

  it("offers no way for the operator to leave", () => {
    // Adding a participant requires being one, so there is no way back in - and
    // the daemon refuses it too.
    draw();
    expect(screen.queryByRole("button", { name: "Remove you" })).toBeNull();
    expect(screen.getByRole("button", { name: "Remove amiran" })).toBeInTheDocument();
  });

  it("lists only the bots that are not here yet", () => {
    draw();
    fireEvent.click(screen.getByRole("button", { name: /add/ }));
    const list = screen.getByRole("listbox", { name: "Bots not in this thread" });
    expect(list).toHaveTextContent("kate");
    expect(list).not.toHaveTextContent("amiran");
  });

  it("closes the list on Escape", () => {
    draw();
    fireEvent.click(screen.getByRole("button", { name: /add/ }));
    fireEvent.keyDown(window, { key: "Escape" });
    expect(screen.queryByRole("listbox", { name: "Bots not in this thread" })).toBeNull();
  });

  it("disables both operations while the socket is down", () => {
    // Both travel over the socket, and a control that answers a click with
    // silence is worse than one that is visibly unavailable.
    draw(false);
    expect(screen.getByRole("button", { name: "Remove amiran" })).toBeDisabled();
    expect(screen.getByRole("button", { name: /add/ })).toBeDisabled();
  });

  it("adds the bot that was picked, and closes", () => {
    // The callbacks were constructed and never asserted on: replacing either
    // with a no-op left the whole suite green.
    const { onAdd } = draw();
    fireEvent.click(screen.getByRole("button", { name: /add/ }));
    fireEvent.click(screen.getByRole("option", { name: /kate/ }));
    expect(onAdd).toHaveBeenCalledWith("bot:kate");
    expect(screen.queryByRole("listbox")).toBeNull();
  });

  it("hands focus back to the control the list was opened from", () => {
    // Closing unmounts whatever had focus, so without this a keyboard reader is
    // dropped at the top of the document.
    const trigger = () => screen.getByRole("button", { name: /add/ });
    draw();
    fireEvent.click(trigger());
    fireEvent.click(screen.getByRole("option", { name: /kate/ }));
    expect(document.activeElement).toBe(trigger());
  });

  it("removes the bot that was dismissed", () => {
    const { onRemove } = draw();
    fireEvent.click(screen.getByRole("button", { name: "Remove amiran" }));
    expect(onRemove).toHaveBeenCalledWith("bot:amiran");
  });

  it("offers no add button when the whole fleet is already here", () => {
    draw(true, ["human:operator", "bot:amiran", "bot:kate"]);
    expect(screen.queryByRole("button", { name: /add/ })).toBeNull();
  });
});
