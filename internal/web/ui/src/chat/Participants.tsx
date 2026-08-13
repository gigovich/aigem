import { useEffect, useRef, useState } from "react";
import { Plus, X } from "lucide-react";
import { displayName, type Actor } from "@/lib/chatprotocol";
import { Button, RunDot } from "@/components/ui";
import { cn } from "@/lib/utils";

interface ParticipantsProps {
  participants: string[];
  fleet: Actor[];
  operator: string;
  /** False while the socket is reconnecting: both operations travel over it,
   *  and a control that answers a click with silence is worse than one that is
   *  visibly unavailable. */
  connected: boolean;
  onAdd: (actor: string) => void;
  onRemove: (actor: string) => void;
}

/** Who is in the thread, in the thread's header.
 *
 *  Inline and not a modal: DESIGN.md allows a nested panel in exactly three
 *  places and this is not one of them. Adding is the exception - a list of the
 *  fleet has to appear from somewhere - and it is a popover anchored to its own
 *  button rather than a dialog over the conversation.
 *
 *  Removing a bot is not styled as destructive. It is reversible, and the store
 *  writes a system message either way, so the transcript stays honest without
 *  the interface being alarming about it. */
export function Participants({
  participants,
  fleet,
  operator,
  connected,
  onAdd,
  onRemove,
}: ParticipantsProps) {
  const [adding, setAdding] = useState(false);
  const box = useRef<HTMLDivElement>(null);
  // The control the popover belongs to. Closing it unmounts whatever had focus,
  // so without handing focus back a keyboard reader is dropped at the top of
  // the document - both on Escape and on picking a bot.
  const trigger = useRef<HTMLButtonElement>(null);
  const close = () => {
    setAdding(false);
    trigger.current?.focus();
  };

  // A popover that outlives a click elsewhere is a popover the reader has to
  // hunt for a way out of. Escape and a click outside both close it.
  useEffect(() => {
    if (!adding) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      // Claimed, so the diff overlay behind this - which also listens on the
      // window - does not close along with it. One Escape, one layer.
      e.stopPropagation();
      close();
    };
    const onDown = (e: MouseEvent) => {
      if (!box.current?.contains(e.target as Node)) setAdding(false);
    };
    // Capture, so this runs before the overlay's own window listener and can
    // stop it.
    window.addEventListener("keydown", onKey, true);
    window.addEventListener("mousedown", onDown);
    return () => {
      window.removeEventListener("keydown", onKey, true);
      window.removeEventListener("mousedown", onDown);
    };
  }, [adding]);

  const absent = fleet.filter((a) => a.kind === "bot" && !participants.includes(a.id));
  // The roster is polled, so it can empty while the popover is open. Closing
  // with it avoids leaving an empty box behind a trigger that has vanished.
  const showAdd = adding && absent.length > 0;

  return (
    <div ref={box} className="relative flex flex-wrap items-center gap-x-2 gap-y-1">
      {participants.map((id) => {
        const actor = fleet.find((a) => a.id === id);
        const you = id === operator;
        return (
          <span key={id} className="flex items-center gap-1 text-[12px] text-muted">
            {displayName(id, operator)}
            {actor?.present && <RunDot label={`${actor.name} is running`} />}
            {/* The operator has no way back into a thread they left, since
                adding a participant requires being one, so leaving is not
                offered at all - the daemon refuses it too. */}
            {!you && (
              <button
                onClick={() => onRemove(id)}
                disabled={!connected}
                aria-label={`Remove ${displayName(id, operator)}`}
                // Always visible. DESIGN.md hides row actions until hover, but
                // these are five chips in a header rather than a list of forty
                // rows, and there is nothing here for them to be noise against.
                //
                // Sized like a control and not like its glyph: the row wraps, so
                // a 44px target on touch costs a taller header rather than an
                // overflow, and five 16px targets a few pixels apart in a header
                // a thumb can reach is the trap that rule exists for.
                className={cn(
                  "flex h-6 w-6 shrink-0 items-center justify-center rounded-sm",
                  "[@media(pointer:coarse)]:h-11 [@media(pointer:coarse)]:w-11",
                  "transition-colors duration-[120ms] ease-out",
                  "hover:bg-raised hover:text-fg disabled:text-disabled",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60",
                )}
              >
                <X className="h-3 w-3" aria-hidden />
              </button>
            )}
          </span>
        );
      })}

      {absent.length > 0 && (
        <Button
          ref={trigger}
          variant="ghost"
          size="sm"
          disabled={!connected}
          aria-expanded={adding}
          aria-haspopup="listbox"
          onClick={() => setAdding((v) => !v)}
        >
          <Plus className="mr-1 h-3 w-3" aria-hidden /> add
        </Button>
      )}

      {showAdd && (
        <ul
          role="listbox"
          aria-label="Bots not in this thread"
          className="absolute top-full left-0 z-20 mt-1 max-h-64 w-64 overflow-y-auto rounded-lg border border-line bg-panel"
        >
          {absent.map((a) => (
            <li key={a.id} className="border-b border-line-faint last:border-b-0">
              <button
                role="option"
                aria-selected={false}
                onClick={() => {
                  onAdd(a.id);
                  close();
                }}
                className={
                  "flex h-9 w-full items-center gap-2 px-2.5 text-left text-[13px] " +
                  "hover:bg-raised [@media(pointer:coarse)]:h-11 " +
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:-outline-offset-2"
                }
              >
                <span className="truncate">{a.name}</span>
                {a.role && <span className="truncate text-[12px] text-muted">{a.role}</span>}
                {a.present && <RunDot className="ml-auto" label={`${a.name} is running`} />}
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
