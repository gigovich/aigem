import { useEffect, useRef } from "react";
import type { ThreadMessages } from "@/lib/chat";
import { clock, displayName, type Message, type ThreadView as View } from "@/lib/chatprotocol";
import { Markdown } from "@/components/md";
import { Button, RunDot, SkeletonRows } from "@/components/ui";
import { cn } from "@/lib/utils";

/** One thing someone said. Author and time in a label line, body below - not
 *  bubbles and not opposing alignment: a thread here holds several bots as well
 *  as the operator, and two sides cannot say which of five wrote a line. */
function Said({ message, operator }: { message: Message; operator: string }) {
  const mine = message.author === operator;
  if (message.kind === "system") {
    // What the store did rather than what anyone said. It is in the transcript
    // because membership that changed silently cannot be audited.
    return (
      <li className="px-4 py-1 text-[12px] text-muted">
        {message.body} <span className="font-mono">{clock(message.created)}</span>
      </li>
    );
  }
  return (
    <li className="px-4 py-2">
      <div className="flex items-baseline gap-2">
        <span className={cn("text-[13px] font-medium", mine ? "text-muted" : "text-fg")}>
          {displayName(message.author, operator)}
        </span>
        {message.await && (
          // The one thing in this interface allowed to ask for the reader, and
          // it carries a word as well as the colour.
          <span className="text-[11px] font-medium text-accent">needs you</span>
        )}
        <span className="ml-auto font-mono text-[12px] text-muted">{clock(message.created)}</span>
      </div>
      <div className="mt-0.5 max-w-[68ch]">
        <Markdown text={message.body} />
      </div>
    </li>
  );
}

interface ThreadPaneProps {
  thread: View;
  operator: string;
  held: ThreadMessages | undefined;
  onOlder: () => void;
}

/** The thread itself. Stage 6 draws what was said; the agent timeline that
 *  produced it is the next stage, and the summary line it collapses into hangs
 *  under each bot's message. */
export function ThreadPane({ thread, operator, held, onOlder }: ThreadPaneProps) {
  const bottom = useRef<HTMLDivElement>(null);
  const scroller = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);
  // Not defaulted to a fresh array: that identity changes every render, and it
  // is what the follow-the-end effect below keys off.
  const items = held?.items;

  // Follow the end only while the reader is already there. Being yanked away
  // from something being read is worse than missing the newest line.
  useEffect(() => {
    if (pinned.current) bottom.current?.scrollIntoView({ block: "end" });
  }, [items]);

  // A different thread starts at its end, whatever the last one was doing.
  useEffect(() => {
    pinned.current = true;
    bottom.current?.scrollIntoView({ block: "end" });
  }, [thread.id]);

  return (
    <div
      ref={scroller}
      onScroll={() => {
        const el = scroller.current;
        if (!el) return;
        pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
      }}
      // Focusable and named: nothing inside a message is focusable, so without
      // this the transcript cannot be scrolled from the keyboard at all.
      tabIndex={0}
      role="region"
      aria-label="Messages"
      className="min-h-0 flex-1 overflow-y-auto focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:-outline-offset-2"
    >
      {held?.more && (
        <div className="px-4 py-2">
          <Button variant="ghost" size="sm" onClick={onOlder}>
            Older messages
          </Button>
        </div>
      )}
      {!held?.loaded && <SkeletonRows rows={4} rowClass="h-16" className="px-4 py-3" />}
      {held?.loaded && items?.length === 0 && (
        <p className="max-w-[68ch] px-4 py-6 text-[14px] text-muted">
          Nothing has been said here yet. Anything posted wakes every bot in the thread.
        </p>
      )}
      <ul className="divide-y divide-line-faint">
        {items?.map((m) => (
          <Said key={m.seq} message={m} operator={operator} />
        ))}
      </ul>
      {thread.working && (
        <p className="flex items-center gap-2 px-4 py-2 text-[12px] text-muted">
          <RunDot /> working
        </p>
      )}
      <div ref={bottom} />
    </div>
  );
}
