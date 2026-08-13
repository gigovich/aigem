import { useCallback, useEffect, useState } from "react";

/** The two top-level screens one bundle serves: the session workspace that
 *  drives an agent on this machine, and the fleet's conversations. */
export type Mode = "sessions" | "chat";

/** Where each mode lives. Two paths and a back button is not a routing problem,
 *  so there is no router dependency: one would own the URL, and the URL here
 *  also carries the daemon's token on first load. */
export const CHAT_PATH = "/chat";

export function modeOf(pathname: string): Mode {
  return pathname === CHAT_PATH || pathname.startsWith(CHAT_PATH + "/") ? "chat" : "sessions";
}

function pathOf(mode: Mode): string {
  return mode === "chat" ? CHAT_PATH : "/";
}

/** useMode reads the screen from the URL and writes it back through history, so
 *  the back button leaves the mode it entered and a reload lands where the
 *  reader was.
 *
 *  The query string is carried across every push. It is empty by the time this
 *  runs - protocol.ts strips the token out of it on first read - but a push
 *  that dropped it would silently change what a link the operator copied out of
 *  the address bar is worth. */
export function useMode(): [Mode, (next: Mode) => void] {
  const [mode, setMode] = useState<Mode>(() =>
    typeof window === "undefined" ? "sessions" : modeOf(window.location.pathname),
  );

  useEffect(() => {
    const sync = () => setMode(modeOf(window.location.pathname));
    window.addEventListener("popstate", sync);
    return () => window.removeEventListener("popstate", sync);
  }, []);

  // The push happens outside the updater. A state updater must be pure - React
  // is free to run it twice, and in development it does, which stacked two
  // history entries per switch and made the back button need two presses.
  const go = useCallback((next: Mode) => {
    if (modeOf(window.location.pathname) === next) return;
    window.history.pushState({}, "", pathOf(next) + window.location.search);
    setMode(next);
  }, []);

  return [mode, go];
}

/** threadOf reads the thread a `/chat/<id>` URL names, or null for the inbox. */
export function threadOf(pathname: string): string | null {
  if (!pathname.startsWith(CHAT_PATH + "/")) return null;
  const id = pathname.slice(CHAT_PATH.length + 1);
  if (id === "") return null;
  try {
    return decodeURIComponent(id);
  } catch {
    // A malformed percent-escape - "/chat/50%" - throws here. This runs during
    // render, and there is no error boundary above it, so an unguarded decode
    // turns a mistyped URL into a blank application. The raw segment names no
    // thread and fails the ordinary way instead.
    return id;
  }
}

/** useThread puts the open thread in the URL.
 *
 *  It is what makes the phone's back button leave a thread rather than leave
 *  the app - the plan's mobile model is an inbox with threads pushed over it -
 *  and it makes a thread a link someone can send. */
export function useThread(): {
  thread: string | null;
  open: (id: string) => void;
  close: () => void;
} {
  const [thread, setThread] = useState<string | null>(() =>
    typeof window === "undefined" ? null : threadOf(window.location.pathname),
  );

  useEffect(() => {
    const sync = () => setThread(threadOf(window.location.pathname));
    window.addEventListener("popstate", sync);
    return () => window.removeEventListener("popstate", sync);
  }, []);

  const open = useCallback((id: string) => {
    if (threadOf(window.location.pathname) === id) return;
    // Stamped as ours, so close() below can tell an entry this app pushed from
    // one the operator arrived on directly.
    window.history.pushState(OURS, "", `${CHAT_PATH}/${encodeURIComponent(id)}${window.location.search}`);
    setThread(id);
  }, []);

  const close = useCallback(() => {
    if (threadOf(window.location.pathname) === null) return;
    // Pop rather than push. A push leaves the thread one hardware-back away, so
    // the phone's back button walks straight back into the thread just left -
    // which is the opposite of what a back arrow means.
    if ((window.history.state as typeof OURS | null)?.aigemThread) {
      window.history.back();
      return;
    }
    // Arrived here directly, on a shared link: there is no entry of ours to pop,
    // and popping someone else's would leave the application.
    window.history.replaceState({}, "", CHAT_PATH + window.location.search);
    setThread(null);
  }, []);

  return { thread, open, close };
}

const OURS = { aigemThread: true };

/** replaceMode rewrites the URL without adding a history entry. It is for the
 *  correction a daemon serving one mode has to make when a link points at the
 *  other: pushing there would put a screen this daemon cannot serve one back
 *  button away. */
export function replaceMode(mode: Mode) {
  // Compared by mode, not by path. A path comparison called /chat/<id> a
  // different place from /chat and rewrote it away, which on the fleet's
  // daemon - the one that serves only this mode, so the only one that reaches
  // here - destroyed every shared link to a thread on arrival.
  if (modeOf(window.location.pathname) === mode) return;
  window.history.replaceState({}, "", pathOf(mode) + window.location.search);
}
