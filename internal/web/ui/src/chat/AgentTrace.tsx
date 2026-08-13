import { memo, useMemo } from "react";
import { ChevronRight } from "lucide-react";
import { Timeline } from "@/components/timeline";
import { Button, RunDot, SkeletonRows } from "@/components/ui";
import { summaryOf, traceItems, type TurnTrace } from "@/lib/trace";
import type { Turn } from "@/lib/chatprotocol";
import { cn } from "@/lib/utils";

/** plural writes a count with its noun, and nothing when there is none. A line
 *  reading "0 tools" spends a column saying that nothing happened. */
function plural(n: number, one: string, many = one + "s"): string | null {
  if (n <= 0) return null;
  return `${n} ${n === 1 ? one : many}`;
}

interface AgentTraceProps {
  turn: Turn;
  held: TurnTrace | undefined;
  open: boolean;
  blobURL: (seq: number) => string;
  /** Both take the run rather than closing over it, so the caller can pass one
   *  stable function for every trace on screen. An inline arrow per message
   *  would defeat the memo below on every frame. */
  onToggle: (turn: number) => void;
  onMore: (turn: number) => void;
}

/** What a bot did to produce the message under it: one line closed, the whole
 *  timeline open.
 *
 *  This summary is the point of the migration. A chat product could show the
 *  answer and nothing else, so a bot that spent four minutes and fourteen tool
 *  calls getting it looked exactly like one that guessed.
 *
 *  It is drawn from the turn's own row rather than from its events, which is
 *  why it costs nothing to show on every message in a thread: the daemon counts
 *  the steps as it records them. A run being watched can be further along than
 *  its row - the row is re-read only at a turn boundary - so summaryOf takes
 *  whichever is larger. */
function Trace({ turn, held, open, blobURL, onToggle, onMore }: AgentTraceProps) {
  const running = !turn.ended;
  // Memoised on the two things it reads. A thread mid-run re-renders on every
  // frame, and this walks the whole event list of the run being watched.
  const { steps, tools, files } = useMemo(() => summaryOf(turn, held), [turn, held]);
  // The fold is a pass over the whole turn, so it is paid for only while the
  // trace is on screen. Everything else here reads counters.
  const items = useMemo(
    () => (open && held ? traceItems(held.events) : []),
    [open, held],
  );

  const parts = [
    plural(steps, "step"),
    plural(tools, "tool"),
    plural(files, "file"),
  ].filter(Boolean);
  // A turn that recorded nothing has nothing to disclose, and a control that
  // opens an empty panel is worse than no control. A running one always shows,
  // because "working" is the fact.
  if (parts.length === 0 && !running) return null;

  return (
    <div className="mt-1">
      <button
        onClick={() => onToggle(turn.seq)}
        aria-expanded={open}
        className={cn(
          "flex h-8 items-center gap-1.5 rounded-md pr-2 text-left text-[12px]",
          "[@media(pointer:coarse)]:h-11",
          "text-muted transition-colors duration-[120ms] ease-out hover:bg-raised hover:text-fg",
          // A raw button, so it does not inherit the ring `Button` carries, and
          // DESIGN.md states it without exception.
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60",
        )}
      >
        <ChevronRight
          aria-hidden
          className={cn(
            "h-3.5 w-3.5 shrink-0 transition-transform duration-[120ms]",
            open && "rotate-90",
          )}
        />
        <span className="font-mono">{parts.join(" · ") || "working"}</span>
        {running && <RunDot className="ml-1" label="This turn is still running" />}
        {/* Monospaced and truncating: this is a provider or daemon error string
            verbatim, and an unbroken URL in one would scroll the transcript
            sideways. The whole of it is in the trace when it is opened. */}
        {turn.error && (
          <span className="ml-1 min-w-0 truncate font-mono text-bad">{turn.error}</span>
        )}
      </button>

      {open && (
        // No card. The permitted nested panel is the tool call, and Timeline
        // already draws each one as `border border-line bg-panel` - a panel
        // around the whole trace would put those on the same fill they sit on,
        // erasing the one containment relationship elevation is reserved for.
        // The timeline carries its own spine, so indentation is all this needs.
        <div className="mt-1">
          {/* At the shape of what arrives: Timeline is px-4 py-4 with a gap-3
              stack of label-plus-paragraph blocks, not a list of 32px rows.
              Guarded on the events and not on `length === 0`, because a trace
              opened for the first time holds no entry at all - and `undefined
              === 0` is false, which left the reader looking at an empty box for
              the length of the fetch. */}
          {!held?.loaded && !held?.events.length && (
            <SkeletonRows rows={3} rowClass="h-14" className="gap-3 px-4 py-4" />
          )}
          {/* A run outlives its own timeline: turn rows are never pruned and
              events age out at thirty days, so an old run keeps saying "14
              steps" over a stream that no longer exists. Drawing an empty spine
              for that is the same broken promise the collapsed line refuses to
              make for a run that recorded nothing. */}
          {held?.loaded && items.length === 0 && (
            <p className="px-4 py-3 text-[13px] text-muted">
              The steps of this run have aged out; only what it did is still counted.
            </p>
          )}
          <Timeline items={items} blobURL={blobURL} />
          {/* Below the timeline, and named for the direction it actually goes.
              The daemon pages a turn forwards - `WHERE seq > ?` from the oldest
              - so the next page is the rest of the run and it lands at the
              bottom. A button labelled "earlier" above the list would promise
              the opposite of both. */}
          {held?.more && (
            <div className="px-4 pb-3">
              <Button variant="ghost" size="sm" onClick={() => onMore(turn.seq)}>
                More steps
              </Button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

/** Memoised, because the thread pane re-renders on every timeline frame and a
 *  thread on screen holds one of these per bot message. Only the run a frame
 *  belongs to changes identity: `held` comes from a per-turn record, and the
 *  callbacks are stable. */
export const AgentTrace = memo(Trace);
