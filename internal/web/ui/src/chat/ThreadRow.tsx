import {
  clock,
  displayName,
  firstLine,
  stateLabel,
  type ThreadView,
} from "@/lib/chatprotocol";
import { RunDot } from "@/components/ui";
import { cn } from "@/lib/utils";

interface ThreadRowProps {
  thread: ThreadView;
  active: boolean;
  maxUnread: number;
  /** The operator's actor id, as the daemon reports it. */
  operator: string;
  onSelect: () => void;
}

/** One inbox row: a 2px marker slot, a truncating label and a right-aligned
 *  metadata cluster, in 36px.
 *
 *  One line, not the two the plan's mockup drew. Two lines of 13px and 12px
 *  text cannot fit in the 32-36px DESIGN.md gives a row, and the row height is
 *  what makes a rail of twenty threads scannable - so the preview line is the
 *  part that goes. The last thing said becomes the label when there is no
 *  title, which is the case it was carrying weight in. */
export function ThreadRow({ thread, active, maxUnread, operator, onSelect }: ThreadRowProps) {
  const needs = thread.state === "needs_you";
  const bots = thread.participants.filter((p) => p !== operator).map((p) => displayName(p, operator));
  const title =
    thread.title || (thread.last_text && firstLine(thread.last_text)) || bots.join(" · ") || "untitled";
  const unread = thread.unread > maxUnread ? `${maxUnread}+` : String(thread.unread);

  return (
    <li className="border-b border-line-faint">
      <button
        onClick={onSelect}
        aria-current={active ? "true" : undefined}
        // The full label, for a title the row had to truncate.
        title={title}
        className={cn(
          "flex h-9 w-full items-center gap-2 pr-2 text-left",
          "[@media(pointer:coarse)]:h-11",
          "transition-colors duration-[120ms] ease-out",
          active ? "bg-raised" : "hover:bg-raised",
        )}
      >
        {/* The marker says two different things and never confuses them: the
            accent means you are the one who has to act, and `fg` means this is
            the row you are reading. Background alone could not carry the
            second - it is the same colour the pointer paints on hover. */}
        <span
          aria-hidden
          data-testid="row-marker"
          className={cn(
            "h-full w-0.5 shrink-0",
            needs ? "bg-accent" : active ? "bg-fg" : "bg-transparent",
          )}
        />
        <span className={cn("truncate text-[13px]", active || needs ? "text-fg" : "text-muted")}>
          {title}
        </span>
        <span className="ml-auto flex shrink-0 items-center gap-1.5">
          {/* Never colour alone: every state carries its word, and the dot is
              the only thing that also moves. */}
          {thread.working && <RunDot />}
          <span className={cn("text-[11px] font-medium", needs ? "text-accent" : "text-muted")}>
            {stateLabel(thread.state)}
          </span>
          {thread.unread > 0 && <span className="font-mono text-[12px] text-fg">{unread}</span>}
          <span className="font-mono text-[12px] text-muted">{clock(thread.last_at)}</span>
        </span>
      </button>
    </li>
  );
}
