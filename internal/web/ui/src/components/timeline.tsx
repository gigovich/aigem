import { useState, type ReactNode } from "react";
import { Check, ChevronRight, CornerDownRight, TriangleAlert, Wrench } from "lucide-react";
import type { Item, RunStep } from "@/lib/session";
import { Badge, RunDot } from "./ui";
import { Markdown } from "./md";
import { argSummary, cn } from "@/lib/utils";

/** Where the rest of an oversized tool result lives. It is a function rather
 *  than an id because the same timeline draws a session's stream and a bot
 *  thread's, and those keep their blobs under different routes. */
export type BlobURL = (seq: number) => string;

/** The plan is rendered as a plan, in the rail. Its writes were also arriving
 *  here as tool cards - six identical rows of raw JSON per turn, which is most
 *  of what the timeline showed and none of what it meant. */
const PLAN_TOOL = "todo_write";

/** Hidden only when it worked. A failed plan write leaves the rail showing the
 *  previous plan, so dropping the card too would erase the failure entirely. */
function isPlanWrite(item: Item): boolean {
  return item.kind === "tool" && item.name === PLAN_TOOL && !item.error;
}

/** Who is speaking, said in words. The turns are all left-aligned on one spine,
 *  so the label is what separates them rather than which side they sit on. */
function TurnLabel({ who, children }: { who: "you" | "aigem"; children?: ReactNode }) {
  return (
    <div
      className={cn(
        "flex items-center gap-1.5 text-[11px] font-medium",
        who === "you" ? "text-fg" : "text-muted",
      )}
    >
      <span>{who}</span>
      {children}
    </div>
  );
}

/** A tool call and its result as one card, which is the thing the terminal
 *  cannot do: there, a result is whatever line happened to arrive next. */
function ToolCard({ item, blobURL }: { item: Extract<Item, { kind: "tool" }>; blobURL: BlobURL }) {
  const [open, setOpen] = useState(false);
  const [full, setFull] = useState<string | null>(null);
  // A blob that will not load is said out loud. Silently showing the head
  // forever is how a broken key went unnoticed in the first place.
  const [failed, setFailed] = useState(false);
  const body = full ?? item.result ?? "";

  const expand = async () => {
    const next = !open;
    setOpen(next);
    // The journal keeps only the head of a large result; the rest is fetched
    // when someone actually looks at it.
    if (next && item.blob && item.blobSeq && full === null) {
      try {
        const res = await fetch(blobURL(item.blobSeq), {
          headers: { Authorization: `Bearer ${sessionStorage.getItem("aigem-token") ?? ""}` },
        });
        if (res.ok) setFull(await res.text());
        else setFailed(true);
      } catch {
        setFailed(true);
      }
    }
  };

  return (
    <div className="rounded-lg border border-line bg-panel">
      {/* One monospaced line closed: the call, its one readable argument, and
          whether it worked. Everything else is a click away. */}
      <button
        onClick={expand}
        aria-expanded={open}
        className="flex h-8 w-full items-center gap-2 px-2.5 text-left [@media(pointer:coarse)]:h-11"
      >
        <ChevronRight
          className={cn("h-3.5 w-3.5 shrink-0 text-muted transition-transform duration-[120ms]", open && "rotate-90")}
        />
        <Wrench className="h-3.5 w-3.5 shrink-0 text-muted" aria-hidden />
        <span className="shrink-0 font-mono text-[12px] text-fg">{item.name}</span>
        <span className="truncate font-mono text-[12px] text-muted">{argSummary(item.args)}</span>
        <span className="ml-auto shrink-0">
          {!item.done ? (
            <RunDot label={`${item.name} is running`} />
          ) : item.error ? (
            <Badge className="border-bad/40 text-bad">failed</Badge>
          ) : (
            <Check className="h-3.5 w-3.5 text-good" aria-label="Succeeded" />
          )}
        </span>
      </button>
      {open && (
        <div className="border-t border-line px-2.5 py-2">
          <pre className="overflow-x-auto whitespace-pre-wrap break-words font-mono text-[12px] leading-[1.45] text-muted">
            {JSON.stringify(item.args, null, 2)}
          </pre>
          {(item.error || body) && (
            <pre className="mt-2 max-h-96 overflow-auto whitespace-pre-wrap break-words border-t border-line pt-2 font-mono text-[12px] leading-[1.45]">
              <span className={item.error ? "text-bad" : "text-fg"}>{item.error || body}</span>
            </pre>
          )}
          {item.blob && full === null && (
            <p className={cn("mt-1 text-[12px]", failed ? "text-bad" : "text-muted")}>
              {failed
                ? `could not load the rest of this result (${item.bytes} bytes)`
                : `showing the first ${item.result?.length ?? 0} of ${item.bytes} bytes`}
            </p>
          )}
        </div>
      )}
    </div>
  );
}

function Step({ s }: { s: RunStep }) {
  return (
    <div className="flex items-center gap-2 py-0.5 text-[12px]">
      <CornerDownRight className="h-3 w-3 shrink-0 text-muted" aria-hidden />
      <span className="font-mono text-fg">{s.name}</span>
      {!s.done ? (
        <RunDot label={`${s.name} is running`} />
      ) : s.error ? (
        <span className="text-bad">failed</span>
      ) : null}
    </div>
  );
}

/** A delegated run is its own lane. Concurrent subagents interleave in the
 *  terminal because a terminal only has one column; here they do not have to. */
function RunLane({ item }: { item: Extract<Item, { kind: "run" }> }) {
  return (
    <div className="rounded-lg border border-line bg-panel px-2.5 py-2">
      <div className="flex items-center gap-2">
        <Badge className={cn(item.failed && "border-bad/40 text-bad")}>{item.agent}</Badge>
        <span className="truncate text-[13px] text-muted">{item.prompt}</span>
        {!item.done && <RunDot className="ml-auto" label={`${item.agent} is running`} />}
      </div>
      {item.steps.length > 0 && (
        <div className="mt-1 border-l border-line pl-2">
          {item.steps.map((s, i) => (
            <Step key={`${s.id}-${i}`} s={s} />
          ))}
        </div>
      )}
    </div>
  );
}

export function Timeline({ items, blobURL }: { items: Item[]; blobURL: BlobURL }) {
  const shown = items.filter((it) => !isPlanWrite(it));
  return (
    <div className="px-4 py-4">
      {/* One continuous spine down the left. Chronology is the only ordering
          this stream has, so nothing is allowed to hang off the other side. */}
      <div className="flex flex-col gap-3 border-l border-line-faint pl-4">
        {shown.map((item) => {
          switch (item.kind) {
            case "user":
              return (
                <div key={`${item.kind}-${item.seq}`}>
                  <TurnLabel who="you">
                    {item.injected && <span className="text-muted">added mid-turn</span>}
                    {item.images > 0 && <span className="text-muted">{item.images} image(s)</span>}
                  </TurnLabel>
                  <p className="mt-1 max-w-[68ch] whitespace-pre-wrap break-words text-[14px]">
                    {item.text}
                  </p>
                </div>
              );
            case "assistant":
              return (
                <div key={`${item.kind}-${item.seq}`}>
                  <TurnLabel who="aigem" />
                  <div className="mt-1">
                    <Markdown text={item.text} streaming={item.streaming} />
                  </div>
                </div>
              );
            case "tool":
              return <ToolCard key={`${item.kind}-${item.seq}`} item={item} blobURL={blobURL} />;
            case "run":
              return <RunLane key={`${item.kind}-${item.seq}`} item={item} />;
            case "notice":
              return (
                <div
                  key={`${item.kind}-${item.seq}`}
                  className={cn(
                    "flex max-w-[68ch] items-start gap-2 rounded-md border px-2.5 py-1.5 text-[13px]",
                    item.tone === "error"
                      ? "border-bad/35 bg-bad/12 text-bad"
                      : "border-line bg-panel text-muted",
                  )}
                >
                  {item.tone === "error" && (
                    <TriangleAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-label="Error" />
                  )}
                  <span className="min-w-0 whitespace-pre-wrap break-words">{item.text}</span>
                </div>
              );
          }
        })}
      </div>
    </div>
  );
}
