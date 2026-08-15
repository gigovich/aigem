import { useEffect, useId, useRef, useState } from "react";
import { MoreHorizontal, Trash2 } from "lucide-react";
import { Button } from "@/components/ui";

export function DeleteThreadControl({
  title,
  onDelete,
}: {
  title: string;
  onDelete: () => Promise<void>;
}) {
  const [menu, setMenu] = useState(false);
  const [dialog, setDialog] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const opener = useRef<HTMLButtonElement>(null);
  const modal = useRef<HTMLDivElement>(null);
  const cancel = useRef<HTMLButtonElement>(null);
  const pendingRef = useRef(pending);
  const remove = useRef(onDelete);
  const heading = useId();
  const consequence = useId();

  useEffect(() => {
    pendingRef.current = pending;
    remove.current = onDelete;
  }, [pending, onDelete]);

  useEffect(() => {
    if (!dialog) return;
    const openerElement = opener.current;
    cancel.current?.focus();

    const onKey = (event: KeyboardEvent) => {
      const root = modal.current;
      if (!root) return;
      if (event.key === "Escape") {
        if (!pendingRef.current) setDialog(false);
        return;
      }
      if (event.key !== "Tab") return;

      const focusable = Array.from(
        root.querySelectorAll<HTMLElement>(
          'button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
        ),
      );
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      const active = document.activeElement;
      if (!first || !last) {
        event.preventDefault();
        root.focus();
      } else if (event.shiftKey && (active === first || active === root || !root.contains(active))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (active === last || active === root || !root.contains(active))) {
        event.preventDefault();
        first.focus();
      }
    };

    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      openerElement?.focus();
    };
  }, [dialog]);

  const confirm = async () => {
    if (pendingRef.current) return;
    pendingRef.current = true;
    setPending(true);
    setError(null);
    try {
      await remove.current();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
      pendingRef.current = false;
      setPending(false);
    }
  };

  return (
    <div className="relative ml-auto shrink-0">
      <Button
        ref={opener}
        variant="ghost"
        size="icon"
        aria-label="Thread actions"
        aria-haspopup="menu"
        aria-expanded={menu}
        onClick={() => setMenu((open) => !open)}
      >
        <MoreHorizontal className="h-4 w-4" />
      </Button>

      {menu && (
        <div
          role="menu"
          aria-label="Thread actions"
          className="absolute top-full right-0 z-20 mt-1 min-w-40 rounded-md border border-line bg-panel p-1 shadow-lg"
        >
          <button
            type="button"
            role="menuitem"
            className="flex h-9 w-full items-center gap-2 rounded-sm px-2 text-left text-[13px] text-bad hover:bg-bad/12 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60"
            onClick={() => {
              setMenu(false);
              setError(null);
              setDialog(true);
            }}
          >
            <Trash2 className="h-4 w-4" aria-hidden />
            Delete thread
          </button>
        </div>
      )}

      {dialog && (
        <>
          <div aria-hidden className="fixed inset-0 z-40 bg-canvas/70" />
          <div
            ref={modal}
            tabIndex={-1}
            role="dialog"
            aria-modal="true"
            aria-labelledby={heading}
            aria-describedby={consequence}
            className="fixed top-1/2 left-1/2 z-50 flex w-[calc(100%-2rem)] max-w-md -translate-x-1/2 -translate-y-1/2 flex-col gap-4 rounded-lg border border-line bg-panel p-5 shadow-xl outline-none"
          >
            <div className="min-w-0">
              <h2 id={heading} className="text-[17px] font-medium">
                Delete thread?
              </h2>
              <p className="mt-2 break-words text-[14px] font-medium">{title || "untitled"}</p>
              <p id={consequence} className="mt-2 text-[13px] text-muted">
                Messages, run history, and related files will be permanently deleted. This can’t be undone.
              </p>
            </div>
            {error && (
              <p role="alert" className="break-words font-mono text-[12px] text-bad">
                {error}
              </p>
            )}
            <div className="flex flex-wrap justify-end gap-2">
              <Button
                ref={cancel}
                variant="outline"
                disabled={pending}
                onClick={() => setDialog(false)}
              >
                Cancel
              </Button>
              <Button variant="danger" disabled={pending} onClick={() => void confirm()}>
                {pending ? "Deleting…" : "Delete thread"}
              </Button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}
