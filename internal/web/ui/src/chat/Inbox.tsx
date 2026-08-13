import { useState } from "react";
import { ChevronRight, Plus } from "lucide-react";
import type { ChatState } from "@/lib/chat";
import { countsOf, inboxOf } from "@/lib/chat";
import { stateLabel, type Actor, type ThreadState } from "@/lib/chatprotocol";
import { Button, SkeletonRows } from "@/components/ui";
import { cn } from "@/lib/utils";
import { ThreadRow } from "./ThreadRow";
import { NewThread } from "./NewThread";

interface InboxProps {
  state: ChatState;
  fleet: Actor[];
  activeID: string | null;
  maxUnread: number;
  /** The states this daemon can put on a thread, in the order it lists them,
   *  which is the order the chips are drawn in. Taken from the daemon so a
   *  state added there is not invisible here until the bundle is rebuilt. */
  states: ThreadState[];
  /** The operator's actor id, so "you" is decided by the daemon rather than by
   *  a string this file happened to copy. */
  operator: string;
  loaded: boolean;
  onSelect: (id: string) => void;
  onCreate: (title: string, participants: string[], text: string) => Promise<void>;
  /** Fetches the archived threads. They are not in the inbox response, so the
   *  "done" list is empty until this has run once. */
  onLoadDone: () => Promise<unknown>;
  maxTitleChars: number;
}

/** The agent inbox. Bots work unattended, so the question on opening the tab is
 *  "what requires me", not "what happened" - which is why this is sorted by
 *  state and not by a folder anyone has to maintain. */
export function Inbox({
  state,
  fleet,
  activeID,
  maxUnread,
  states,
  operator,
  loaded,
  onSelect,
  onCreate,
  onLoadDone,
  maxTitleChars,
}: InboxProps) {
  const [filter, setFilter] = useState<ThreadState | null>(null);
  const [composing, setComposing] = useState(false);
  const [showDone, setShowDone] = useState(false);
  const counts = countsOf(state);
  const rows = inboxOf(state, filter ?? undefined);
  const done = inboxOf(state, undefined, true);

  return (
    <nav aria-label="Threads" className="flex min-h-0 flex-1 flex-col">
      <div className="shrink-0 px-2 pt-2">
        <Button
          variant="outline"
          size="sm"
          className="w-full justify-start"
          onClick={() => setComposing((open) => !open)}
          aria-expanded={composing}
        >
          <Plus className="h-3.5 w-3.5" /> New thread
        </Button>
      </div>

      {composing && (
        <NewThread
          fleet={fleet}
          maxTitleChars={maxTitleChars}
          onCancel={() => setComposing(false)}
          onCreate={async (title, participants, text) => {
            await onCreate(title, participants, text);
            setComposing(false);
          }}
        />
      )}

      {/* Ghost toggles, not tabs: these narrow one list rather than switching
          between several, and a tab strip would promise a different screen. */}
      <div role="group" aria-label="Filter threads" className="flex flex-wrap gap-1 px-2 py-2">
        {states.map((f) => (
          <button
            key={f}
            aria-pressed={filter === f}
            onClick={() => setFilter((current) => (current === f ? null : f))}
            className={cn(
              "inline-flex items-center gap-1 rounded-sm border px-1.5 py-0.5 text-[11px] font-medium",
              "transition-colors duration-[120ms] ease-out",
              "[@media(pointer:coarse)]:min-h-11 [@media(pointer:coarse)]:px-2",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60",
              filter === f
                ? "border-line bg-raised text-fg"
                : "border-transparent text-muted hover:bg-raised hover:text-fg",
            )}
          >
            {stateLabel(f)}
            {counts[f] > 0 && <span className="font-mono">{counts[f]}</span>}
          </button>
        ))}
      </div>

      {/* The skeleton and the empty state live inside the scroll container, in
          the space the rows will occupy. Outside it they sit against the bottom
          edge of the rail, and the layout jumps its whole height when the
          answer lands - which is the one thing a skeleton exists to prevent. */}
      <div className="min-h-0 flex-1 overflow-y-auto border-t border-line-faint">
        <ul>
          {rows.map((thread) => (
            <ThreadRow
              key={thread.id}
              thread={thread}
              active={thread.id === activeID}
              maxUnread={maxUnread}
              operator={operator}
              onSelect={() => onSelect(thread.id)}
            />
          ))}
        </ul>

        {!loaded && (
          <SkeletonRows rows={6} rowClass="h-9 [@media(pointer:coarse)]:h-11" className="gap-0" />
        )}
        {loaded && rows.length === 0 && (
          <p className="px-3 py-4 text-[13px] text-muted">
            {filter
              ? `Nothing is ${stateLabel(filter)}.`
              : "No threads yet. A thread is one task, with the bots working on it in it."}
          </p>
        )}

        {/* Archived threads, below the rule and folded away. A disclosure
            rather than a second list: they are the ones deliberately out of the
            way, and a rail with two lists in it has no primary one. */}
        {loaded && !filter && (
          <div className="border-t border-line-faint">
            <button
              aria-expanded={showDone}
              onClick={() => {
                const next = !showDone;
                setShowDone(next);
                if (next) void onLoadDone();
              }}
              className={cn(
                "flex h-9 w-full items-center gap-1.5 px-3 text-left text-[12px] text-muted",
                "[@media(pointer:coarse)]:h-11",
                "transition-colors duration-[120ms] ease-out hover:bg-raised hover:text-fg",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60",
              )}
            >
              <ChevronRight
                aria-hidden
                className={cn("h-3.5 w-3.5 transition-transform", showDone && "rotate-90")}
              />
              done
              {done.length > 0 && <span className="font-mono">{done.length}</span>}
            </button>
            {showDone && (
              <ul>
                {done.map((thread) => (
                  <ThreadRow
                    key={thread.id}
                    thread={thread}
                    active={thread.id === activeID}
                    maxUnread={maxUnread}
                    operator={operator}
                    onSelect={() => onSelect(thread.id)}
                  />
                ))}
              </ul>
            )}
            {showDone && done.length === 0 && (
              <p className="px-3 pb-3 text-[13px] text-muted">Nothing is archived.</p>
            )}
          </div>
        )}
      </div>
    </nav>
  );
}
