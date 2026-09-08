/**
 * A run's timeline, folded into what the screens draw.
 *
 * The stream is the truth and this is a projection of it: every field below is
 * derived from events that have arrived, never from what this client asked for.
 * That is what makes two tabs agree - and what makes a terminal attached to the
 * same run agree with both.
 */

import { EventKind } from '@/lib/wire'
import type { Approval, PresenceClient, RunEvent, TodoItem } from '@/lib/wire'

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
  events: RunEvent[]
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
  clients: PresenceClient[]
  /** The last error the conversation reported, for the header. */
  error: string
}

export const emptyRun: RunView = {
  events: [],
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
  clients: [],
  error: '',
}

/**
 * Fold one event in.
 *
 * It returns the previous view unchanged when nothing moved, so a stream of
 * content deltas that only append to `events` still produces one new object -
 * and never a new object for an event that changed nothing at all.
 */
export function apply(view: RunView, e: RunEvent): RunView {
  const next: RunView = { ...view, events: [...view.events, e], seq: Math.max(view.seq, e.seq ?? 0) }
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
