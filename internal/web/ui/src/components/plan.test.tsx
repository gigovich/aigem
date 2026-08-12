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

  it("counts an empty plan rather than rendering a bar with no width", () => {
    render(<Plan todos={[]} />);

    // A count, not a proportion: there is no ratio to compute and so nothing to
    // divide by zero, which is what the bar this replaced had to guard against.
    expect(screen.getByText("0/0")).toBeInTheDocument();
    expect(screen.queryAllByRole("listitem")).toHaveLength(0);
  });

  it("says a step is done without relying on the colour to say it", () => {
    render(<Plan todos={todos} />);

    const done = screen.getByText("Read the config");
    expect(done).toHaveClass("line-through");
    expect(screen.getByLabelText("Done")).toBeInTheDocument();
  });
});
