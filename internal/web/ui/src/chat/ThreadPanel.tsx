import { Plan } from "@/components/plan";
import { ChangedFiles, threadArtifacts, type Artifact } from "@/components/files";
import { SkeletonRows } from "@/components/ui";
import { displayName, type Spend, type Turn } from "@/lib/chatprotocol";
import type { Todo } from "@/lib/protocol";

/** planOf is the thread's working plan: the first turn in the list that wrote
 *  one, which is the newest because the daemon serves runs newest-first.
 *
 *  Carried forward on read rather than at write time. A bot writes a plan in
 *  the turn it revises it and not in the twelve heartbeats after, so copying
 *  the last one onto every turn would fill the table with a plan nobody set. */
export function planOf(turns: Turn[]): { plan: Todo[]; whose?: string } {
  for (const t of turns) {
    if (t.plan && t.plan.length > 0) return { plan: t.plan, whose: t.actor };
  }
  return { plan: [] };
}

/** tokens writes a token count the way a reader scans one. Exact below ten
 *  thousand, because that is a number someone is checking; rounded above,
 *  because past that only the order of magnitude is being read. */
export function tokens(n: number): string {
  if (n < 10_000) return String(n);
  return `${Math.round(n / 1000)}k`;
}

interface ThreadPanelProps {
  thread: string;
  operator: string;
  turns: Turn[];
  spend: Spend | null;
  /** False until the runs have landed. The count and the per-bot table are
   *  suppressed while it is: a confident "0 turns" over a thread that has
   *  eighty is worse than saying nothing yet. */
  loaded: boolean;
  /** Set when the runs could not be read. Reported inside the panel rather than
   *  only in the app-wide notice bar - an error belongs adjacent to the thing
   *  that failed, not thirty rows above it. */
  failed?: string | null;
  openPath?: string;
  /** Bumped whenever a file changes in the watched thread, so an open diff is
   *  refetched rather than read at the version it had when it was opened. */
  version: number;
  onOpenDiff: (a: Artifact) => void;
}

/** What is going on in this thread, beside the conversation: the plan, the
 *  files the newest run touched, and what the whole of it has cost.
 *
 *  All three are read-only views over what the store already has. Nothing here
 *  is a source of truth, which is why none of it needs an event of its own. */
export function ThreadPanel({
  thread,
  operator,
  turns,
  spend,
  loaded,
  failed,
  openPath,
  version,
  onOpenDiff,
}: ThreadPanelProps) {
  const { plan, whose } = planOf(turns);
  // The first run in the list that changed anything, which is the newest
  // because the daemon serves them newest-first - and during a run that is the
  // running one, whose row file count is re-read as the files land. Asking for
  // it by number rather than letting the daemon pick is what keeps the list and
  // the diff behind it on one run while the next is starting.
  const changed = turns.find((t) => (t.files ?? 0) > 0);
  const usage = spend?.usage;
  const total = (usage?.input_tokens ?? 0) + (usage?.output_tokens ?? 0);

  // Who has worked here, and how much of it each did. Derived from the turns
  // already fetched rather than asked for separately: it is the same fact
  // counted a second way.
  const byActor = new Map<string, number>();
  for (const t of turns) byActor.set(t.actor, (byActor.get(t.actor) ?? 0) + 1);
  // Whether the tally below covers the thread or only the page that was
  // fetched. Against runs, not spending turns: a run killed before its first
  // usage flush spent nothing, so comparing the two silently suppressed this
  // for any thread with a few of those - which is most of them.
  const partial = !!spend && spend.runs > turns.length;

  return (
    <>
      {plan.length > 0 && <Plan todos={plan} whose={displayName(whose ?? "", operator)} />}

      <ChangedFiles
        // Keyed by the run, so a later turn's files replace the previous run's
        // rather than reconciling into a list that is half of each.
        key={changed?.seq ?? thread}
        artifactsURL={threadArtifacts(thread, changed?.seq)}
        version={version}
        openPath={openPath}
        onOpen={onOpenDiff}
        title="Changed"
        empty="No run in this thread has written a file."
        count={changed?.files}
        whose={changed ? displayName(changed.actor, operator) : undefined}
      />

      <section aria-label="This thread" className="flex shrink-0 flex-col">
        <div className="flex shrink-0 items-center gap-2 px-3 py-2">
          <h2 className="text-[15px] font-medium">This thread</h2>
          {/* The daemon's own total, not the length of the page above it. The
              runs are fetched a hundred at a time for the summary lines, and a
              thread with two hundred would otherwise report exactly a hundred,
              confidently and forever. */}
          {loaded && spend && (
            <span className="ml-auto font-mono text-[12px] text-muted">
              {spend.runs} {spend.runs === 1 ? "run" : "runs"}
            </span>
          )}
        </div>
        {failed && (
          <p role="alert" className="px-3 pb-2 font-mono text-[12px] text-bad">
            {failed}
          </p>
        )}
        {!loaded && !failed && (
          <SkeletonRows rows={3} rowClass="h-8" className="gap-0 border-t border-line-faint" />
        )}
        <ul className="border-t border-line-faint">
          {loaded && partial && (
            // Said out loud. A per-bot tally drawn from the newest hundred runs
            // is a real number about a subset, and presenting it as the thread's
            // is the kind of quiet miscount nothing else here is allowed.
            <li className="flex min-h-8 items-center border-b border-line-faint px-3 py-1 text-[12px] text-muted">
              the newest {turns.length} runs
            </li>
          )}
          {[...byActor.entries()].map(([actor, count]) => (
            <li
              key={actor}
              className="flex h-8 items-center gap-2 border-b border-line-faint px-3 text-[13px]"
            >
              <span className="truncate">{displayName(actor, operator)}</span>
              <span className="ml-auto shrink-0 font-mono text-[12px] text-muted">
                {count} {count === 1 ? "turn" : "turns"}
              </span>
            </li>
          ))}
          {/* Only what the daemon actually counted. A provider that reported no
              numbers is said out loud rather than folded into the total, which
              is the one thing a cost figure must never quietly do. */}
          {spend && (
            <li className="flex h-8 items-center gap-2 border-b border-line-faint px-3 text-[13px]">
              <span className="text-muted">tokens</span>
              <span className="ml-auto shrink-0 font-mono text-[12px]">{tokens(total)}</span>
            </li>
          )}
          {(usage?.uncounted ?? 0) > 0 && (
            <li className="flex min-h-8 items-center gap-2 border-b border-line-faint px-3 py-1 text-[12px] text-muted">
              <span className="font-mono">{usage?.uncounted}</span> of{" "}
              <span className="font-mono">{usage?.calls}</span> calls reported no usage
            </li>
          )}
          {spend?.models?.map((m) => (
            <li
              key={m}
              className="flex h-8 items-center border-b border-line-faint px-3 font-mono text-[12px] text-muted"
            >
              <span className="truncate">{m}</span>
            </li>
          ))}
        </ul>
      </section>
    </>
  );
}
