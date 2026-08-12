import { useEffect, useRef, type ReactNode } from "react";
import { X } from "lucide-react";
import { Button } from "./ui";
import { cn } from "@/lib/utils";

/** Where a panel sits at the current width. Docked is a standing column; the
 *  other two cover the page, which is what makes them dialogs and Escape a way
 *  out. Passed in rather than expressed as CSS breakpoints so the layout and the
 *  focus trap cannot disagree about where the line is. */
export type PanelLayout = "docked" | "drawer" | "sheet";

interface SidePanelProps {
  side: "left" | "right";
  open: boolean;
  layout: PanelLayout;
  title: string;
  onDismiss: () => void;
  children: ReactNode;
}

/** A standing column on a wide screen, a drawer on a medium one and a bottom
 *  sheet on a phone, from one piece of state: a rail that only exists at one
 *  width is a rail the phone never gets. */
export function SidePanel({ side, open, layout, title, onDismiss, children }: SidePanelProps) {
  const panel = useRef<HTMLElement>(null);
  const modal = layout !== "docked";
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
      {modal && (
        <div onClick={onDismiss} aria-hidden className="fixed inset-0 z-30 bg-canvas/70" />
      )}
      <aside
        ref={panel}
        tabIndex={-1}
        aria-label={title}
        role={modal ? "dialog" : undefined}
        aria-modal={modal ? true : undefined}
        className={cn(
          "z-40 flex min-h-0 shrink-0 flex-col bg-panel outline-none",
          layout === "docked" && "h-full",
          layout === "docked" && (side === "left" ? "w-[260px]" : "w-[420px]"),
          layout === "drawer" &&
            "fixed inset-y-0 w-[320px] max-w-[85vw] pb-[env(safe-area-inset-bottom)]",
          layout === "drawer" && (side === "left" ? "left-0" : "right-0"),
          // A sheet rises from the edge the thumb is already at. Capped so the
          // conversation it was opened from stays visible above it.
          layout === "sheet" &&
            "fixed inset-x-0 bottom-0 max-h-[75dvh] rounded-t-lg border-t border-line " +
              "pb-[env(safe-area-inset-bottom)]",
          layout !== "sheet" && (side === "left" ? "border-r border-line" : "border-l border-line"),
        )}
      >
        {modal && (
          <div className="flex shrink-0 items-center border-b border-line px-3 py-2">
            <span className="text-[15px] font-medium">{title}</span>
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
        )}
        <div className="min-h-0 flex-1 overflow-y-auto">{children}</div>
      </aside>
    </>
  );
}
