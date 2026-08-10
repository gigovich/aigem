import { cleanup, render, screen } from "@testing-library/react";
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

  it("keeps a plan write that failed, which the rail cannot show", () => {
    render(<Timeline items={[...items, failedPlan]} sessionID="s" />);

    expect(screen.getByText("todo_write")).toBeInTheDocument();
    expect(screen.getByText("invalid todos: text is required")).toBeInTheDocument();
  });

  it("prefers the error over the output in the preview", () => {
    render(
      <Timeline
        items={[{
          kind: "tool", seq: 9, id: "9", name: "bash", done: true,
          args: { cmd: "false" }, result: "stdout line", error: "exit status 1",
        }]}
        sessionID="s"
      />,
    );

    expect(screen.getByText("exit status 1")).toBeInTheDocument();
    expect(screen.queryByText("stdout line")).not.toBeInTheDocument();
  });

  it("shows no preview line for a result that is only whitespace", () => {
    const { container } = render(
      <Timeline
        items={[{
          kind: "tool", seq: 10, id: "10", name: "bash", done: true,
          args: { cmd: "true" }, result: "   \n  ",
        }]}
        sessionID="s"
      />,
    );

    expect(container.querySelectorAll("button > span")).toHaveLength(1);
  });

  it("previews the first line of a result on the closed card", () => {
    render(<Timeline items={items} sessionID="s" />);

    expect(screen.getByText("cmd/aigem/main.go:146")).toBeInTheDocument();
    expect(screen.queryByText(/second line/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /bash/ })).toHaveAttribute("aria-expanded", "false");
  });
});
