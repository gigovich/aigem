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
 *  "-" rather than a row of confident zeroes.
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
          Every bot this daemon was asked to run. Stopped means the daemon could not start it and
          is still retrying.
        </p>
      </div>

      {!loaded ? (
        <div className="px-4">
          <SkeletonRows rows={4} rowClass="h-8" />
        </div>
      ) : bots.length === 0 ? (
        <p className="px-4 text-[14px] text-muted">
          No bots are configured yet. <code className="font-mono">aigem bot create</code> adds one.
        </p>
      ) : (
        <div className="min-w-0 overflow-x-auto">
          <div className="min-w-[57rem]">
            <div
              className={`${COLS} items-center gap-3 border-y border-line bg-raised px-4 py-1.5 text-[11px] text-muted`}
            >
              <span>bot</span>
              <span>role</span>
              <span>state</span>
              <span className="text-right">threads</span>
              <span>heartbeat</span>
              <span>next job</span>
              <span>model</span>
            </div>
            <ul>
              {bots.map((m) => (
                <FleetRow key={m.id} member={m} />
              ))}
            </ul>
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

function FleetRow({ member }: { member: FleetMember }) {
  const live = member.live;
  return (
    <li className={`${COLS} items-center gap-3 border-b border-line-faint px-4 py-1.5`}>
      <span className="truncate text-[14px]">{member.name}</span>
      <span className="truncate text-[13px] text-muted">{member.role || "-"}</span>
      <State member={member} />
      <span className="text-right font-mono text-[12px] text-muted">{member.threads}</span>
      <span className="font-mono text-[12px] text-muted">{heartbeatOf(live)}</span>
      <span className="truncate font-mono text-[12px] text-muted">{nextJobOf(live)}</span>
      <span className="truncate font-mono text-[12px] text-muted">{live?.model || "-"}</span>
    </li>
  );
}

/** What the bot is doing, in one word.
 *
 *  `working` is a turn row with no end, exactly as the inbox reads it, so the
 *  two screens cannot disagree. `stopped` is the only state drawn in a signal
 *  colour, because it is the only one that means something is wrong. A bot no
 *  daemon reported on is "-", never "stopped". */
function State({ member }: { member: FleetMember }) {
  if (member.working) {
    return (
      <span className="inline-flex items-center gap-1 text-[12px]">
        <RunDot /> working
      </span>
    );
  }
  if (!member.live) return <span className="text-[12px] text-muted">-</span>;
  if (!member.live.running) {
    return <Badge className="justify-self-start border-bad/40 text-bad">stopped</Badge>;
  }
  return <span className="text-[12px] text-muted">idle</span>;
}

function heartbeatOf(live?: LiveBot): string {
  if (!live?.running || !live.heartbeat) return "-";
  return `${live.heartbeat} (t${live.tier})`;
}

/** The next scheduled run, as a clock time - with the weekday when it is not
 *  today, because "09:00" read on a Friday evening otherwise looks like tonight.
 *  A job already overdue is shown at the time it was due rather than hidden:
 *  that is what the scheduler does with it on its next tick. */
function nextJobOf(live?: LiveBot): string {
  if (!live?.next_job || !live.next_run) return "-";
  const at = new Date(live.next_run);
  if (Number.isNaN(at.getTime())) return live.next_job;
  const now = new Date();
  const sameDay =
    at.getFullYear() === now.getFullYear() &&
    at.getMonth() === now.getMonth() &&
    at.getDate() === now.getDate();
  const clock = at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  return `${live.next_job} ${sameDay ? clock : `${at.toLocaleDateString([], { weekday: "short" })} ${clock}`}`;
}
