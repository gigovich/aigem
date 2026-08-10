import { Plus, X } from "lucide-react";
import type { SessionView } from "@/lib/protocol";
import { Button, Spinner } from "./ui";
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
      <ul className="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
        {list.map((s) => {
          const active = s.id === activeID;
          return (
            <li key={s.id} className="group/row relative">
              <button
                onClick={() => onSelect(s.id)}
                aria-current={active ? "true" : undefined}
                className={cn(
                  "flex w-full items-center gap-2 rounded-md py-2 pr-10 pl-2 text-left text-[13px]",
                  active ? "bg-panel-2 text-fg" : "text-muted hover:bg-panel-2 hover:text-fg",
                )}
              >
                <span
                  className={cn(
                    "h-4 w-0.5 shrink-0 rounded-full",
                    active ? "bg-accent" : "bg-transparent",
                  )}
                />
                <span className="truncate">{s.title || "new conversation"}</span>
                {s.running && <Spinner className="ml-auto shrink-0" />}
              </button>
              <Button
                variant="ghost"
                size="icon"
                title="Close conversation"
                aria-label={`Close ${s.title || "new conversation"}`}
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
