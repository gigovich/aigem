import { useState } from "react";
import type { Actor } from "@/lib/chatprotocol";
import { Button } from "@/components/ui";
import { cn } from "@/lib/utils";

interface NewThreadProps {
  fleet: Actor[];
  /** The daemon's title limit, so a thread is not refused after it was
   *  composed. */
  maxTitleChars: number;
  onCancel: () => void;
  onCreate: (title: string, participants: string[], text: string) => Promise<void>;
}

/** Opening a thread is naming who is in it. That is the whole difference from a
 *  channel, and it is why this asks for the bots before it asks for the text:
 *  posting into a thread wakes everyone in it, so who is in it is the decision.
 *
 *  Inline in the rail rather than a modal: DESIGN.md allows a panel over the
 *  workspace in three places and this is not one of them. */
export function NewThread({ fleet, maxTitleChars, onCancel, onCreate }: NewThreadProps) {
  const [picked, setPicked] = useState<string[]>([]);
  const [title, setTitle] = useState("");
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const bots = fleet.filter((a) => a.kind === "bot");
  const ready = picked.length > 0 && text.trim().length > 0 && !busy;

  const submit = async () => {
    if (!ready) return;
    setBusy(true);
    setError(null);
    try {
      await onCreate(title.trim(), picked, text.trim());
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="shrink-0 border-b border-line px-2 py-2">
      <p className="text-[11px] font-medium text-muted">Who is on it</p>
      <div className="mt-1 flex flex-wrap gap-1">
        {bots.map((bot) => {
          const on = picked.includes(bot.id);
          return (
            <button
              key={bot.id}
              aria-pressed={on}
              onClick={() =>
                setPicked((current) =>
                  on ? current.filter((id) => id !== bot.id) : [...current, bot.id],
                )
              }
              className={cn(
                "inline-flex items-center gap-1 rounded-sm border px-1.5 py-0.5 text-[11px] font-medium",
                "transition-colors duration-[120ms] ease-out",
                "[@media(pointer:coarse)]:min-h-11 [@media(pointer:coarse)]:px-2",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60",
                on ? "border-line bg-raised text-fg" : "border-transparent text-muted hover:bg-raised",
              )}
            >
              {bot.name}
              {bot.role && <span className="text-muted">{bot.role}</span>}
              {/* A bot the fleet has not started cannot answer, and picking it
                  without knowing that is how a thread sits unanswered.
                  In words, not a dot: "started" is not a live state, and a
                  pulse for it would have the whole roster moving while the
                  fleet sits idle. */}
              <span className="text-muted">{bot.present ? "ready" : "stopped"}</span>
            </button>
          );
        })}
        {bots.length === 0 && <p className="text-[12px] text-muted">No bots are registered.</p>}
      </div>

      {/* Labels above the fields, not placeholders standing in for them: a
          placeholder is gone the moment there is anything to check it against. */}
      <label htmlFor="new-thread-title" className="mt-2 block text-[11px] font-medium text-muted">
        Title, optional
      </label>
      <input
        id="new-thread-title"
        value={title}
        maxLength={maxTitleChars}
        onChange={(e) => setTitle(e.target.value)}
        className="mt-1 min-h-9 w-full rounded-md border border-line bg-raised px-2 text-[13px] outline-none focus:border-accent/60 [@media(pointer:coarse)]:min-h-11"
      />
      <label htmlFor="new-thread-text" className="mt-2 block text-[11px] font-medium text-muted">
        What needs doing
      </label>
      <textarea
        id="new-thread-text"
        value={text}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault();
            void submit();
          }
        }}
        rows={2}
        className="mt-1 max-h-[9.5rem] w-full resize-y rounded-md border border-line bg-raised px-2 py-1.5 text-[14px] outline-none focus:border-accent/60"
      />
      {error && (
        <p className="mt-1 font-mono text-[12px] text-bad" role="alert">
          {error}
        </p>
      )}
      <div className="mt-2 flex gap-2">
        <Button size="sm" onClick={() => void submit()} disabled={!ready}>
          Open thread
        </Button>
        <Button variant="ghost" size="sm" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </div>
  );
}
