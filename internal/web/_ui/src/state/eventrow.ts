/**
 * One event, as a line of the stream.
 *
 * The whole vocabulary is mapped in one place. A screen that decided per event
 * what glyph to draw would end up disagreeing with the other screen that draws
 * the same stream - the chat transcript and the run screen are the same
 * renderer over the same events, and the canvas draws them the same way.
 */

import { bytes as sizeOf, clock, count } from '@/lib/format'
import { readable } from '@/lib/text'
import { EventKind } from '@/lib/wire'
import type { RunEvent } from '@/lib/wire'

export type EventRow = {
  key: string
  time: string
  glyph: string
  color: string
  text: string
  meta: string
  /** Indentation: 0 the conversation, 1 its tools, 2 a subagent's. */
  level: number
  /** A step of the conversation rather than a detail under one. */
  phase: boolean
  mono: boolean
  /** The event this row came from, for the rows that can be opened. */
  event: RunEvent
  /**
   * The sequence whose whole body the daemon kept, when this row is the head of
   * a tool result that was trimmed. `blob` is set from the write that kept it
   * and never ahead of it, so a row that admits it was trimmed without
   * promising a body is the honest answer to a state directory that could not
   * be written - and offering a fetch there would 404.
   */
  blob?: number
}

const MUTED = 'var(--fg-subtle)'

/** Events that drive state but are not steps anybody reads in a timeline. */
const SILENT = new Set<string>([
  EventKind.Content,
  EventKind.ToolBatch,
  EventKind.SessionMeta,
  EventKind.Usage,
  EventKind.Presence,
  EventKind.Desync,
])

function toolText(e: RunEvent): string {
  if (!e.args) return e.name ?? 'tool'
  // The argument line is whatever the tool was given; a path or a command is
  // the useful half and the rest is noise in a one-line row.
  const args = e.args as Record<string, unknown>
  const first = ['path', 'file_path', 'command', 'pattern', 'query'].find(
    (k) => typeof args[k] === 'string',
  )
  // Through `readable`: a path with a bidi override in it displays as one name
  // and is another, and the timeline is where a person reads back what the
  // agent actually did.
  return first ? `${e.name ?? 'tool'} ${readable(String(args[first]))}` : (e.name ?? 'tool')
}

export function toRow(e: RunEvent): EventRow | null {
  if (SILENT.has(e.kind)) return null
  const base = {
    key: String(e.seq),
    time: clock(e.time),
    level: 0,
    phase: false,
    mono: false,
    meta: '',
    event: e,
  }
  const sub = e.run_id ? 1 : 0
  switch (e.kind) {
    case EventKind.UserMessage:
      return {
        ...base,
        glyph: '›',
        color: 'var(--primary)',
        text: e.text ?? '',
        meta: e.images ? `${e.images} image${e.images > 1 ? 's' : ''}` : '',
        phase: true,
      }
    case EventKind.TurnStart:
      return { ...base, glyph: '●', color: 'var(--running)', text: 'Agent started', phase: true }
    case EventKind.TurnEnd:
      // A turn that failed says so on the event that ends it, not on a separate
      // error event - a provider that could not be dialled, a budget that ran
      // out. Drawing every turn_end as a tick is how a conversation that never
      // happened reads as one that did.
      if (e.error) {
        return { ...base, glyph: '×', color: 'var(--danger)', text: e.error, phase: true }
      }
      return {
        ...base,
        glyph: e.interrupted ? '■' : '✓',
        color: e.interrupted ? MUTED : 'var(--success)',
        text: e.interrupted ? 'Interrupted' : readable(e.text?.trim() || 'Turn finished'),
        phase: true,
      }
    case EventKind.AssistantMessage:
      return { ...base, glyph: '', color: MUTED, text: e.text ?? '' }
    case EventKind.Reasoning:
      return { ...base, glyph: '◇', color: 'var(--agent)', text: e.text ?? '', level: 1 }
    case EventKind.ToolStart:
    case EventKind.SubToolStart:
      return {
        ...base,
        glyph: '',
        color: MUTED,
        text: toolText(e),
        level: 1 + sub,
        mono: true,
      }
    case EventKind.ToolEnd:
    case EventKind.SubToolEnd:
      return {
        ...base,
        glyph: e.error ? '×' : '✓',
        color: e.error ? 'var(--danger)' : 'var(--success)',
        text: e.error ? (e.error ?? '') : (e.name ?? 'tool'),
        meta: e.bytes ? sizeOf(e.bytes) : '',
        blob: e.blob ? e.seq : undefined,
        level: 1 + sub,
        mono: true,
      }
    case EventKind.AgentStart:
      return {
        ...base,
        glyph: '◆',
        color: 'var(--agent)',
        text: `Spawned subagent: ${e.agent || e.name || 'agent'}`,
        meta: e.name ?? '',
        phase: true,
      }
    case EventKind.AgentEnd:
      return {
        ...base,
        glyph: '✓',
        color: 'var(--success)',
        text: `${e.agent || e.name || 'Subagent'} completed`,
        meta: e.tokens ? `${count(e.tokens)} tok` : '',
        level: 1,
      }
    case EventKind.SubNotice:
      return { ...base, glyph: '', color: MUTED, text: e.text ?? '', level: 2 }
    case EventKind.Notice:
      return { ...base, glyph: '·', color: MUTED, text: e.text ?? '' }
    case EventKind.Error:
      return {
        ...base,
        glyph: '×',
        color: 'var(--danger)',
        text: e.error || e.text || 'Failed',
        phase: true,
      }
    case EventKind.Todo:
      return {
        ...base,
        glyph: '≡',
        color: 'var(--primary)',
        text: 'Plan updated',
        meta: `${e.todos?.length ?? 0} items`,
        level: 1,
      }
    case EventKind.BudgetExhausted:
      return {
        ...base,
        glyph: '■',
        color: 'var(--warning)',
        text: e.text || 'The turn budget was exhausted',
        phase: true,
      }
    case EventKind.FileChanged:
      return {
        ...base,
        glyph: e.created ? '+' : '~',
        color: e.created ? 'var(--added)' : 'var(--modified)',
        text: e.path ?? '',
        meta: e.created ? 'created' : 'modified',
        level: 1,
        mono: true,
      }
    case EventKind.ApprovalRequest:
      return {
        ...base,
        glyph: '!',
        color: 'var(--attention)',
        text: `Approval required: ${e.approval?.tool ?? e.text ?? ''}`,
        phase: true,
      }
    case EventKind.ApprovalResolved:
      return {
        ...base,
        glyph: e.decision === 'deny' ? '×' : '✓',
        color: e.decision === 'deny' ? 'var(--danger)' : 'var(--success)',
        text: e.decision === 'deny' ? 'Refused' : 'Approved',
        meta: e.by ? `by ${e.by}` : '',
        level: 1,
      }
    default:
      return { ...base, glyph: '·', color: MUTED, text: e.text ?? e.kind }
  }
}

/**
 * The rows a screen draws: all of them, or only the conversation's own steps.
 *
 * The mapping itself happens once per event, in the fold - see RunView.rows.
 * This is the detail toggle and nothing else.
 */
export function visibleRows(rows: EventRow[], detail = true): EventRow[] {
  return detail ? rows : rows.filter((r) => r.level === 0)
}
