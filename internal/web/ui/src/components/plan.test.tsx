import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Plan, planProgress } from "./plan";

afterEach(cleanup);

const todos = [
  { text: "Read the config", status: "completed" },
  { text: "Rename the flag", status: "in_progress" },
  { text: "Update the docs", status: "pending" },
];

describe("Plan", () => {
  it("counts only completed steps", () => {
    expect(planProgress(todos)).toEqual({ done: 1, total: 3 });
    expect(planProgress([])).toEqual({ done: 0, total: 0 });
  });

  it("names the step being worked on, and only that one", () => {
    render(<Plan todos={todos} />);

    expect(screen.getByText("1/3")).toBeInTheDocument();
    const current = screen.getAllByRole("listitem").filter(
      (li) => li.getAttribute("aria-current") === "step",
    );
    expect(current).toHaveLength(1);
    expect(current[0]).toHaveTextContent("Rename the flag");
  });

  it("survives an empty plan without dividing by zero", () => {
    const { container } = render(<Plan todos={[]} />);

    expect(screen.getByText("0/0")).toBeInTheDocument();
    // The guard being tested is the bar's width, not the counter's text.
    expect(container.querySelector(".bg-accent")).toHaveStyle({ width: "0%" });
  });

  it("fills the bar in proportion to the steps that are done", () => {
    const { container } = render(<Plan todos={todos} />);
    expect(container.querySelector(".bg-accent")).toHaveStyle({
      width: `${(1 / 3) * 100}%`,
    });
  });
});
