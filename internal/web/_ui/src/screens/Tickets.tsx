import { useEffect, useState } from 'react'
import { api } from '@/lib/api'
import { navigate } from '@/lib/route'
import type { Repository, Ticket } from '@/lib/wire'
import { currentProject, explain, flash, patchNewTicket, refresh, useApp } from '@/state/app'
import { TICKET_STATUS, treeRows, waitsFor } from '@/state/tickets'
import type { TicketFilter, TicketRow } from '@/state/tickets'
import { DataGrid } from '@/ui/DataGrid'
import type { Column } from '@/ui/DataGrid'
import { EmptyState } from '@/ui/EmptyState'
import { FilterInput } from '@/ui/FilterInput'
import { Modal } from '@/ui/Modal'
import { SegmentedControl } from '@/ui/SegmentedControl'

export function Tickets() {
  const { project, tickets, name, newOpen } = useApp((s) => ({
    project: s.project,
    tickets: s.tickets,
    name: currentProject(s)?.name ?? '',
    newOpen: s.newTicketOpen,
  }))
  const [filter, setFilter] = useState<TicketFilter>('active')
  const [needle, setNeedle] = useState('')
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())

  useEffect(() => {
    if (newOpen && !project) patchNewTicket(false)
  }, [newOpen, project])

  const toggle = (id: string) =>
    setCollapsed((s) => {
      const next = new Set(s)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })

  const columns: Column<TicketRow>[] = [
    {
      key: 'id',
      header: 'Id',
      width: '5.5rem',
      cell: (r) => (
        <span className="flex items-center gap-1 font-mono text-[0.75rem] text-fg-subtle">
          {r.open !== undefined && (
            <button
              type="button"
              aria-label={`${r.open ? 'Collapse' : 'Expand'} ${r.ticket.id}`}
              title={r.open ? 'Collapse' : 'Expand'}
              onKeyDown={(e) => e.stopPropagation()}
              onClick={(e) => {
                e.stopPropagation()
                toggle(r.ticket.id)
              }}
              className="text-fg-subtle hover:text-fg"
            >
              {r.open ? '▾' : '▸'}
            </button>
          )}
          <span className={r.depth ? 'pl-4' : ''}>{r.ticket.id}</span>
        </span>
      ),
    },
    {
      key: 'title',
      header: 'Title',
      width: 'minmax(12rem, 1fr)',
      cell: (r) => <TitleCell row={r} all={tickets} />,
    },
    { key: 'repo', header: 'Repo', width: '7rem', cell: (r) => r.ticket.repo || name },
    { key: 'status', header: 'Status', width: '6.5rem', cell: (r) => <TicketStatusLabel ticket={r.ticket} /> },
  ]

  return (
    <>
      <div className="flex flex-none flex-wrap items-center gap-2.5 border-b border-line px-4.5 pt-3.5 pb-3">
        <h1 className="m-0 text-[1.0625rem] font-semibold tracking-[-0.015em]">Tickets</h1>
        {project && (
          <>
            <SegmentedControl
              label="Which tickets"
              value={filter}
              onChange={setFilter}
              segments={[
                { value: 'active', label: 'Active' },
                { value: 'ready', label: 'Ready' },
                { value: 'blocked', label: 'Blocked' },
                { value: 'all', label: 'All' },
              ]}
            />
            <FilterInput value={needle} onChange={setNeedle} label="Filter tickets…" />
            <button
              type="button"
              onClick={() => patchNewTicket(true)}
              className="ml-auto h-6.5 rounded-md border border-primary bg-primary px-2.5 text-[0.78125rem] font-medium text-bg hover:brightness-110"
            >
              New ticket
            </button>
          </>
        )}
      </div>
      {!project ? (
        <EmptyState
          title="Tickets need a project."
          detail="Choose or add a project in the sidebar. A ticket belongs to a repository inside it."
        />
      ) : (
        <DataGrid
          label="Tickets"
          columns={columns}
          rows={treeRows(tickets, filter, needle, collapsed)}
          rowKey={(r) => r.ticket.id}
          onSelect={(r) => navigate({ screen: 'task', id: r.ticket.id })}
          minWidth={560}
          empty={<EmptyState title="No tickets here." detail="Create one, or choose another filter." />}
        />
      )}
      {newOpen && project && <NewTicketDialog project={project} tickets={tickets} onClose={() => patchNewTicket(false)} />}
    </>
  )
}

function TitleCell({ row, all }: { row: TicketRow; all: Ticket[] }) {
  const t = row.ticket
  const waits = t.status === 'ready' && !t.runnable ? waitsFor(t, all) : []
  return (
    <span className="flex min-w-0 items-baseline gap-2">
      <span className={`truncate ${row.depth ? '' : 'font-medium'}`}>{t.title}</span>
      {t.progress && (
        <span className="flex-none text-[0.75rem] text-fg-subtle">
          {t.progress.done}/{t.progress.total} done
        </span>
      )}
      {waits.length > 0 && (
        <span className="flex-none text-[0.75rem] text-attention">⧗ waits for {waits.join(', ')}</span>
      )}
    </span>
  )
}

export function TicketStatusLabel({ ticket }: { ticket: Ticket }) {
  const s = TICKET_STATUS[ticket.status]
  return (
    <span className="text-[0.78125rem] whitespace-nowrap" style={{ color: s.color }}>
      <span aria-hidden="true">{s.icon}</span> {s.label}
    </span>
  )
}

const INPUT =
  'rounded-md border border-line bg-bg px-2 py-1 text-[0.8125rem] text-fg outline-none focus:border-primary'

export function NewTicketDialog({
  project,
  tickets,
  parent: fixedParent,
  onClose,
}: {
  project: string
  tickets: Ticket[]
  parent?: string
  onClose: () => void
}) {
  const [repos, setRepos] = useState<Repository[]>([])
  const [repo, setRepo] = useState('')
  const [title, setTitle] = useState('')
  const [body, setBody] = useState('')
  const [parent, setParent] = useState(fixedParent ?? '')
  const [deps, setDeps] = useState<string[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const ready = title.trim() !== '' && !busy

  useEffect(() => {
    const abort = new AbortController()
    void api.projectRepos(project, abort.signal).then(setRepos, () => setRepos([]))
    return () => abort.abort()
  }, [project])

  const create = async () => {
    if (!ready) return
    setBusy(true)
    setError('')
    try {
      const t = await api.createTicket(project, {
        title: title.trim(),
        ...(repo && { repo }),
        ...(body.trim() && { body }),
        ...(parent && { parent }),
        ...(deps.length > 0 && { dependsOn: deps }),
      })
      await refresh.tickets()
      flash(`Created ${t.id}`)
      onClose()
    } catch (err) {
      setError(explain(err))
      setBusy(false)
    }
  }

  return (
    <Modal
      title="New ticket"
      onClose={onClose}
      width={560}
      confirm={{ label: busy ? 'Creating…' : 'Create ticket', onClick: () => void create(), disabled: !ready }}
    >
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          void create()
        }}
      >
        <label className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
          Title
          <input value={title} onChange={(e) => setTitle(e.target.value)} className={INPUT} />
        </label>
        <label className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
          Repository
          <select value={repo} onChange={(e) => setRepo(e.target.value)} className={INPUT}>
            <option value="">the project itself</option>
            {repos.filter((r) => r.name).map((r) => (
              <option key={r.name} value={r.name}>
                {r.name}
              </option>
            ))}
          </select>
        </label>
        {!fixedParent && (
          <label className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
            Parent
            <select value={parent} onChange={(e) => setParent(e.target.value)} className={INPUT}>
              <option value="">none (top-level)</option>
              {tickets.filter((t) => !t.parent).map((t) => (
                <option key={t.id} value={t.id}>
                  {t.id} {t.title}
                </option>
              ))}
            </select>
          </label>
        )}
        <label className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
          Waits for
          <select
            multiple
            value={deps}
            onChange={(e) => setDeps(Array.from(e.target.selectedOptions, (o) => o.value))}
            className={`${INPUT} h-24`}
          >
            {tickets.map((t) => (
              <option key={t.id} value={t.id}>
                {t.id} {t.title}
              </option>
            ))}
          </select>
        </label>
        <label className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
          Description (markdown)
          <textarea value={body} onChange={(e) => setBody(e.target.value)} rows={6} className={INPUT} />
        </label>
        {error && (
          <p role="alert" className="m-0 text-[0.78125rem] text-attention">
            {error}
          </p>
        )}
      </form>
    </Modal>
  )
}
