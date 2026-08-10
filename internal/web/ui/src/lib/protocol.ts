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

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { ...(init?.headers ?? {}), Authorization: `Bearer ${token()}` },
  });
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}: ${await res.text()}`);
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}
