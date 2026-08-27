import type { Event, Todo } from "./protocol";

/** The wire format of internal/chat. Every type here mirrors a Go struct in
 *  that package; the field names are its JSON tags, not renamed on the way in,
 *  so a change on either side is a type error rather than an empty column. */

/** Thread states, as the store derives them. Never guessed here: `working` is a
 *  turn row with no end, `needs_you` is a bot's message flagged await. */
export type ThreadState = "needs_you" | "working" | "waiting" | "idle";

/** Actor ids carry their kind, so a bot named "operator" cannot collide with
 *  the human. */
export const OPERATOR = "human:operator";

export interface Actor {
  id: string;
  kind: "human" | "bot";
  name: string;
  role?: string;
  present: boolean;
  created: string;
}

/** What only the process running the bots can report. It is absent on a member
 *  nobody reported one for - the operator, and any bot this daemon does not
 *  run - and the screen says so rather than drawing a stopped bot. */
export interface LiveBot {
  running: boolean;
  model?: string;
  /** The wake-up interval in force ("30m"), and how far the idle backoff has
   *  walked from the working cadence. */
  heartbeat?: string;
  tier: number;
  next_job?: string;
  next_run?: string;
}

/** What a bot is doing, in one word. Decided by the daemon, never derived here:
 *  two clients draw this roster, and a state added there must not be invisible
 *  in one of them until its bundle is rebuilt. Empty for anyone who is not a
 *  bot. */
export type FleetState = "working" | "idle" | "stopped";

/** A roster row: the actor, what the store can count about it, and what the
 *  daemon adds. */
export interface FleetMember extends Actor {
  state?: FleetState;
  threads: number;
  working: boolean;
  live?: LiveBot;
}

export interface ModelOption {
  ref: string;
  name: string;
  provider: string;
  usable: boolean;
  reason?: string;
}

export interface BotModelSettings {
  name: string;
  role: string;
  configured?: string;
  selected: string;
  source: "configured" | "role-default";
  running?: string;
  restart_required: boolean;
}

export interface BotModels {
  options: ModelOption[];
  bots: BotModelSettings[];
}

export interface ThreadView {
  id: string;
  title?: string;
  created: string;
  created_by: string;
  last_seq: number;
  last_at: string;
  last_author?: string;
  last_text?: string;
  state: ThreadState;
  archived?: boolean;
  participants: string[];
  unread: number;
  working: boolean;
}

export interface Attachment {
  id: string;
  thread: string;
  message?: number;
  filename: string;
  mime: string;
  size: number;
  sha256: string;
  created: string;
}

export interface Message {
  seq: number;
  thread: string;
  author: string;
  body: string;
  kind: "message" | "system" | "handoff";
  mentions?: string[];
  await?: boolean;
  created: string;
  attachments?: string[];
  /** The run this message came out of, or absent for one said outside any -
   *  which is every message the operator writes. It is what hangs a collapsed
   *  trace under the answer that run produced. */
  turn?: number;
}

/** One bot's run inside a thread, and what it did. The counts are kept on the
 *  row by the daemon rather than derived here: the collapsed summary is drawn
 *  for every bot message on screen, and computing it would mean pulling a long
 *  thread's whole timeline to render a hundred one-line summaries. */
export interface Turn {
  seq: number;
  thread: string;
  actor: string;
  started: string;
  ended?: string;
  usage?: Usage;
  model?: string;
  error?: string;
  steps?: number;
  tools?: number;
  files?: number;
  /** The working plan as this turn left it. Absent on a turn that never wrote
   *  one; the panel shows the newest turn that did. */
  plan?: Todo[];
}

export interface Usage {
  input_tokens?: number;
  cached_tokens?: number;
  output_tokens?: number;
  calls?: number;
  uncounted?: number;
}

/** What a thread's work has cost, summed over its runs. `turns` counts the runs
 *  that reached the provider - the right denominator beside a cost - and `runs`
 *  counts every one of them, which is the number to put beside a list of who
 *  ran what. */
export interface Spend {
  usage?: Usage;
  turns: number;
  runs: number;
  models?: string[];
}

/** A turn's changed files are typed as `Artifact` in `components/files.tsx`,
 *  not here: one pair of components draws them for both daemons, and the type
 *  that pair consumes is the union of the two shapes. A second mirror in this
 *  file would be a wire type with no reader, which is how the last five got
 *  written. */

/** The streams a frame can belong to. `desync` and `truncated` call for
 *  opposite reactions and are deliberately distinct: a client that confused
 *  them would throw away the backlog it had just been handed. */
export type Stream = "message" | "thread" | "event" | "desync" | "truncated";

export interface Frame {
  seq: number;
  stream: Stream;
  /** Set on every frame. A thread frame carrying no `thr` is a tombstone: the
   *  conversation is gone and there is nothing left to describe. */
  thread?: string;
  msg?: Message;
  thr?: ThreadView;
  event?: Event;
  /** The run an event frame belongs to. Not inside the event: a uisession.Event
   *  knows nothing about threads or runs. */
  turn?: number;
  /** The resume point on a desync or a truncated backlog. */
  from?: number;
}

/** A rejection of one client's op. Not a Frame, and deliberately so: frames are
 *  what happened in the conversation, and a bad request did not happen in it. */
export interface ClientError {
  kind: "client_error";
  op?: string;
  error: string;
}

export type Incoming = Frame | ClientError;

export function isClientError(v: Incoming): v is ClientError {
  return (v as ClientError).kind === "client_error";
}

/** Everything the operator can do to the fleet's conversations. */
export interface ClientOp {
  op: "send" | "watch" | "unwatch" | "read" | "add" | "remove" | "title" | "archive" | "ping";
  thread?: string;
  text?: string;
  actor?: string;
  seq?: number;
  title?: string;
  mentions?: string[];
  attachments?: string[];
  archived?: boolean;
}

/** A bounded slice of a stream plus where to resume it. Cursor is only set when
 *  More is: a route that stopped at its limit has to say so, because a bare
 *  array reads exactly like the end of the conversation. */
export interface Page<T> {
  items: T[];
  cursor?: number;
  more?: boolean;
}

/** What the daemon says about itself before the page draws anything. Served
 *  rather than compiled in: a browser holding yesterday's bundle against
 *  today's binary is the ordinary case, and a limit copied into the client is a
 *  limit that drifts until an upload starts failing. */
export interface ChatMeta {
  operator: string;
  states: ThreadState[];
  max_body_bytes: number;
  max_title_chars: number;
  max_unread: number;
  max_attachment_bytes?: number;
  max_attachments?: number;
  inline_image_types?: string[];
}

/** What each state is called on screen. One map, because the rail's filter
 *  chips and the rows they filter must agree - two copies is how a chip comes
 *  to say something the list under it does not. A state the daemon grows that
 *  is not here still appears, under its own name. */
const STATE_LABEL: Record<string, string> = {
  needs_you: "needs you",
  working: "working",
  waiting: "waiting",
  idle: "idle",
};

export function stateLabel(state: string): string {
  return STATE_LABEL[state] ?? state;
}

/** clock renders the time something last happened. Date-less: every row in an
 *  inbox is either today or old enough that the time of day says nothing, and a
 *  date belongs in the thread itself. */
export function clock(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  return at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
}

/** firstLine is a one-line stand-in for a body: the fallback label for a thread
 *  with no title, now that the row has no room for a preview of its own. */
export function firstLine(text: string, max = 80): string {
  const line = text.split("\n", 1)[0].trim();
  return line.length > max ? line.slice(0, max).trimEnd() + "…" : line;
}

/** displayName strips the kind off an actor id. "you" for the operator, because
 *  a transcript addressed to the reader in the third person reads as someone
 *  else's log. */
export function displayName(actorID: string, operator = OPERATOR): string {
  if (actorID === operator) return "you";
  const at = actorID.indexOf(":");
  return at < 0 ? actorID : actorID.slice(at + 1);
}
