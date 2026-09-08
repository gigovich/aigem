/**
 * A run's timeline, folded into what the screens draw.
 *
 * The stream is the truth and this is a projection of it: every field below is
 * derived from events that have arrived, never from what this client asked for.
 * That is what makes two tabs agree - and what makes a terminal attached to the
 * same run agree with both.
 */

import { EventKind, runStatus } from '@/lib/wire'
import type { Approval, PresenceClient, Run, RunEvent, StatusKey, TodoItem } from '@/lib/wire'
import { toRow } from './eventrow'
import type { EventRow } from './eventrow'

export type PendingApproval = { id: string; approval: Approval; at: string }

export type AgentNode = {
  id: string
  name: string
  /** Depth in the delegation tree; the root conversation is 0. */
  level: number
  running: boolean
  tokens?: number
  ended?: boolean
}

export type ChangedFile = { path: string; created: boolean }

export type RunView = {
  /**
   * The timeline, and the same events already mapped to rows.
   *
   * Both are appended to in place rather than rebuilt. A run is tens of
   * thousands of events long and copying the array per event is quadratic -
   * measured at 74 seconds of pure copying for forty thousand events - and
   * mapping the whole of it to rows per event costs the same again.
   *
   * The consequence, which is the price: these two arrays are NOT new objects
   * when the view changes, so nothing may memoise on their identity. Memoise on
   * `seq`, which is what actually moves. The view object itself is replaced on
   * every event, so React still re-renders.
   */
  events: RunEvent[]
  rows: EventRow[]
  /** The last sequence applied. It is what a reconnection resumes from. */
  seq: number
  title: string
  model: string
  sessionId: string
  ctxSize: number
  contextTokens: number
  running: boolean
  interrupted: boolean
  pending: PendingApproval[]
  todos: TodoItem[]
  agents: AgentNode[]
  files: ChangedFile[]
  /**
   * How many times the run said it wrote something.
   *
   * Distinct from `files.length`, which counts paths: a conversation editing
   * one file over and over moves this and not that, and it is this that says
   * the working tree is worth reading again.
   */
  writes: number
  clients: PresenceClient[]
  /** The last error the conversation reported, for the header. */
  error: string
}

/**
 * A conversation nothing has been read into yet.
 *
 * A function and not a constant, because the arrays above are appended to: one
 * shared value would leak the first run's timeline into the second's.
 */
export function emptyRun(): RunView {
  return {
    events: [],
    rows: [],
    seq: 0,
    title: '',
    model: '',
    sessionId: '',
    ctxSize: 0,
    contextTokens: 0,
    running: false,
    interrupted: false,
    pending: [],
    todos: [],
    agents: [],
    files: [],
    writes: 0,
    clients: [],
    error: '',
  }
}

/**
 * Fold one event in, returning a new view.
 *
 * Every branch replaces only the collections it touches, so an event that adds
 * nothing but a line to the timeline leaves `pending`, `agents` and `files` as
 * the same objects - which is what keeps a component that renders one of them
 * from re-rendering on every content delta.
 */
export function apply(view: RunView, e: RunEvent): RunView {
  // Appended in place; see the note on RunView.events.
  view.events.push(e)
  const row = toRow(e)
  if (row) view.rows.push(row)
  const next: RunView = { ...view, seq: Math.max(view.seq, e.seq ?? 0) }
  switch (e.kind) {
    case EventKind.SessionMeta:
      next.sessionId = e.id ?? next.sessionId
      next.title = e.text ?? next.title
      next.model = e.name ?? next.model
      if (e.ctx) next.ctxSize = e.ctx
      break
    case EventKind.TurnStart:
      next.running = true
      next.interrupted = false
      next.error = ''
      break
    case EventKind.TurnEnd:
      next.running = false
      next.interrupted = e.interrupted === true
      // The reason a turn failed arrives on the event that ends it.
      if (e.error) next.error = e.error
      break
    case EventKind.Usage:
      if (typeof e.tokens === 'number') next.contextTokens = e.tokens
      break
    case EventKind.Todo:
      next.todos = e.todos ?? []
      break
    case EventKind.Error:
      next.error = e.error ?? e.text ?? ''
      break
    case EventKind.ApprovalRequest:
      if (e.id && e.approval) {
        next.pending = [...next.pending, { id: e.id, approval: e.approval, at: e.time }]
      }
      break
    case EventKind.ApprovalResolved:
      next.pending = next.pending.filter((p) => p.id !== e.id)
      break
    case EventKind.AgentStart:
      next.agents = [
        ...next.agents,
        {
          id: e.id ?? String(e.seq),
          name: e.agent || e.name || 'subagent',
          // A nested agent carries the parent call's id; the root's events do
          // not, which is what puts it at depth zero without a second field.
          level: e.run_id ? 2 : 1,
          running: true,
        },
      ]
      break
    case EventKind.AgentEnd:
      next.agents = next.agents.map((a) =>
        a.id === e.id ? { ...a, running: false, ended: true, tokens: e.tokens ?? a.tokens } : a,
      )
      break
    case EventKind.FileChanged:
      next.writes = next.writes + 1
      if (e.path && !next.files.some((f) => f.path === e.path)) {
        next.files = [...next.files, { path: e.path, created: e.created === true }]
      }
      break
    case EventKind.Presence:
      next.clients = e.clients ?? []
      break
    case EventKind.BudgetExhausted:
      next.running = false
      break
    default:
      break
  }
  return next
}

export function applyAll(view: RunView, events: RunEvent[]): RunView {
  return events.reduce(apply, view)
}

/**
 * A run's state as the dictionary's keys, preferring what the stream says.
 *
 * The record's `running`, `waiting` and `step` are read off the live session
 * when the record is built, and the daemon announces a record only when the
 * conversation is opened, named, switched or closed - never per turn. So a page
 * that drew the record would show a conversation that never starts and never
 * stops. The stream has the truth for the run this tab is attached to; the
 * record is what everything else has.
 */
export function liveStatus(record: Run, view: RunView): StatusKey {
  if (!record.live) return 'stopped'
  if (view.pending.length > 0) return 'attention'
  if (view.running) return 'running'
  // With events in hand the stream has said everything the record could, and
  // more recently. Falling through to the record here is how a conversation
  // whose turn has visibly finished goes on being drawn as running.
  if (view.seq > 0) return 'waiting'
  return runStatus(record)
}

/**
 * The root conversation plus whatever it delegated to, as the agent tree draws
 * it. The root is not an event - the session is always there - so it is added
 * here rather than being something the stream has to say.
 */
export function agentTree(view: RunView): AgentNode[] {
  const root: AgentNode = {
    id: 'root',
    name: view.model || 'agent',
    level: 0,
    running: view.running,
    tokens: view.contextTokens,
  }
  return [root, ...view.agents]
}
