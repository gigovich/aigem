import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DeleteThreadControl } from "./DeleteThreadDialog";

afterEach(cleanup);

function openDialog(onDelete = vi.fn(() => Promise.resolve())) {
  render(<DeleteThreadControl title="A thread to remove" onDelete={onDelete} />);
  const opener = screen.getByRole("button", { name: "Thread actions" });
  fireEvent.click(opener);
  fireEvent.click(screen.getByRole("menuitem", { name: "Delete thread" }));
  return opener;
}

describe("DeleteThreadControl", () => {
  it("names the thread and focuses the safe action", () => {
    openDialog();

    expect(screen.getByRole("dialog", { name: "Delete thread?" })).toBeInTheDocument();
    expect(screen.getByText("A thread to remove")).toBeInTheDocument();
    expect(screen.getByText(/permanently deleted/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus();
  });

  it("cancels with Escape and returns focus without deleting", () => {
    const onDelete = vi.fn(() => Promise.resolve());
    const opener = openDialog(onDelete);

    fireEvent.keyDown(window, { key: "Escape" });

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(onDelete).not.toHaveBeenCalled();
    expect(opener).toHaveFocus();
  });

  it("traps keyboard focus inside the dialog", () => {
    openDialog();
    const cancel = screen.getByRole("button", { name: "Cancel" });
    const confirm = screen.getByRole("button", { name: "Delete thread" });

    confirm.focus();
    fireEvent.keyDown(window, { key: "Tab" });
    expect(cancel).toHaveFocus();

    cancel.focus();
    fireEvent.keyDown(window, { key: "Tab", shiftKey: true });
    expect(confirm).toHaveFocus();
  });

  it("submits once and locks the dialog while the request is pending", () => {
    let resolve!: () => void;
    const onDelete = vi.fn(
      () =>
        new Promise<void>((done) => {
          resolve = done;
        }),
    );
    openDialog(onDelete);
    const confirm = screen.getByRole("button", { name: "Delete thread" });

    fireEvent.click(confirm);
    fireEvent.click(confirm);

    expect(onDelete).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "Deleting…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    fireEvent.keyDown(window, { key: "Escape" });
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    resolve();
  });

  it("keeps a failure in the dialog and retries only on another click", async () => {
    const onDelete = vi
      .fn<() => Promise<void>>()
      .mockRejectedValueOnce(new Error("500 Internal Server Error"))
      .mockResolvedValueOnce();
    openDialog(onDelete);

    fireEvent.click(screen.getByRole("button", { name: "Delete thread" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("500 Internal Server Error");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(onDelete).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "Delete thread" }));
    await waitFor(() => expect(onDelete).toHaveBeenCalledTimes(2));
  });
});
