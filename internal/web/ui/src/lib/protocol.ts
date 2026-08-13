/** The wire format, mirroring internal/uisession.Event. Flat by design: the
 *  same value is journalled, handed to the terminal, and decoded here. */
export type Kind =
  | "user_message" | "turn_start" | "turn_end"
  | "content" | "reasoning" | "assistant_message"
  | "tool_batch" | "tool_start" | "tool_end"
  | "agent_start" | "agent_end"
  | "sub_tool_start" | "sub_tool_end" | "sub_notice"
  | "notice" | "error" | "usage" | "todo" | "budget_exhausted"
  | "file_changed" | "approval_request" | "approval_resolved"
  | "session_meta" | "presence" | "desync";

export type Decision = "once" | "always" | "deny";

export interface Option { value: Decision; label: string }

export interface Approval {
  kind: "tool" | "path";
  tool: string;
  args?: unknown;
  path?: string;
  write?: boolean;
  options: Option[];
}

export interface Todo { text: string; status: string }

export interface Client { id: string; kind?: string; label?: string }

export interface Event {
  seq: number;
  time: string;
  kind: Kind;
  id?: string;
  run_id?: string;
  agent?: string;
  name?: string;
  text?: string;
  args?: unknown;
  error?: string;
  bytes?: number;
  blob?: boolean;
  round?: number;
  calls?: { id: string; name: string }[];
  tokens?: number;
  ctx?: number;
  todos?: Todo[];
  images?: number;
  injected?: boolean;
  interrupted?: boolean;
  path?: string;
  created?: boolean;
  approval?: Approval;
  decision?: Decision;
  by?: string;
  clients?: Client[];
  from?: number;
}

export interface SessionView {
  id: string;
  title?: string;
  model?: string;
  cwd?: string;
  started: string;
  running: boolean;
  seq: number;
}

/** The token arrives in the URL, in the style of jupyter, and is kept for the
 *  tab so a reload does not need it pasted again. It is deliberately not in
 *  localStorage: it authorises an agent with a shell, and it should not outlive
 *  the tab that was given it. */
export function token(): string {
  const url = new URL(window.location.href);
  const fromURL = url.searchParams.get("token");
  if (fromURL) {
    sessionStorage.setItem("aigem-token", fromURL);
    url.searchParams.delete("token");
    window.history.replaceState({}, "", url.toString());
    return fromURL;
  }
  return sessionStorage.getItem("aigem-token") ?? "";
}

/** The page trades the token for a cookie, once, before it renders anything.
 *
 *  A token in the URL is fine on loopback and wrong everywhere else: behind a
 *  reverse proxy it is in every access log on the way, and it is in the URL of
 *  every websocket this page opens. The cookie the daemon gives back is
 *  HttpOnly and SameSite=Strict, and the browser attaches it to a websocket
 *  handshake by itself - which is the whole reason the token was ever in a
 *  query string.
 *
 *  The exchange happens once per page and never rejects. A daemon that refuses
 *  it is one this page must still be able to talk to, so the failure becomes
 *  "no cookie" and the bearer token carries on being sent - which is also what
 *  every caller sees before the exchange has finished. */
let session: Promise<boolean> | null = null;
let cookie = false;

export function authenticate(): Promise<boolean> {
  session ??= fetch("/api/auth/session", {
    method: "POST",
    headers: { Authorization: `Bearer ${token()}` },
  })
    .then((res) => res.ok)
    .catch(() => false)
    .then((ok) => (cookie = ok));
  return session;
}

/** Whether this page has a cookie yet. Synchronous on purpose: it is read while
 *  a websocket is being opened, and main.tsx settles the exchange before the
 *  first render, so by then it is the answer rather than a default. */
export function hasCookie(): boolean {
  return cookie;
}

/** Forgets the exchange. For tests, which stand a fresh daemon up per case. */
export function resetAuth() {
  session = null;
  cookie = false;
}

/** Called when the daemon refuses a request this page believed was
 *  authenticated. Cookies live in the daemon's memory, so restarting the fleet
 *  revokes every one of them - and a page that latched "I have a cookie" would
 *  then never send its token again, never be issued a new cookie, and never
 *  recover short of a reload nobody knows to do.
 *
 *  Deploying is a restart, so this is the ordinary case, not the exotic one. */
function reauthenticate() {
  cookie = false;
  session = null;
}

/** The credential to send with a request. Empty once the page has a cookie -
 *  the browser attaches that itself - and the bearer token before then, or when
 *  the exchange did not take. Every request in the app goes through this or
 *  through api(), so there is one answer to "how is this authenticated". */
export function authHeaders(): Record<string, string> {
  return hasCookie() ? {} : { Authorization: `Bearer ${token()}` };
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  // The bearer header goes only while there is no cookie. Sending both would
  // leave the cookie path unexercised by anything but the socket, which is how
  // a credential that does not work ships.
  const send = () =>
    fetch(path, {
      ...init,
      headers: { ...(init?.headers ?? {}), ...authHeaders() },
    });

  let res = await send();
  if (res.status === 401 && hasCookie()) {
    // The cookie was revoked. Trade the token for a new one and retry once -
    // once, so a token that is genuinely wrong fails as an error rather than
    // looping.
    reauthenticate();
    await authenticate();
    res = await send();
  }
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}: ${await res.text()}`);
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

/** Called when a websocket closes. The handshake's status is not visible to a
 *  browser - a 401 arrives as an ordinary close - so a socket that will not
 *  stay open re-runs the exchange before its next attempt rather than
 *  reconnecting forever with a credential the daemon has forgotten.
 *
 *  It is cheap and idempotent: with a live cookie the daemon answers the
 *  exchange from the same cookie and issues nothing new. */
export function socketClosed(): Promise<boolean> {
  reauthenticate();
  return authenticate();
}

/** socketURL builds a websocket URL for a daemon path. The token goes in the
 *  query string only without a cookie, which is the one case where the
 *  handshake would otherwise carry no credential at all. */
export function socketURL(path: string, params: Record<string, string>): string {
  const proto = window.location.protocol === "https:" ? "wss" : "ws";
  const q = new URLSearchParams(params);
  if (!hasCookie()) q.set("token", token());
  return `${proto}://${window.location.host}${path}?${q.toString()}`;
}
