import type { Ticket, TicketStatus } from '@/lib/wire'

export type TicketRow = { ticket: Ticket; depth: 0 | 1; open?: boolean }
export type TicketFilter = 'active' | 'ready' | 'blocked' | 'all'

export const TICKET_STATUS: Record<TicketStatus, { label: string; icon: string; color: string }> = {
  open: { label: 'open', icon: '○', color: 'var(--fg-muted)' },
  planning: { label: 'planning', icon: '◇', color: 'var(--agent)' },
  review: { label: 'review', icon: '◆', color: 'var(--warning)' },
  ready: { label: 'ready', icon: '○', color: 'var(--success)' },
  running: { label: 'running', icon: '●', color: 'var(--running)' },
  blocked: { label: 'blocked', icon: '!', color: 'var(--danger)' },
  done: { label: 'done', icon: '✓', color: 'var(--fg-subtle)' },
  closed: { label: 'closed', icon: '×', color: 'var(--fg-subtle)' },
}

const MOVES: Record<TicketStatus, TicketStatus[]> = {
  open: ['ready', 'closed'],
  ready: ['open', 'done', 'closed'],
  blocked: ['open', 'ready', 'done', 'closed'],
  done: ['closed'],
  closed: ['open'],
  running: [],
  planning: [],
  review: [],
}

/** The moves the daemon lets a person make from a status. Keep in step with runner.personMove. */
export function personMoves(status: TicketStatus): TicketStatus[] {
  return MOVES[status]
}

function shown(t: Ticket, filter: TicketFilter, needle: string): boolean {
  if (needle && !`${t.id} ${t.title}`.toLowerCase().includes(needle)) return false
  if (filter === 'active') return t.status !== 'done' && t.status !== 'closed'
  if (filter === 'ready') return t.status === 'ready'
  if (filter === 'blocked') return t.status === 'blocked'
  return true
}

/** Top-level tickets in order, each followed by its shown subtickets unless collapsed. */
export function treeRows(tickets: Ticket[], filter: TicketFilter, needle: string, collapsed: Set<string>): TicketRow[] {
  const q = needle.trim().toLowerCase()
  const rows: TicketRow[] = []
  for (const top of tickets.filter((t) => !t.parent)) {
    const kids = tickets.filter((t) => t.parent === top.id && shown(t, filter, q))
    if (!shown(top, filter, q) && kids.length === 0) continue
    const open = !collapsed.has(top.id)
    rows.push({ ticket: top, depth: 0, open: kids.length > 0 ? open : undefined })
    if (open) for (const k of kids) rows.push({ ticket: k, depth: 1 })
  }
  return rows
}

export function waitsFor(t: Ticket, all: Ticket[]): string[] {
  return t.dependsOn.filter((id) => all.find((x) => x.id === id)?.status !== 'done')
}

export function blocks(t: Ticket, all: Ticket[]): Ticket[] {
  return all.filter((x) => x.dependsOn.includes(t.id))
}
