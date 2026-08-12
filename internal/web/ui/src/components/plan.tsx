import { Check, Circle } from "lucide-react";
import type { Todo } from "@/lib/protocol";
import { RunDot } from "./ui";
import { cn } from "@/lib/utils";

export function planProgress(todos: Todo[]): { done: number; total: number } {
  return { done: todos.filter((t) => t.status === "completed").length, total: todos.length };
}

function Mark({ status }: { status: string }) {
  if (status === "completed") {
    return <Check className="mt-0.5 h-3.5 w-3.5 shrink-0 text-good" aria-label="Done" />;
  }
  if (status === "in_progress") return <RunDot className="mt-1.5" label="In progress" />;
  return <Circle className="mt-0.5 h-3.5 w-3.5 shrink-0 text-disabled" aria-hidden />;
}

/** The plan as a checklist that reads top to bottom. It was a row of pills
 *  across the full width, where six steps became two and a half on a phone and
 *  the one being worked on was wherever it happened to land. */
export function Plan({ todos }: { todos: Todo[] }) {
  const { done, total } = planProgress(todos);
  return (
    <section aria-label="Plan" className="flex shrink-0 flex-col">
      <div className="flex shrink-0 items-center gap-2 px-3 py-2">
        <h2 className="text-[15px] font-medium">Plan</h2>
        {/* A count, not a percentage: seven steps do not divide into a figure
            anyone can act on, and the reader is counting steps anyway. */}
        <span className="ml-auto font-mono text-[12px] text-muted">
          {done}/{total}
        </span>
      </div>
      <ol className="border-t border-line-faint">
        {todos.map((t, i) => (
          <li
            key={i}
            // The step being worked on is the one thing the rail exists to show,
            // and colour alone does not say it to a screen reader.
            aria-current={t.status === "in_progress" ? "step" : undefined}
            className="flex items-start gap-2 border-b border-line-faint px-3 py-1.5"
          >
            <Mark status={t.status} />
            <span
              className={cn(
                // min-w-0: a flex item's automatic minimum is its longest
                // unbreakable run, and break-words is excluded from that - so
                // without this a long step scrolls the rail sideways instead.
                "min-w-0 text-[13px] leading-snug break-words",
                // Struck through as well as recoloured, so a finished step still
                // reads as finished with the colour taken away.
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
