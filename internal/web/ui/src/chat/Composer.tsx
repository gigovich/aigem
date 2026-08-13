import { useLayoutEffect, useMemo, useRef, useState } from "react";
import { Send } from "lucide-react";
import { Button, RunDot } from "@/components/ui";
import type { Actor } from "@/lib/chatprotocol";
import { cn } from "@/lib/utils";

interface ComposerProps {
  /** The daemon's own limit, served rather than copied: a client enforcing a
   *  stale one either refuses what would be accepted or accepts what will be
   *  refused after the reader has already spent the words. */
  maxBytes: number;
  /** Whether the socket the message travels over is up. A write to a closed
   *  one is dropped on the floor, so the draft must survive the attempt. */
  connected: boolean;
  /** Everyone who could be named, and who is already here. A name that is not
   *  in the thread is added to it when the message is sent. */
  fleet: Actor[];
  participants: string[];
  onSend: (text: string) => boolean;
  /** Adds a named bot that was not already a participant. It is a convenience
   *  over the participants op, not a second mechanism: the daemon still decides,
   *  and the store still writes the system message that records it. */
  onAdd: (actor: string) => void;
}

/** What may precede an "@name" for it to be a mention.
 *
 *  Spelled out rather than `\s`, because JavaScript's `\s` and Go's are not the
 *  same set - JS includes U+00A0, U+2007, U+202F and the line separators, Go's
 *  is `[\t\n\f\r ]`. This has to be the daemon's rule exactly: it decides whom
 *  the composer adds to a thread, the daemon decides whom the message names, and
 *  a non-breaking space before a name made the two disagree - the bot joined and
 *  was never addressed. */
const BOUNDARY = /[\t\n\f\r (]/;
const MENTION = /(^|[\t\n\f\r (])@([A-Za-z0-9._-]{1,64})/g;

/** mentionAt finds the "@name" the caret is inside, if any. It is anchored to
 *  the caret rather than to the whole draft: a message that already names one
 *  bot must not reopen its list when the reader gets to the end of a sentence. */
export function mentionAt(text: string, caret: number): { at: number; query: string } | null {
  const head = text.slice(0, caret);
  const at = head.lastIndexOf("@");
  if (at < 0) return null;
  // Only at a word boundary, so an email address is not a mention.
  if (at > 0 && !BOUNDARY.test(head[at - 1])) return null;
  const query = head.slice(at + 1);
  // The same character class the daemon's own mention pattern accepts. A space
  // ends the name, which is what closes the list without a keystroke for it.
  if (!/^[A-Za-z0-9._-]*$/.test(query)) return null;
  return { at, query };
}

/** Pinned to the bottom edge, six lines before it scrolls. There is deliberately
 *  no way to send without waking the thread, because there is no such thing:
 *  posting is the whole wake mechanism. */
export function Composer({
  maxBytes,
  connected,
  fleet,
  participants,
  onSend,
  onAdd,
}: ComposerProps) {
  const [draft, setDraft] = useState("");
  const [failed, setFailed] = useState(false);
  const [caret, setCaret] = useState(0);
  const [picked, setPicked] = useState(0);
  // Set by Escape, cleared by the next thing typed: dismissing the list must
  // not dismiss it for the rest of the message.
  const [dismissed, setDismissed] = useState(false);
  const box = useRef<HTMLTextAreaElement>(null);
  // Memoised: the composer re-renders with the screen, which re-renders on every
  // timeline frame, and this re-encodes the whole draft each time.
  const bytes = useMemo(() => new TextEncoder().encode(draft).length, [draft]);
  const overLimit = bytes > maxBytes;

  const mention = dismissed ? null : mentionAt(draft, caret);
  const matches = mention
    ? fleet
        .filter((a) => a.kind === "bot" && a.name.toLowerCase().startsWith(mention.query.toLowerCase()))
        .slice(0, 6)
    : [];
  const open = matches.length > 0;
  // Clamped rather than reset: the list shortens as the name is typed, and an
  // index left past the end selects nothing when Enter is pressed.
  const active = Math.min(picked, matches.length - 1);

  // A textarea does not grow with its content, so without this the second line
  // already scrolls inside a one-line box. Measured rather than counted: the
  // wrapped height of what was typed is the only thing that knows how many
  // lines it actually became. The max-height caps it at six.
  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${el.scrollHeight}px`;
  }, [draft]);

  const complete = (actor: Actor) => {
    if (!mention) return;
    const rest = draft.slice(caret);
    const next = `${draft.slice(0, mention.at)}@${actor.name} ${rest}`;
    const to = mention.at + actor.name.length + 2;
    setDraft(next);
    setPicked(0);
    setDismissed(false);
    // Synchronously, not in the frame callback below. The list is anchored to
    // the caret and nothing else, so leaving the caret at the half-typed name
    // for a frame leaves the list open over the name it just completed.
    setCaret(to);
    // The caret goes after the name, not to the end: a mention typed into the
    // middle of a sentence would otherwise send the reader to the far end of
    // it. In a frame callback because the textarea has not been given the new
    // value yet, and setting a range past the current one is clamped.
    requestAnimationFrame(() => {
      const el = box.current;
      if (!el) return;
      el.focus();
      el.setSelectionRange(to, to);
    });
  };

  const send = () => {
    const text = draft.trim();
    if (!text || overLimit) return;
    // Before the message, not after. The daemon applies socket ops strictly in
    // order, and it resolves "@name" only against actors already in the thread -
    // so sending first means the mention is dropped, the bot joins a moment
    // later to a membership note it ignores, and the message that named it was
    // addressed to nobody. Naming a bot that then says nothing is the whole
    // failure this affordance exists to avoid.
    //
    // The cost of this order is a bot added for a message the socket then
    // refused: a membership change with nothing behind it, which is visible,
    // reversible, and recorded - unlike a silent non-wake.
    for (const a of namedIn(text, fleet)) {
      if (!participants.includes(a.id)) onAdd(a.id);
    }
    // The draft is cleared only once the message is actually on the wire. It
    // travels over the socket, and a socket mid-reconnect drops what is written
    // to it - which used to empty the box for a message that never existed.
    if (!onSend(text)) {
      setFailed(true);
      return;
    }
    setFailed(false);
    setDraft("");
    setCaret(0);
  };

  return (
    // No safe-area padding here: index.css already puts it on the body, and
    // adding it again floats the composer an inset above the home indicator.
    <div className="relative shrink-0 border-t border-line bg-panel px-4 py-2">
      {open && (
        <ul
          // Above the composer, not below it: below is the edge of the viewport
          // on a phone, and on a laptop it is behind the software keyboard.
          id="mention-list"
          role="listbox"
          aria-label="Bots you can name"
          className="absolute bottom-full left-4 z-20 mb-1 w-72 overflow-hidden rounded-lg border border-line bg-panel"
        >
          {matches.map((a, i) => (
            <li key={a.id} className="border-b border-line-faint last:border-b-0">
              <button
                id={`mention-${a.id}`}
                role="option"
                aria-selected={i === active}
                // Out of the tab order: Tab belongs to the composer, and walking
                // into a popup that the next keystroke closes is a trap. The
                // list is driven by the arrow keys, which is what a combobox
                // announces to a reader.
                tabIndex={-1}
                // Mouse down, not click: a click fires after the textarea has
                // already lost focus, and the list is gone by then.
                onMouseDown={(e) => {
                  e.preventDefault();
                  complete(a);
                }}
                className={cn(
                  "flex h-9 w-full items-center gap-2 pr-2.5 pl-0 text-left text-[13px]",
                  "[@media(pointer:coarse)]:h-11",
                  i === active ? "bg-raised text-fg" : "text-muted hover:bg-raised",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60",
                )}
              >
                <span
                  aria-hidden
                  // fg, not accent. Stage 6 settled this on the inbox row: the
                  // accent says "you are the one who has to act", and spending
                  // it on "the row your cursor is on" is what stops it meaning
                  // anything where it does.
                  className={cn("h-full w-0.5 shrink-0", i === active ? "bg-fg" : "bg-transparent")}
                />
                <span className="truncate pl-1.5 text-fg">{a.name}</span>
                {a.role && <span className="truncate text-[12px] text-muted">{a.role}</span>}
                {/* One ml-auto in the row, or the free space is split between
                    them and neither lands where it was meant to. */}
                <span className="ml-auto flex shrink-0 items-center gap-2">
                  {a.present && <RunDot label={`${a.name} is running`} />}
                  {!participants.includes(a.id) && (
                    <span className="text-[11px] font-medium text-muted">adds to this thread</span>
                  )}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}

      <div className="flex items-end gap-2">
        <textarea
          ref={box}
          value={draft}
          onChange={(e) => {
            setDraft(e.target.value);
            setCaret(e.target.selectionStart ?? e.target.value.length);
            setDismissed(false);
          }}
          // Both, because a caret moved by click or by arrow key changes which
          // "@name" it is inside without changing the text at all.
          onSelect={(e) => setCaret(e.currentTarget.selectionStart ?? 0)}
          // The list is anchored to the caret, so leaving the field is leaving
          // it. Safe against picking from the list itself: those entries commit
          // on mousedown, which is prevented, so the field never loses focus.
          onBlur={() => setDismissed(true)}
          onKeyDown={(e) => {
            if (open) {
              if (e.key === "ArrowDown") {
                e.preventDefault();
                setPicked((p) => (Math.min(p, matches.length - 1) + 1) % matches.length);
                return;
              }
              if (e.key === "ArrowUp") {
                e.preventDefault();
                setPicked((p) => (Math.min(p, matches.length - 1) + matches.length - 1) % matches.length);
                return;
              }
              if (e.key === "Enter" && !e.nativeEvent.isComposing) {
                e.preventDefault();
                complete(matches[active]);
                return;
              }
              if (e.key === "Escape") {
                e.preventDefault();
                // A flag, not a caret nudge. The list is keyed on the mention
                // the caret is inside, and while you are typing one the caret
                // is already at the end of the draft - so moving it there was a
                // no-op that also swallowed the keypress.
                setDismissed(true);
                return;
              }
            }
            // Enter sends; a newline needs a modifier, as it does everywhere
            // else a message is typed.
            if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault();
              send();
            }
          }}
          rows={1}
          aria-label="Message"
          // Announced as a combobox only while the list is up, so a composer
          // with no autocomplete showing is still just a text box. Without
          // activedescendant the arrow keys move a highlight a screen reader
          // never hears about.
          role={open ? "combobox" : undefined}
          aria-expanded={open || undefined}
          aria-controls={open ? "mention-list" : undefined}
          aria-activedescendant={open ? `mention-${matches[active]?.id}` : undefined}
          aria-autocomplete={open ? "list" : undefined}
          aria-invalid={overLimit || undefined}
          placeholder="Reply..."
          className="max-h-[9.5rem] min-h-9 flex-1 resize-none overflow-y-auto rounded-md border border-line bg-raised px-3 py-2 text-[14px] outline-none placeholder:text-muted focus:border-accent/60 [@media(pointer:coarse)]:min-h-11"
        />
        <Button size="icon" onClick={send} disabled={!draft.trim() || overLimit} title="Send">
          <Send className="h-4 w-4" />
        </Button>
      </div>

      {/* Said before the message goes, not after. Adding a bot to a thread is a
          membership change, and one that happens as a side effect of a word in
          a sentence has to be visible while there is still time to delete it. */}
      {newcomers(draft, fleet, participants).length > 0 && (
        <p className="mt-1 text-[12px] text-muted">
          adds {newcomers(draft, fleet, participants).map((a) => a.name).join(", ")} to this thread
        </p>
      )}
      {/* Only once it matters. A byte counter on every message is a number
          nobody reads until the one time it stops them. */}
      {overLimit && (
        <p role="alert" className="mt-1 font-mono text-[12px] text-bad">
          {bytes} bytes; this daemon accepts {maxBytes}.
        </p>
      )}
      {/* Not gated on the connection flag. Between a socket erroring and the
          close event that clears that flag there is a window where the write
          was dropped and the page still believes it is connected, and a kept
          draft with no explanation is the same silence this replaced. */}
      {failed && (
        <p role="alert" className="mt-1 text-[12px] text-bad">
          {connected ? "That did not send." : "Not connected."} Your message is still here - send it
          again in a moment.
        </p>
      )}
    </div>
  );
}

/** namedIn resolves the "@name"s actually written in a message.
 *
 *  Deliberately the same rule as the daemon's `mentionRe`, down to the boundary
 *  set and the case-insensitive comparison: this decides whom to add, the daemon
 *  decides whom to mention, and a name only one of them recognises is a bot that
 *  joins without being addressed or is addressed without joining. Typing
 *  "@Demetre" rather than picking from the list did exactly that. */
export function namedIn(text: string, fleet: Actor[]): Actor[] {
  const out: Actor[] = [];
  for (const m of text.matchAll(MENTION)) {
    const name = m[2].toLowerCase();
    const found = fleet.find((a) => a.kind === "bot" && a.name.toLowerCase() === name);
    if (found && !out.includes(found)) out.push(found);
  }
  return out;
}

/** newcomers are the named bots that are not in the thread yet. */
function newcomers(text: string, fleet: Actor[], participants: string[]): Actor[] {
  return namedIn(text, fleet).filter((a) => !participants.includes(a.id));
}
