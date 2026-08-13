import { useCallback, useSyncExternalStore } from "react";

/** The two top-level screens one bundle serves: the session workspace that
 *  drives an agent on this machine, and the fleet's conversations. */
export type Mode = "sessions" | "chat";

/** Where each mode lives. Two paths and a back button is not a routing problem,
 *  so there is no router dependency: one would own the URL, and the URL here
 *  also carries the daemon's token on first load. */
export const CHAT_PATH = "/chat";

/** The fleet screen's path. It sits under the chat mode because that is the
 *  daemon it describes, and it cannot be mistaken for a thread: a thread id is
 *  `t_` followed by hex. */
export const FLEET_PATH = CHAT_PATH + "/fleet";

// ---- the one reader of the URL ----
//
// Every screen hook below derives from this rather than keeping a copy. Copies
// only agree until one of them pushes: pushState fires no event, so a hook that
// listened for popstate alone kept rendering the screen the operator had just
// left - the inbox behind an open roster, the roster behind an open thread.
//
// So the two writers here notify explicitly, and popstate covers the back
// button and anything outside this module.

const readers = new Set<() => void>();

function subscribe(onChange: () => void): () => void {
  readers.add(onChange);
  window.addEventListener("popstate", onChange);
  return () => {
    readers.delete(onChange);
    window.removeEventListener("popstate", onChange);
  };
}

function moved() {
  for (const r of [...readers]) r();
}

function push(path: string, state: unknown = {}) {
  window.history.pushState(state, "", path);
  moved();
}

function replace(path: string, state: unknown = {}) {
  window.history.replaceState(state, "", path);
  moved();
}

/** usePathname is the current path, kept in step with every way it can change. */
function usePathname(): string {
  return useSyncExternalStore(
    subscribe,
    () => window.location.pathname,
    () => "/",
  );
}

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
  const mode = modeOf(usePathname());

  const go = useCallback((next: Mode) => {
    if (modeOf(window.location.pathname) === next) return;
    push(pathOf(next) + window.location.search);
  }, []);

  return [mode, go];
}

/** threadOf reads the thread a `/chat/<id>` URL names, or null for the inbox. */
export function threadOf(pathname: string): string | null {
  if (pathname === FLEET_PATH) return null;
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
  const thread = threadOf(usePathname());

  const open = useCallback((id: string) => {
    if (threadOf(window.location.pathname) === id) return;
    // Stamped as ours, so close() below can tell an entry this app pushed from
    // one the operator arrived on directly.
    push(`${CHAT_PATH}/${encodeURIComponent(id)}${window.location.search}`, OURS);
  }, []);

  const close = useCallback(() => {
    if (threadOf(window.location.pathname) === null) return;
    leave();
  }, []);

  return { thread, open, close };
}

/** useFleetScreen puts the roster in the URL, on the same terms as a thread: a
 *  screen the back button leaves rather than a mode the application is in, and
 *  a link someone can send. */
export function useFleetScreen(): { fleet: boolean; open: () => void; close: () => void } {
  const fleet = usePathname() === FLEET_PATH;

  const open = useCallback(() => {
    if (window.location.pathname === FLEET_PATH) return;
    push(FLEET_PATH + window.location.search, OURS);
  }, []);

  const close = useCallback(() => {
    if (window.location.pathname !== FLEET_PATH) return;
    leave();
  }, []);

  return { fleet, open, close };
}

/** leave goes back to the inbox from a screen pushed over it.
 *
 *  It pops rather than pushes. A push leaves the screen just closed one
 *  hardware-back away, so the phone's back button walks straight back into it -
 *  the opposite of what a back arrow means. Arriving on a shared link is the
 *  case with no entry of ours to pop, and popping someone else's would leave
 *  the application entirely. */
function leave() {
  if ((window.history.state as typeof OURS | null)?.aigemThread) {
    window.history.back();
    return;
  }
  replace(CHAT_PATH + window.location.search);
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
  replace(pathOf(mode) + window.location.search);
}
