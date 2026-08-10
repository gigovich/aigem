import { useEffect, useRef, type ReactNode } from "react";
import { X } from "lucide-react";
import { Button } from "./ui";
import { cn } from "@/lib/utils";

interface SidePanelProps {
  side: "left" | "right";
  open: boolean;
  /** true below the breakpoint, where the panel covers the page rather than
   *  standing beside it - which is what makes it a dialog and Escape a way out. */
  modal: boolean;
  title: string;
  onDismiss: () => void;
  children: ReactNode;
}

/** A standing column on a wide screen and a dismissable drawer on a narrow one,
 *  from one piece of state: a rail that only exists at one width is a rail the
 *  phone never gets. */
export function SidePanel({ side, open, modal, title, onDismiss, children }: SidePanelProps) {
  const panel = useRef<HTMLElement>(null);
  // Held in a ref: keyed off the callback's identity, this effect re-ran on
  // every render of the app and dragged focus back to the drawer each time, so
  // the list inside it could not be tabbed through.
  const dismiss = useRef(onDismiss);
  useEffect(() => {
    dismiss.current = onDismiss;
  }, [onDismiss]);

  useEffect(() => {
    if (!open || !modal) return;
    // aria-modal hides the rest of the page from assistive tech, so leaving the
    // caret on the button that opened the drawer parks it somewhere the reader
    // can no longer be told about. Handed back on close for the same reason.
    const opener = document.activeElement as HTMLElement | null;
    panel.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        dismiss.current();
        return;
      }
      if (e.key !== "Tab" || !panel.current) return;

      const root = panel.current;
      const focusable = Array.from(root.querySelectorAll<HTMLElement>(
        'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [contenteditable="true"], [tabindex]:not([tabindex="-1"])',
      )).filter((element) => !element.closest('[hidden], [aria-hidden="true"]'));
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      const active = document.activeElement;

      if (!first || !last) {
        e.preventDefault();
        root.focus();
      } else if (e.shiftKey && (active === root || active === first || !root.contains(active))) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && (active === root || active === last || !root.contains(active))) {
        e.preventDefault();
        first.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      opener?.focus?.();
    };
  }, [open, modal]);

  if (!open) return null;
  return (
    <>
      <div onClick={onDismiss} aria-hidden className="fixed inset-0 z-30 bg-black/60 lg:hidden" />
      <aside
        ref={panel}
        tabIndex={-1}
        aria-label={title}
        role={modal ? "dialog" : undefined}
        aria-modal={modal ? true : undefined}
        className={cn(
          "z-40 flex w-72 max-w-[85vw] shrink-0 flex-col bg-panel outline-none",
          "fixed inset-y-0 pb-[env(safe-area-inset-bottom)] shadow-xl lg:static lg:pb-0 lg:shadow-none",
          side === "left" ? "left-0 border-r border-border" : "right-0 border-l border-border",
        )}
      >
        <div className="flex shrink-0 items-center border-b border-border px-3 py-2 lg:hidden">
          <span className="text-[13px] font-medium">{title}</span>
          <Button
            variant="ghost"
            size="icon"
            className="ml-auto"
            aria-label={`Close ${title}`}
            onClick={onDismiss}
          >
            <X className="h-4 w-4" />
          </Button>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto">{children}</div>
      </aside>
    </>
  );
}
