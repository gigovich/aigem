import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { Item } from "@/lib/session";
import { Timeline } from "./timeline";

afterEach(cleanup);

const items: Item[] = [
  {
    kind: "tool", seq: 1, id: "1", name: "todo_write", done: true,
    args: { todos: [{ text: "Read the config", status: "completed" }] },
  },
  {
    kind: "tool", seq: 2, id: "2", name: "bash", done: true,
    args: { cmd: "grep -R main cmd/" }, result: "cmd/aigem/main.go:146\nsecond line",
  },
];

const failedPlan: Item = {
  kind: "tool", seq: 3, id: "3", name: "todo_write", done: true,
  args: { todos: [] }, error: "invalid todos: text is required",
};

describe("Timeline", () => {
  it("leaves the plan to the rail instead of repeating it as tool cards", () => {
    render(<Timeline items={items} sessionID="s" />);

    expect(screen.queryByText("todo_write")).not.toBeInTheDocument();
    expect(screen.getByText("bash")).toBeInTheDocument();
  });

  it("keeps a plan write that failed, which the rail cannot show", async () => {
    render(<Timeline items={[...items, failedPlan]} sessionID="s" />);

    expect(screen.getByText("todo_write")).toBeInTheDocument();
    expect(screen.getByText("failed")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /todo_write/ }));

    await waitFor(() => {
      expect(screen.getByText("invalid todos: text is required")).toBeInTheDocument();
    });
  });

  it("says a call failed on the closed row, without its output", () => {
    render(
      <Timeline
        items={[{
          kind: "tool", seq: 9, id: "9", name: "bash", done: true,
          args: { cmd: "false" }, result: "stdout line", error: "exit status 1",
        }]}
        sessionID="s"
      />,
    );

    // The word, not only the colour. The output that explains it is one click
    // away rather than crowding the row.
    expect(screen.getByText("failed")).toBeInTheDocument();
    expect(screen.queryByText("exit status 1")).not.toBeInTheDocument();
    expect(screen.queryByText("stdout line")).not.toBeInTheDocument();
  });

  it("closes a call down to one line: the name, one argument and the outcome", () => {
    render(<Timeline items={items} sessionID="s" />);

    expect(screen.getByText("bash")).toBeInTheDocument();
    expect(screen.getByText("grep -R main cmd/")).toBeInTheDocument();
    expect(screen.getByLabelText("Succeeded")).toBeInTheDocument();
    // No result, first line or otherwise, until it is opened.
    expect(screen.queryByText(/cmd\/aigem\/main\.go/)).not.toBeInTheDocument();
    expect(screen.queryByText(/second line/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /bash/ })).toHaveAttribute("aria-expanded", "false");
  });

  it("shows the result only once the row is opened", async () => {
    render(<Timeline items={items} sessionID="s" />);

    fireEvent.click(screen.getByRole("button", { name: /bash/ }));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /bash/ })).toHaveAttribute("aria-expanded", "true");
    });
    expect(screen.getByText(/cmd\/aigem\/main\.go:146/)).toBeInTheDocument();
  });
});
