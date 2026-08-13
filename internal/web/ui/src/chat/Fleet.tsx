import type { FleetMember, LiveBot } from "@/lib/chatprotocol";
import { Spend } from "@/components/usage";
import { Badge, RunDot, SkeletonRows } from "@/components/ui";

/** The column template, shared by the header and every row so the two cannot
 *  drift apart. */
const COLS = "grid grid-cols-[10rem_9rem_6rem_5rem_7rem_minmax(11rem,1fr)_minmax(9rem,1fr)]";

/** The fleet screen: one row per bot, and the columns an operator would
 *  otherwise read journalctl for.
 *
 *  Every column says where it comes from. What a bot is doing and how much it
 *  is carrying are counted in the store, so they are the same answers the inbox
 *  gives; the model, the heartbeat and the next scheduled job live in the
 *  memory of the process running the bots, and a daemon that runs none reports
 *  "-" for those three rather than a row of confident zeroes.
 *
 *  Seven columns do not fit a phone, and dropping some of them would lose the
 *  operational half of the screen without saying so. The table scrolls inside
 *  its own container instead, which is what this interface already does with
 *  every other piece of content wider than the viewport. */
export function Fleet({ members, loaded }: { members: FleetMember[]; loaded: boolean }) {
  const bots = members.filter((m) => m.kind === "bot");
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="px-4 py-3">
        <h2 className="text-[15px] font-medium">Fleet</h2>
        <p className="mt-1 max-w-[68ch] text-[13px] text-muted">
          Every bot this store knows. Stopped means it is not running - either this daemon could
          not start it and is retrying, or no daemon is running it at all.
        </p>
      </div>

      {!loaded ? (
        // The header bar is drawn now too, and the rows are flush like the real
        // ones: a skeleton whose shape differs from what replaces it moves the
        // layout at exactly the moment the reader starts reading.
        <div>
          <Header />
          <SkeletonRows rows={4} rowClass="h-[29px]" className="gap-0" />
        </div>
      ) : bots.length === 0 ? (
        <p className="px-4 text-[14px] text-muted">
          No bots are configured yet. <code className="font-mono">aigem bot create</code> adds one.
        </p>
      ) : (
        <div className="min-w-0 overflow-x-auto">
          {/* No explicit min-width: the fixed tracks in COLS are the floor, and
              restating their sum here was a second number to keep in step - one
              that had already drifted, painting the header short of the rows. */}
          <div className="w-max min-w-full" role="table" aria-label="Fleet">
            <Header />
            <div role="rowgroup">
              {bots.map((m) => (
                <FleetRow key={m.id} member={m} />
              ))}
            </div>
          </div>
        </div>
      )}

      {/* What the fleet as a whole is spending, against the provider's own
          reading of the quota. It is the one figure here that is not per bot,
          so it sits under the list rather than in it. */}
      <div className="mt-4">
        <Spend />
      </div>
    </div>
  );
}

/** The column labels. Seven unlabelled strings per row is what a screen reader
 *  gets from a grid of spans, so the roster says out loud that it is a table -
 *  the layout stays CSS Grid either way. */
function Header() {
  return (
    <div role="rowgroup">
      <div
        role="row"
        className={`${COLS} items-center gap-3 border-y border-line bg-raised px-4 py-1.5 text-[11px] text-muted`}
      >
        {["bot", "role", "state", "threads", "heartbeat", "next job", "model"].map((c) => (
          <span key={c} role="columnheader" className={c === "threads" ? "text-right" : undefined}>
            {c}
          </span>
        ))}
      </div>
    </div>
  );
}

function FleetRow({ member }: { member: FleetMember }) {
  const live = member.live;
  return (
    <div role="row" className={`${COLS} items-center gap-3 border-b border-line-faint px-4 py-1.5`}>
      <span role="cell" className="truncate text-[14px]">
        {member.name}
      </span>
      <span role="cell" className="truncate text-[13px] text-muted">
        {member.role || "-"}
      </span>
      <span role="cell">
        <State member={member} />
      </span>
      <span role="cell" className="text-right font-mono text-[12px] text-muted">
        {member.threads}
      </span>
      <span role="cell" className="font-mono text-[12px] text-muted">
        {heartbeatOf(live)}
      </span>
      <span role="cell" className="truncate font-mono text-[12px] text-muted">
        {nextJobOf(live)}
      </span>
      <span role="cell" className="truncate font-mono text-[12px] text-muted">
        {live?.model || "-"}
      </span>
    </div>
  );
}

/** What the bot is doing, in the word the daemon chose. Deriving it here would
 *  be the second implementation of one decision, and `aigem chat fleet` draws
 *  the same roster.
 *
 *  `stopped` is the only state drawn in a signal colour, because it is the only
 *  one that means something is wrong. */
function State({ member }: { member: FleetMember }) {
  switch (member.state) {
    case "working":
      return (
        <span className="inline-flex items-center gap-1 text-[12px]">
          <RunDot /> working
        </span>
      );
    case "stopped":
      return <Badge className="justify-self-start border-bad/40 text-bad">stopped</Badge>;
    case "idle":
      return <span className="text-[12px] text-muted">idle</span>;
    default:
      // A daemon too old to send one. The row still carries what it can count.
      return <span className="text-[12px] text-muted">-</span>;
  }
}

function heartbeatOf(live?: LiveBot): string {
  if (!live?.running || !live.heartbeat) return "-";
  return `${live.heartbeat} (t${live.tier})`;
}

/** The next scheduled run, as a clock time - with the weekday when it is later
 *  this week, because "09:00" read on a Friday evening otherwise looks like
 *  tonight, and with a date when it is further out than that.
 *
 *  A run already past is "overdue", not a time. The scheduler will fire it on
 *  its next free tick, so a time is the wrong thing to print: rendered as a
 *  weekday it was indistinguishable from the same weekday next week, and a
 *  one-shot a month stale read as an appointment. */
function nextJobOf(live?: LiveBot): string {
  if (!live?.next_job || !live.next_run) return "-";
  const at = new Date(live.next_run);
  if (Number.isNaN(at.getTime())) return live.next_job;
  return `${live.next_job} ${when(at, new Date())}`;
}

const WEEK_MS = 7 * 24 * 60 * 60 * 1000;

function when(at: Date, now: Date): string {
  if (at.getTime() <= now.getTime()) return "overdue";
  const clock = at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  const sameDay =
    at.getFullYear() === now.getFullYear() &&
    at.getMonth() === now.getMonth() &&
    at.getDate() === now.getDate();
  if (sameDay) return clock;
  if (at.getTime() - now.getTime() < WEEK_MS) {
    return `${at.toLocaleDateString([], { weekday: "short" })} ${clock}`;
  }
  return `${at.toLocaleDateString([], { month: "short", day: "numeric" })} ${clock}`;
}
