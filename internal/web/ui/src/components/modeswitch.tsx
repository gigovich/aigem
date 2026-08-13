import type { Mode } from "@/lib/route";
import { cn } from "@/lib/utils";

const LABELS: Record<Mode, string> = { sessions: "Sessions", chat: "Bots" };

/** The top-level screen selector: two ghost buttons, the active one carrying a
 *  2px underline. Not a pill and not a segmented card - DESIGN.md rules both
 *  out at this density, and the underline is the same 2px marker the rows use
 *  for the same meaning.
 *
 *  The underline is `fg` rather than the accent. The accent means "you are the
 *  one who has to act", and a switch showing which screen is open is not asking
 *  for anything. */
export function ModeSwitch({ mode, onChange }: { mode: Mode; onChange: (next: Mode) => void }) {
  return (
    // A group of toggles, not ARIA tabs. Tabs promise a tabpanel these
    // reference, a roving tabindex and arrow-key movement; these two swap the
    // whole screen and have none of that, so claiming the role would announce
    // "tab 1 of 2" for what is really navigation.
    <div role="group" aria-label="Screen" className="flex shrink-0 items-center gap-1">
      {(Object.keys(LABELS) as Mode[]).map((m) => (
        <button
          key={m}
          aria-pressed={mode === m}
          onClick={() => onChange(m)}
          className={cn(
            "h-9 rounded-md px-2 text-[13px] font-medium transition-colors duration-[120ms] ease-out",
            "border-b-2 focus-visible:outline-none focus-visible:ring-2",
            "focus-visible:ring-accent/60 focus-visible:ring-offset-1 focus-visible:ring-offset-canvas",
            "[@media(pointer:coarse)]:min-h-11",
            mode === m
              ? "border-fg text-fg"
              : "border-transparent text-muted hover:bg-raised hover:text-fg",
          )}
        >
          {LABELS[m]}
        </button>
      ))}
    </div>
  );
}
