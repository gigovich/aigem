import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { FileDiff, X } from "lucide-react";
import { api } from "@/lib/protocol";
import { Badge, Button, SkeletonRows } from "./ui";
import { cn } from "@/lib/utils";

export interface Artifact { path: string; created: boolean; old?: string; new?: string }

/** The tail of a path, which is what identifies a file; the head is shared by
 *  everything in the project and eats the column. The daemon reports paths
 *  relative to the working directory, and absolute ones for anything outside it.
 *  Truncating from the left with `dir="rtl"` looked right until a leading slash
 *  was reordered onto the end of the name. */
export function shortPath(path: string, segments = 2): string {
  const parts = path.split("/").filter(Boolean);
  return parts.slice(-segments).join("/");
}

/** Labels short enough to read and long enough to tell apart. Two segments is
 *  usually plenty, but parallel trees put `components/files.tsx` in the list
 *  twice, and the full path is only on a tooltip - which a phone does not have. */
export function uniqueLabels(paths: string[]): Map<string, string> {
  const out = new Map<string, string>();
  for (const path of paths) {
    let segments = 2;
    let label = shortPath(path, segments);
    while (
      segments < 12 &&
      paths.some((other) => other !== path && shortPath(other, segments) === label)
    ) {
      segments += 1;
      label = shortPath(path, segments);
    }
    out.set(path, label);
  }
  return out;
}

type Row = {
  left?: string;
  right?: string;
  ln?: number;
  rn?: number;
  kind: "same" | "add" | "del" | "replace";
};

/** Rows past this are not rendered. The quadratic guard bounds the cost of
 *  computing a diff; this bounds the cost of showing one, which a pair of
 *  generated files reaches first: a row is two or four elements, and the DOM
 *  gives out long before a reader would have scrolled here. */
export const MAX_ROWS = 4000;

/** A file's lines. A text file ends in a newline, and splitting on it leaves a
 *  final empty element that is not a line: numbered, it advertises a line 201 in
 *  a 200-line file. */
function lines(text: string): string[] {
  if (!text.length) return [];
  const out = text.split("\n");
  if (out[out.length - 1] === "") out.pop();
  return out;
}

const noNewline = "\\ No newline at end of file";

/** Add the conventional marker only when two existing, nonempty versions differ
 *  in whether their last line is terminated. It describes the preceding line,
 *  so deliberately has no line number of its own. */
function markMissingNewline(rows: Row[], oldText: string, newText: string): Row[] {
  if (!oldText || !newText || oldText.endsWith("\n") === newText.endsWith("\n")) return rows;
  return [
    ...rows,
    oldText.endsWith("\n")
      ? { right: noNewline, kind: "add" }
      : { left: noNewline, kind: "del" },
  ];
}

/** A line diff over the whole file. The longest common subsequence is quadratic,
 *  so a large pair uses a bounded line-by-line fallback rather than locking the
 *  tab. Common edges are retained, and equal paired lines remain unchanged. */
export function diff(oldText: string, newText: string): Row[] {
  const a = lines(oldText);
  const b = lines(newText);
  if (a.length * b.length > 4_000_000) {
    const rows: Row[] = [];
    let prefix = 0;
    while (prefix < a.length && prefix < b.length && a[prefix] === b[prefix]) prefix++;

    let ai = a.length - 1;
    let bi = b.length - 1;
    while (ai >= prefix && bi >= prefix && a[ai] === b[bi]) {
      ai--;
      bi--;
    }

    for (let k = 0; k < prefix; k++) {
      rows.push({ left: a[k], right: b[k], ln: k + 1, rn: k + 1, kind: "same" });
    }
    const middle = Math.max(ai - prefix + 1, bi - prefix + 1);
    for (let k = 0; k < middle; k++) {
      const li = prefix + k;
      const ri = prefix + k;
      const left = li <= ai ? a[li] : undefined;
      const right = ri <= bi ? b[ri] : undefined;
      rows.push({
        left,
        right,
        ln: left === undefined ? undefined : li + 1,
        rn: right === undefined ? undefined : ri + 1,
        kind:
          left !== undefined && right !== undefined
            ? left === right ? "same" : "replace"
            : left === undefined ? "add" : "del",
      });
    }
    for (let k = ai + 1, l = bi + 1; k < a.length && l < b.length; k++, l++) {
      rows.push({ left: a[k], right: b[l], ln: k + 1, rn: l + 1, kind: "same" });
    }
    return markMissingNewline(rows, oldText, newText);
  }
  const n = a.length;
  const m = b.length;
  const lcs: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }
  const rows: Row[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      rows.push({ left: a[i], right: b[j], ln: i + 1, rn: j + 1, kind: "same" });
      i++;
      j++;
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) {
      rows.push({ left: a[i], ln: i + 1, kind: "del" });
      i++;
    } else {
      rows.push({ right: b[j], rn: j + 1, kind: "add" });
      j++;
    }
  }
  for (; i < n; i++) rows.push({ left: a[i], ln: i + 1, kind: "del" });
  for (; j < m; j++) rows.push({ right: b[j], rn: j + 1, kind: "add" });
  return markMissingNewline(rows, oldText, newText);
}

export type UnifiedLine = {
  /** The conventional leading character. It is rendered, not implied by colour,
   *  so the diff still reads when the colour is gone - printed, screenshotted
   *  into a chat, or seen by a reader who does not separate red from green. */
  marker: " " | "+" | "-";
  text: string;
  ln?: number;
  rn?: number;
  tone: "same" | "add" | "del" | "meta";
};

/** Side-by-side rows flattened into one column. A replacement is the only row
 *  that becomes two lines: its old text and its new text, in that order, which
 *  is the order every other diff in the world prints them in. */
export function unified(rows: Row[]): UnifiedLine[] {
  const out: UnifiedLine[] = [];
  for (const r of rows) {
    // Metadata about the preceding line rather than a line of the file, so it
    // carries no marker and no number of its own.
    if (r.left === noNewline || r.right === noNewline) {
      out.push({ marker: " ", text: noNewline, tone: "meta" });
      continue;
    }
    if (r.kind === "same") {
      out.push({ marker: " ", text: r.left ?? r.right ?? "", ln: r.ln, rn: r.rn, tone: "same" });
      continue;
    }
    if (r.left !== undefined) {
      out.push({ marker: "-", text: r.left, ln: r.ln, tone: "del" });
    }
    if (r.right !== undefined) {
      out.push({ marker: "+", text: r.right, rn: r.rn, tone: "add" });
    }
  }
  return out;
}

const TONE: Record<UnifiedLine["tone"], string> = {
  same: "",
  add: "bg-good/16",
  del: "bg-bad/16",
  meta: "text-muted",
};

const MARKER_TONE: Record<UnifiedLine["tone"], string> = {
  same: "text-disabled",
  add: "text-good",
  del: "text-bad",
  meta: "text-muted",
};

/** One column, old and new numbered in their own gutters. Side by side needed
 *  two of every line to show one of them changing, and on the width this
 *  overlay actually gets it spent that on ellipsis. */
function Unified({ lines }: { lines: UnifiedLine[] }) {
  return (
    // No table role: the cells here are plain divs, and a table announced with
    // no rows in it is worse than preformatted text announced as itself. The
    // dialog around this already carries the file's name.
    <div className="grid min-w-max grid-cols-[auto_auto_auto_1fr] font-mono text-[12px] leading-[1.45]">
      {lines.map((l, i) => (
        <Fragment key={i}>
          {/* Muted, not disabled: a line number is read, and the disabled token
              is 3.9:1 on this canvas - a decorative contrast, not a legible
              one. */}
          <div className={cn("px-2 text-right text-muted select-none", TONE[l.tone])}>
            {l.ln ?? ""}
          </div>
          <div
            className={cn(
              "border-r border-line px-2 text-right text-muted select-none",
              TONE[l.tone],
            )}
          >
            {l.rn ?? ""}
          </div>
          <div className={cn("pl-2 select-none", TONE[l.tone], MARKER_TONE[l.tone])}>
            {l.marker}
          </div>
          <div className={cn("pr-3 pl-1 whitespace-pre", TONE[l.tone])}>{l.text}</div>
        </Fragment>
      ))}
    </div>
  );
}

/** The compact list that lives in the rail. Selecting a path opens the diff over
 *  the transcript, where there is room to read it: a side-by-side diff in a 288px
 *  column is two columns of ellipsis. */
export function ChangedFiles({
  sessionID,
  version,
  onOpen,
  openPath,
}: {
  sessionID: string;
  version: number;
  onOpen: (a: Artifact) => void;
  openPath?: string;
}) {
  const [list, setList] = useState<Artifact[]>([]);
  const [failed, setFailed] = useState(false);
  const request = useRef(0);

  const refresh = useCallback(async () => {
    const mine = ++request.current;
    try {
      const next = await api<Artifact[]>(`/api/sessions/${sessionID}/artifacts`);
      // Two writes in quick succession race; the older answer must not win.
      if (mine !== request.current) return;
      setList(next);
      setFailed(false);
    } catch {
      if (mine !== request.current) return;
      // The last good list stays. Emptying it here told the reader the agent had
      // written nothing, which is the opposite of what a failed fetch means.
      setFailed(true);
    }
  }, [sessionID]);

  useEffect(() => {
    // refresh only sets state after its await, so this is a subscription to the
    // daemon rather than the cascading render the rule is guarding against.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void refresh();
    // version counts file_changed events rather than distinct paths, so a second
    // write to a file already listed still refetches.
  }, [refresh, version]);

  const labels = useMemo(() => uniqueLabels(list.map((a) => a.path)), [list]);

  return (
    <section aria-label="Changed files" className="flex shrink-0 flex-col">
      <div className="flex shrink-0 items-center gap-2 px-3 py-2">
        <h2 className="text-[15px] font-medium">Changed</h2>
        <Badge className="ml-auto font-mono">{list.length}</Badge>
      </div>
      {failed && (
        <p className="px-3 pb-2 text-[12px] text-bad">
          {list.length === 0
            ? "Could not load the list."
            : "Could not reach the daemon; this list may be stale."}
        </p>
      )}
      {list.length === 0 && !failed && (
        <p className="px-3 pb-2 text-[12px] text-muted">Nothing written yet.</p>
      )}
      {list.length > 0 && (
        <ul className="border-t border-line-faint">
          {list.map((a) => {
            const open = a.path === openPath;
            return (
              <li key={a.path} className="border-b border-line-faint">
                <button
                  onClick={() => onOpen(a)}
                  title={a.path}
                  aria-current={open ? "true" : undefined}
                  className={cn(
                    "flex h-8 w-full items-center gap-2 pr-2 pl-0 text-left font-mono text-[12px]",
                    "[@media(pointer:coarse)]:h-11",
                    "transition-colors duration-[120ms] ease-out",
                    open ? "bg-raised text-fg" : "text-muted hover:bg-raised hover:text-fg",
                  )}
                >
                  <span
                    aria-hidden
                    className={cn("h-full w-0.5 shrink-0", open ? "bg-accent" : "bg-transparent")}
                  />
                  <span className="truncate pl-1.5">{labels.get(a.path) ?? a.path}</span>
                  {a.created && (
                    <Badge className="ml-auto shrink-0 border-good/40 text-good">new</Badge>
                  )}
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}

/** The diff itself, over the transcript rather than in place of the app, so the
 *  approval and the composer stay reachable while a file is being read. */
export function DiffView({
  sessionID,
  artifact,
  version,
  onClose,
}: {
  sessionID: string;
  artifact: Artifact;
  /** bumped on every file change, so a file written again while its diff is open
   *  is refetched rather than read at the version it had when it was opened. */
  version: number;
  onClose: () => void;
}) {
  const [full, setFull] = useState<Artifact | null>(null);
  const [error, setError] = useState<string | null>(null);
  const panel = useRef<HTMLDivElement>(null);
  const path = artifact.path;

  useEffect(() => {
    let live = true;
    void (async () => {
      try {
        const got = await api<Artifact[]>(
          `/api/sessions/${sessionID}/artifacts?path=${encodeURIComponent(path)}`,
        );
        if (!live) return;
        setFull(got[0] ?? null);
        setError(null);
      } catch (e) {
        if (live) setError(e instanceof Error ? e.message : String(e));
      }
    })();
    return () => {
      live = false;
    };
  }, [sessionID, path, version]);

  // The overlay is opaque, so what is under it must not be where the keyboard
  // lands. Once, on mount: re-running this pulled the caret out of the composer
  // the overlay deliberately leaves usable, on every render of the app.
  useEffect(() => {
    panel.current?.focus();
  }, []);

  // Held in a ref so a caller's inline arrow does not re-register the listener
  // on every render.
  const close = useRef(onClose);
  useEffect(() => {
    close.current = onClose;
  }, [onClose]);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // A drawer over this one owns Escape: it is the layer opened last.
      if (e.key === "Escape" && !document.querySelector('aside[role="dialog"]')) {
        close.current();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // Memoised: this re-renders with the app, which re-renders on every streamed
  // token, and the diff is quadratic.
  const rows = useMemo(() => (full ? diff(full.old ?? "", full.new ?? "") : []), [full]);
  const shown = useMemo(() => {
    const first = rows.slice(0, MAX_ROWS);
    const marker = rows.find((r) => r.left === noNewline || r.right === noNewline);
    // The marker is metadata about the final content line, not another line of
    // the file; keep it visible even when that line lies beyond the render cap.
    return marker && !first.includes(marker) ? [...first, marker] : first;
  }, [rows]);
  const unchanged = rows.length > 0 && rows.every((r) => r.kind === "same");
  const flat = useMemo(() => unified(shown), [shown]);

  return (
    <div
      ref={panel}
      tabIndex={-1}
      role="dialog"
      aria-label={`Diff of ${path}`}
      className="absolute inset-0 z-20 flex flex-col bg-canvas outline-none"
    >
      <div className="flex h-11 shrink-0 items-center gap-2 border-b border-line bg-panel px-3">
        <FileDiff className="h-4 w-4 shrink-0 text-muted" aria-hidden />
        <span title={path} className="truncate font-mono text-[12px]">
          {shortPath(path, 4)}
        </span>
        {artifact.created && <Badge className="shrink-0 border-good/40 text-good">new</Badge>}
        <Button
          variant="ghost"
          size="icon"
          className="ml-auto"
          aria-label="Close diff"
          onClick={onClose}
        >
          <X className="h-4 w-4" />
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-auto">
        {error && <p className="p-4 text-[13px] text-bad">{error}</p>}
        {/* Rows at the height the diff rows will be, so the panel does not jump
            when the file lands. */}
        {!error && !full && <SkeletonRows rows={12} className="gap-px p-3" />}
        {!error && full && rows.length === 0 && (
          <p className="p-4 text-[13px] text-muted">This file is empty.</p>
        )}
        {unchanged && <p className="p-3 text-[12px] text-muted">No line changed.</p>}
        {!error && rows.length > 0 && <Unified lines={flat} />}
        {rows.length > MAX_ROWS && (
          // Silent truncation reads as "that was the whole file", so the count
          // that was dropped is stated rather than implied.
          <p className="border-t border-line p-3 font-mono text-[12px] text-muted">
            Showing the first {MAX_ROWS} of {rows.length} lines.
          </p>
        )}
      </div>
    </div>
  );
}
