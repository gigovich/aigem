import { useState } from "react";
import { ChevronRight, CornerDownRight, TriangleAlert, User, Wrench } from "lucide-react";
import type { Item, RunStep } from "@/lib/session";
import { Badge, Spinner } from "./ui";
import { Markdown } from "./md";
import { cn } from "@/lib/utils";

function argSummary(args: unknown): string {
  if (args == null) return "";
  if (typeof args === "string") return args;
  const o = args as Record<string, unknown>;
  for (const k of ["cmd", "command", "path", "pattern", "query", "url"]) {
    if (typeof o[k] === "string") return o[k] as string;
  }
  return JSON.stringify(args);
}

/** A tool call and its result as one card, which is the thing the terminal
 *  cannot do: there, a result is whatever line happened to arrive next. */
function ToolCard({ item, sessionID }: { item: Extract<Item, { kind: "tool" }>; sessionID: string }) {
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
        const res = await fetch(`/api/sessions/${sessionID}/blobs/${item.blobSeq}`, {
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
    <div className="rounded-lg border border-border bg-panel">
      <button
        onClick={expand}
        className="flex w-full items-center gap-2 px-3 py-2 text-left"
      >
        <ChevronRight
          className={cn("h-3.5 w-3.5 shrink-0 text-muted transition-transform", open && "rotate-90")}
        />
        <Wrench className="h-3.5 w-3.5 shrink-0 text-accent" />
        <span className="shrink-0 font-mono text-[13px] text-fg">{item.name}</span>
        <span className="truncate font-mono text-[12px] text-muted">{argSummary(item.args)}</span>
        <span className="ml-auto shrink-0">
          {!item.done ? (
            <Spinner />
          ) : item.error ? (
            <Badge className="border-bad/40 text-bad">failed</Badge>
          ) : null}
        </span>
      </button>
      {open && (
        <div className="border-t border-border px-3 py-2">
          <pre className="overflow-x-auto whitespace-pre-wrap break-words font-mono text-[12px] text-muted">
            {JSON.stringify(item.args, null, 2)}
          </pre>
          {(item.error || body) && (
            <pre className="mt-2 max-h-96 overflow-auto whitespace-pre-wrap break-words border-t border-border pt-2 font-mono text-[12px]">
              <span className={item.error ? "text-bad" : "text-fg"}>{item.error || body}</span>
            </pre>
          )}
          {item.blob && full === null && (
            <p className={cn("mt-1 text-[11px]", failed ? "text-bad" : "text-muted")}>
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
      <CornerDownRight className="h-3 w-3 shrink-0 text-muted" />
      <span className="font-mono text-fg">{s.name}</span>
      {!s.done ? <Spinner /> : s.error ? <span className="text-bad">failed</span> : null}
    </div>
  );
}

/** A delegated run is its own lane. Concurrent subagents interleave in the
 *  terminal because a terminal only has one column; here they do not have to. */
function RunLane({ item }: { item: Extract<Item, { kind: "run" }> }) {
  return (
    <div className="rounded-lg border border-border bg-panel px-3 py-2">
      <div className="flex items-center gap-2">
        <Badge className={cn(item.failed && "border-bad/40 text-bad")}>{item.agent}</Badge>
        <span className="truncate text-[13px] text-muted">{item.prompt}</span>
        {!item.done && <Spinner className="ml-auto" />}
      </div>
      {item.steps.length > 0 && (
        <div className="mt-1 border-l border-border pl-2">
          {item.steps.map((s, i) => (
            <Step key={`${s.id}-${i}`} s={s} />
          ))}
        </div>
      )}
    </div>
  );
}

export function Timeline({ items, sessionID }: { items: Item[]; sessionID: string }) {
  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-3 px-3 py-4">
      {items.map((item, i) => {
        switch (item.kind) {
          case "user":
            return (
              <div key={i} className="flex justify-end">
                <div className="max-w-[85%] rounded-2xl rounded-br-sm bg-panel-2 px-3.5 py-2">
                  <div className="flex items-center gap-1.5 text-[11px] text-muted">
                    <User className="h-3 w-3" />
                    {item.injected && <span>added mid-turn</span>}
                    {item.images > 0 && <span>{item.images} image(s)</span>}
                  </div>
                  <p className="whitespace-pre-wrap break-words text-[15px]">{item.text}</p>
                </div>
              </div>
            );
          case "assistant":
            return (
              <div key={i} className="max-w-none">
                <Markdown text={item.text} />
                {item.streaming && <Spinner className="ml-0.5 align-middle" />}
              </div>
            );
          case "tool":
            return <ToolCard key={i} item={item} sessionID={sessionID} />;
          case "run":
            return <RunLane key={i} item={item} />;
          case "notice":
            return (
              <div
                key={i}
                className={cn(
                  "flex items-start gap-2 rounded-md border px-3 py-1.5 text-[13px]",
                  item.tone === "error"
                    ? "border-bad/40 bg-bad/10 text-bad"
                    : "border-border bg-panel text-muted",
                )}
              >
                {item.tone === "error" && <TriangleAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" />}
                <span className="whitespace-pre-wrap break-words">{item.text}</span>
              </div>
            );
        }
      })}
    </div>
  );
}
