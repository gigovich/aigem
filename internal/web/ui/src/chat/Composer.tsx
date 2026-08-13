import { useLayoutEffect, useRef, useState } from "react";
import { Send } from "lucide-react";
import { Button } from "@/components/ui";

interface ComposerProps {
  /** The daemon's own limit, served rather than copied: a client enforcing a
   *  stale one either refuses what would be accepted or accepts what will be
   *  refused after the reader has already spent the words. */
  maxBytes: number;
  /** Whether the socket the message travels over is up. A write to a closed
   *  one is dropped on the floor, so the draft must survive the attempt. */
  connected: boolean;
  onSend: (text: string) => boolean;
}

/** Pinned to the bottom edge, six lines before it scrolls. There is no
 *  @mention autocomplete here yet - stage 7 - and deliberately no way to send
 *  without waking the thread, because there is no such thing: posting is the
 *  whole wake mechanism. */
export function Composer({ maxBytes, connected, onSend }: ComposerProps) {
  const [draft, setDraft] = useState("");
  const [failed, setFailed] = useState(false);
  const box = useRef<HTMLTextAreaElement>(null);
  const bytes = new TextEncoder().encode(draft).length;
  const overLimit = bytes > maxBytes;

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

  const send = () => {
    const text = draft.trim();
    if (!text || overLimit) return;
    // The draft is cleared only once the message is actually on the wire. It
    // travels over the socket, and a socket mid-reconnect drops what is written
    // to it - which used to empty the box for a message that never existed.
    if (!onSend(text)) {
      setFailed(true);
      return;
    }
    setFailed(false);
    setDraft("");
  };

  return (
    // No safe-area padding here: index.css already puts it on the body, and
    // adding it again floats the composer an inset above the home indicator.
    <div className="shrink-0 border-t border-line bg-panel px-4 py-2">
      <div className="flex items-end gap-2">
        <textarea
          ref={box}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            // Enter sends; a newline needs a modifier, as it does everywhere
            // else a message is typed.
            if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault();
              send();
            }
          }}
          rows={1}
          aria-label="Message"
          aria-invalid={overLimit || undefined}
          placeholder="Reply..."
          className="max-h-[9.5rem] min-h-9 flex-1 resize-none overflow-y-auto rounded-md border border-line bg-raised px-3 py-2 text-[14px] outline-none placeholder:text-muted focus:border-accent/60 [@media(pointer:coarse)]:min-h-11"
        />
        <Button size="icon" onClick={send} disabled={!draft.trim() || overLimit} title="Send">
          <Send className="h-4 w-4" />
        </Button>
      </div>
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
