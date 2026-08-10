import { Check, Circle } from "lucide-react";
import type { Todo } from "@/lib/protocol";
import { Spinner } from "./ui";
import { cn } from "@/lib/utils";

export function planProgress(todos: Todo[]): { done: number; total: number } {
  return { done: todos.filter((t) => t.status === "completed").length, total: todos.length };
}

function Mark({ status }: { status: string }) {
  if (status === "completed") return <Check className="mt-0.5 h-3.5 w-3.5 shrink-0 text-good" />;
  if (status === "in_progress") return <Spinner className="mt-0.5 shrink-0 border-accent border-t-transparent" />;
  return <Circle className="mt-0.5 h-3.5 w-3.5 shrink-0 text-muted" />;
}

/** The plan as a checklist that reads top to bottom. It was a row of pills
 *  across the full width, where six steps became two and a half on a phone and
 *  the one being worked on was wherever it happened to land. */
export function Plan({ todos }: { todos: Todo[] }) {
  const { done, total } = planProgress(todos);
  return (
    <section aria-label="Plan" className="flex shrink-0 flex-col">
      <div className="flex shrink-0 items-center gap-2 px-3 py-2">
        <h2 className="text-[13px] font-medium">Plan</h2>
        <span className="ml-auto font-mono text-[11px] text-muted">
          {done}/{total}
        </span>
      </div>
      <div className="mx-3 h-1 shrink-0 overflow-hidden rounded-full bg-border">
        <div
          className="h-full rounded-full bg-accent transition-[width]"
          style={{ width: total ? `${(done / total) * 100}%` : "0%" }}
        />
      </div>
      <ol className="px-3 py-2">
        {todos.map((t, i) => (
          <li
            key={i}
            // The step being worked on is the one thing the rail exists to show,
            // and colour alone does not say it to a screen reader.
            aria-current={t.status === "in_progress" ? "step" : undefined}
            className="flex items-start gap-2 py-1"
          >
            <Mark status={t.status} />
            <span
              className={cn(
                // min-w-0: a flex item's automatic minimum is its longest
                // unbreakable run, and break-words is excluded from that - so
                // without this a long step scrolls the rail sideways instead.
                "min-w-0 text-[13px] leading-snug break-words",
                t.status === "completed" && "text-muted line-through",
                t.status === "in_progress" && "font-medium text-fg",
                t.status !== "completed" && t.status !== "in_progress" && "text-muted",
              )}
            >
              {t.text}
            </span>
          </li>
        ))}
      </ol>
    </section>
  );
}
