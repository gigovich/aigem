import { useEffect, useState } from "react";

/** The two lines from DESIGN.md, in JS. The rail stands from the single-column
 *  breakpoint up; a side panel needs a workspace wide enough to hold a diff
 *  beside the stream, which is a good deal more. */
export const RAIL_DOCKS = "(min-width: 768px)";
export const PANEL_DOCKS = "(min-width: 1280px)";

/** useMedia tracks a media query rather than sampling it once. Sampling at
 *  mount left a resized window - or a rotated tablet, or a zoomed page - with
 *  two drawers open over the conversation and two backdrops between the reader
 *  and every control. */
export function useMedia(query: string): boolean {
  const [matches, setMatches] = useState(
    () => typeof window !== "undefined" && window.matchMedia?.(query).matches === true,
  );
  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) return;
    const mq = window.matchMedia(query);
    const sync = () => setMatches(mq.matches);
    sync();
    mq.addEventListener("change", sync);
    return () => mq.removeEventListener("change", sync);
  }, [query]);
  return matches;
}
