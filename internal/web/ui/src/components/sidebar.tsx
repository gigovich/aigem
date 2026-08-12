import { Plus, X } from "lucide-react";
import type { SessionView } from "@/lib/protocol";
import { Button, RunDot } from "./ui";
import { cn } from "@/lib/utils";

interface SidebarProps {
  list: SessionView[];
  activeID: string | null;
  onSelect: (id: string) => void;
  onCreate: () => void;
  onCloseConversation: (id: string) => void;
}

/** The conversation list as a standing column. As a drop-down it pushed the
 *  timeline down, closed on every pick, and never showed which conversation was
 *  running while another was being read. */
export function Sidebar({ list, activeID, onSelect, onCreate, onCloseConversation }: SidebarProps) {
  return (
    <nav aria-label="Conversations" className="flex min-h-0 flex-1 flex-col">
      <div className="shrink-0 p-2">
        <Button variant="outline" size="sm" className="w-full justify-start" onClick={onCreate}>
          <Plus className="h-3.5 w-3.5" /> New conversation
        </Button>
      </div>
      {/* Hairline rows rather than cards: at this density a list of cards is a
          list of borders, and the thing being scanned is the labels. */}
      <ul className="min-h-0 flex-1 overflow-y-auto border-t border-line-faint">
        {list.map((s) => {
          const active = s.id === activeID;
          const label = s.title || "new conversation";
          return (
            <li key={s.id} className="group/row relative border-b border-line-faint">
              <button
                onClick={() => onSelect(s.id)}
                aria-current={active ? "true" : undefined}
                className={cn(
                  // The row grows with the close button that sits in it: at 36px
                  // a 44px target would overhang into the neighbouring row, and
                  // closing a conversation is not undoable.
                  "flex h-9 w-full items-center gap-2 pr-9 pl-0 text-left text-[13px]",
                  "[@media(pointer:coarse)]:h-11 [@media(pointer:coarse)]:pr-11",
                  "transition-colors duration-[120ms] ease-out",
                  active ? "bg-raised text-fg" : "text-muted hover:bg-raised hover:text-fg",
                )}
              >
                <span
                  aria-hidden
                  className={cn(
                    "h-full w-0.5 shrink-0",
                    active ? "bg-accent" : "bg-transparent",
                  )}
                />
                <span className="truncate pl-1.5">{label}</span>
              </button>
              {/* Outside the button: inside it, the dot's label joined the row's
                  accessible name, so every running conversation was announced
                  under a different name than the one it is listed by. */}
              {s.running && (
                <RunDot
                  className="pointer-events-none absolute top-1/2 right-9 -translate-y-1/2 [@media(pointer:coarse)]:right-11"
                  label={`${label} is running`}
                />
              )}
              <Button
                variant="ghost"
                size="icon"
                title="Close conversation"
                aria-label={`Close ${label}`}
                // Revealed by hover only where hover exists. Keyed off the
                // breakpoint, a touch laptop got an invisible button that still
                // took the tap - and closing a conversation is not undoable.
                className="absolute top-1/2 right-0 -translate-y-1/2 opacity-100 focus-visible:opacity-100 [@media(hover:hover)]:opacity-0 [@media(hover:hover)]:group-hover/row:opacity-100"
                onClick={() => onCloseConversation(s.id)}
              >
                <X className="h-3.5 w-3.5" />
              </Button>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}
