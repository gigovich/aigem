import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Item } from "@/lib/session";
import { Timeline } from "./timeline";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

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

const blobURL = (seq: number) => `/api/sessions/s/blobs/${seq}`;

describe("Timeline", () => {
  it("leaves the plan to the rail instead of repeating it as tool cards", () => {
    render(<Timeline items={items} blobURL={blobURL} />);

    expect(screen.queryByText("todo_write")).not.toBeInTheDocument();
    expect(screen.getByText("bash")).toBeInTheDocument();
  });

  it("keeps a plan write that failed, which the rail cannot show", async () => {
    render(<Timeline items={[...items, failedPlan]} blobURL={blobURL} />);

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
        blobURL={blobURL}
      />,
    );

    // The word, not only the colour. The output that explains it is one click
    // away rather than crowding the row.
    expect(screen.getByText("failed")).toBeInTheDocument();
    expect(screen.queryByText("exit status 1")).not.toBeInTheDocument();
    expect(screen.queryByText("stdout line")).not.toBeInTheDocument();
  });

  it("closes a call down to one line: the name, one argument and the outcome", () => {
    render(<Timeline items={items} blobURL={blobURL} />);

    expect(screen.getByText("bash")).toBeInTheDocument();
    expect(screen.getByText("grep -R main cmd/")).toBeInTheDocument();
    expect(screen.getByLabelText("Succeeded")).toBeInTheDocument();
    // No result, first line or otherwise, until it is opened.
    expect(screen.queryByText(/cmd\/aigem\/main\.go/)).not.toBeInTheDocument();
    expect(screen.queryByText(/second line/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /bash/ })).toHaveAttribute("aria-expanded", "false");
  });

  it("shows the result only once the row is opened", async () => {
    render(<Timeline items={items} blobURL={blobURL} />);

    fireEvent.click(screen.getByRole("button", { name: /bash/ }));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /bash/ })).toHaveAttribute("aria-expanded", "true");
    });
    expect(screen.getByText(/cmd\/aigem\/main\.go:146/)).toBeInTheDocument();
  });

  // The caller decides where the rest of a large result lives, which is what
  // lets one timeline draw both a session's stream and a bot thread's.
  it("fetches the tail of a large result from the URL the caller built", async () => {
    const asked: string[] = [];
    const fetchMock = vi.fn(async (url: string) => {
      asked.push(url);
      return new Response("the whole thing");
    });
    vi.stubGlobal("fetch", fetchMock);

    render(
      <Timeline
        items={[{
          kind: "tool", seq: 12, id: "12", name: "bash", done: true,
          args: { cmd: "go test ./..." }, result: "head only", blob: true, blobSeq: 12,
        }]}
        blobURL={(seq) => `/api/chat/threads/t_1/blobs/${seq}`}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /bash/ }));

    await waitFor(() => {
      expect(screen.getByText("the whole thing")).toBeInTheDocument();
    });
    expect(asked).toEqual(["/api/chat/threads/t_1/blobs/12"]);
  });
});
