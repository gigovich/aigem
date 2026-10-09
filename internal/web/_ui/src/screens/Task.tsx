import { useState } from 'react'
import { api } from '@/lib/api'
import { ago } from '@/lib/format'
import { format, navigate } from '@/lib/route'
import type { Run, Ticket, TicketStatus } from '@/lib/wire'
import { currentProject, explain, refresh, setBanner, useApp } from '@/state/app'
import { blocks, personMoves, waitsFor } from '@/state/tickets'
import { Back } from '@/ui/Back'
import { EmptyState } from '@/ui/EmptyState'
import { Markdown } from '@/ui/Markdown'
import { SegmentedControl } from '@/ui/SegmentedControl'
import { INPUT, NewTicketDialog, TicketStatusLabel } from './Tickets'

const MOVE_LABEL: Record<TicketStatus, string> = {
  open: 'Back to open',
  ready: 'Mark ready',
  done: 'Mark done',
  closed: 'Close',
  planning: '',
  review: '',
  running: '',
  blocked: '',
}

const BUTTON =
  'h-6.5 rounded-md border border-line px-2.5 text-[0.78125rem] text-fg-muted hover:border-line-strong hover:text-fg'

export function Task({ id = '' }: { id?: string }) {
  const { project, tickets, runs, loaded, name } = useApp((s) => ({
    project: s.project,
    tickets: s.tickets,
    runs: s.runs,
    loaded: s.ticketsLoaded,
    name: currentProject(s)?.name ?? '',
  }))
  const t = tickets.find((x) => x.id === id)
  const [tab, setTab] = useState<'overview' | 'discussion' | 'runs'>('overview')
  const [error, setError] = useState('')
  const [adding, setAdding] = useState(false)
  const [busy, setBusy] = useState(false)

  if (!project) return <EmptyState title="Tasks need a project." detail="Choose a project in the sidebar." />
  if (!t && !loaded) return <p className="m-0 px-4.5 py-3.5 text-fg-subtle">Loading tickets…</p>
  if (!t) {
    return (
      <EmptyState
        title="No such ticket."
        detail={`${id} is not a ticket of ${name}.`}
        action={{ label: 'Back to tickets', onClick: () => navigate({ screen: 'tickets' }) }}
      />
    )
  }

  const parent = tickets.find((x) => x.id === t.parent)
  const kids = tickets.filter((x) => x.parent === t.id)
  const isParent = kids.length > 0
  const waits = waitsFor(t, tickets)

  const change = async (patch: { status?: TicketStatus; dependsOn?: string[] }) => {
    setError('')
    try {
      await api.updateTicket(project, t.id, patch)
      await refresh.tickets()
    } catch (err) {
      setError(explain(err))
    }
  }

  const act = async (call: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await call()
      await Promise.all([refresh.tickets(), refresh.runs()])
    } catch (err) {
      setError(explain(err))
    } finally {
      setBusy(false)
    }
  }
  const lastRun = t.runs[t.runs.length - 1]
  const lastLive = runs.some((r) => r.id === lastRun && r.live)

  return (
    <>
      <div className="flex-none border-b border-line px-4.5 pt-3.5 pb-3">
        <div className="flex flex-wrap items-center gap-2.5">
          <Back
            label={t.parent || 'Tickets'}
            to={t.parent ? { screen: 'task', id: t.parent } : { screen: 'tickets' }}
          />
          <span className="font-mono text-[0.8125rem] text-fg-subtle">{t.id}</span>
          <h1 className="m-0 text-[1rem] font-semibold">{t.title}</h1>
          <TicketStatusLabel ticket={t} />
          {waits.length > 0 && (
            <span className="text-[0.75rem] text-attention">⧗ waits for {waits.join(', ')}</span>
          )}
          {!isParent && (
            <div className="ml-auto flex gap-1.5">
              {t.runnable && (
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void act(() => api.runTicket(project, t.id))}
                  className={BUTTON}
                >
                  Run
                </button>
              )}
              {lastRun && lastLive && (
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void act(() => api.stopRun(lastRun))}
                  className={BUTTON}
                >
                  Stop
                </button>
              )}
              {t.status === 'blocked' && t.mergePending && (
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void act(() => api.mergeTicket(project, t.id))}
                  className={BUTTON}
                >
                  Retry merge
                </button>
              )}
              {personMoves(t.status).map((to) => (
                <button key={to} type="button" onClick={() => void change({ status: to })} className={BUTTON}>
                  {MOVE_LABEL[to]}
                </button>
              ))}
            </div>
          )}
        </div>
        <div className="mt-1.5 font-mono text-[0.71875rem] text-fg-subtle">
          {t.repo || name} · created by {t.by || 'you'}
          {t.created && ` · created ${ago(t.created)}`}
          {t.updated && ` · updated ${ago(t.updated)}`}
        </div>
        {error && (
          <p role="alert" className="m-0 mt-1.5 text-[0.78125rem] text-attention">
            {error}
          </p>
        )}
      </div>

      <div className="flex min-h-0 flex-1">
        <div className="flex min-w-0 flex-1 flex-col overflow-y-auto px-4.5 py-2.5">
          <SegmentedControl
            label="What to show"
            value={tab}
            onChange={setTab}
            segments={[
              { value: 'overview', label: 'Overview' },
              { value: 'discussion', label: `Discussion ${t.comments.length}` },
              { value: 'runs', label: `Runs ${t.runs.length}` },
            ]}
          />
          {tab === 'overview' && (
            <div className="mt-3">
              {t.body ? <Markdown source={t.body} /> : <p className="text-fg-subtle">No description.</p>}
              {!t.parent && (
                <div className="mt-4">
                  <div className="mb-1.5 flex items-center gap-2">
                    <span className="text-[0.6875rem] tracking-[.07em] text-fg-subtle uppercase">Subtickets</span>
                    {isParent && (
                      <span className="text-[0.75rem] text-fg-subtle">
                        {t.progress?.done ?? 0}/{t.progress?.total ?? kids.length} done
                      </span>
                    )}
                    <button type="button" onClick={() => setAdding(true)} className={`ml-auto ${BUTTON}`}>
                      Add subticket
                    </button>
                  </div>
                  {kids.map((k) => (
                    <button
                      key={k.id}
                      type="button"
                      onClick={() => navigate({ screen: 'task', id: k.id })}
                      className="flex w-full items-center gap-2.5 border-b border-line py-1.5 text-left hover:bg-s0"
                    >
                      <span className="font-mono text-[0.75rem] text-fg-subtle">{k.id}</span>
                      <span className="flex-1 truncate">{k.title}</span>
                      <TicketStatusLabel ticket={k} />
                    </button>
                  ))}
                </div>
              )}
            </div>
          )}
          {tab === 'discussion' && <Discussion project={project} ticket={t} />}
          {tab === 'runs' && <TicketRuns ids={t.runs} />}
        </div>

        <aside
          className="flex-none overflow-y-auto border-l border-line bg-shell px-3.5 py-3 text-[0.75rem]"
          style={{ width: 'var(--panel)' }}
        >
          {t.parent && (
            <Section title="Parent">
              <TicketLink id={t.parent} all={tickets} />
              {parent?.progress && (
                <div className="mt-0.5 text-fg-subtle">
                  {parent.progress.done} of {parent.progress.total} done
                </div>
              )}
            </Section>
          )}
          {isParent && (
            <Section title="Progress">
              {t.progress?.done ?? 0} of {t.progress?.total ?? kids.length} done
            </Section>
          )}
          <Section title="Waits for">
            {t.dependsOn.map((d) => (
              <div key={d} className="flex items-center gap-2 py-0.5">
                <TicketLink id={d} all={tickets} />
                <button
                  type="button"
                  aria-label={`Stop waiting for ${d}`}
                  title="Remove dependency"
                  onClick={() => void change({ dependsOn: t.dependsOn.filter((x) => x !== d) })}
                  className="ml-auto text-fg-subtle hover:text-danger"
                >
                  ×
                </button>
              </div>
            ))}
            <label className="mt-1 flex flex-col gap-1 text-fg-subtle">
              Add dependency
              <select
                value=""
                onChange={(e) => e.target.value && void change({ dependsOn: [...t.dependsOn, e.target.value] })}
                className="rounded-md border border-line bg-bg px-1.5 py-0.5 text-fg"
              >
                <option value="">choose a ticket…</option>
                {tickets
                  .filter((x) => x.id !== t.id && x.id !== t.parent && x.parent !== t.id && !t.dependsOn.includes(x.id))
                  .map((x) => (
                    <option key={x.id} value={x.id}>
                      {x.id} {x.title}
                    </option>
                  ))}
              </select>
            </label>
          </Section>
          <Section title="Blocks">
            {blocks(t, tickets).map((b) => (
              <div key={b.id} className="py-0.5">
                <TicketLink id={b.id} all={tickets} />
              </div>
            ))}
          </Section>
          <Section title="Repository">{t.repo || name}</Section>
        </aside>
      </div>
      {adding && (
        <NewTicketDialog project={project} tickets={tickets} parent={t.id} onClose={() => setAdding(false)} />
      )}
    </>
  )
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="mb-3">
      <div className="mb-1 text-[0.65625rem] tracking-[.06em] text-fg-subtle uppercase">{title}</div>
      {children}
    </div>
  )
}

function TicketLink({ id, all }: { id: string; all: Ticket[] }) {
  const t = all.find((x) => x.id === id)
  return (
    <span className="inline-flex items-center gap-1.5">
      <a
        href={format({ screen: 'task', id })}
        onClick={(e) => {
          e.preventDefault()
          navigate({ screen: 'task', id })
        }}
        className="rounded-[0.1875rem] border border-line-strong px-1 font-mono text-[0.6875rem] text-fg-muted"
      >
        {id}
      </a>
      {t && <TicketStatusLabel ticket={t} />}
    </span>
  )
}

function runState(r?: Run): string {
  if (!r) return 'deleted'
  if (r.running) return 'running'
  return r.live ? 'live' : r.status
}

function TicketRuns({ ids }: { ids: string[] }) {
  const runs = useApp((s) => s.runs)
  if (ids.length === 0) return <p className="mt-3 text-fg-subtle">No runs yet. Run starts one on a runnable ticket.</p>
  return (
    <ul aria-label="Runs" className="m-0 mt-3 list-none p-0">
      {[...ids].reverse().map((id) => {
        const r = runs.find((x) => x.id === id)
        return (
          <li key={id} className="flex items-center gap-2.5 border-b border-line py-1.5">
            <a
              href={format({ screen: 'run', id })}
              onClick={(e) => {
                e.preventDefault()
                navigate({ screen: 'run', id })
              }}
              className="font-mono text-[0.75rem] text-fg-muted hover:text-fg"
            >
              {id}
            </a>
            <span className="flex-1 truncate">{r?.title ?? ''}</span>
            <span className="font-mono text-[0.71875rem] text-fg-subtle">{runState(r)}</span>
          </li>
        )
      })}
    </ul>
  )
}

function Discussion({ project, ticket }: { project: string; ticket: Ticket }) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const send = async () => {
    if (!text.trim() || busy) return
    setBusy(true)
    try {
      await api.commentTicket(project, ticket.id, text)
      setText('')
      await refresh.tickets()
    } catch (err) {
      setBanner(explain(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="mt-3">
      {ticket.comments.map((c, i) => (
        <div key={i} className="border-b border-line py-2">
          <div
            className="font-mono text-[0.6875rem]"
            style={{ color: c.by === 'you' ? 'var(--primary)' : 'var(--agent)' }}
          >
            {c.by}
          </div>
          <Markdown source={c.text} />
        </div>
      ))}
      <div className="mt-2 flex gap-2">
        <textarea
          aria-label="Comment"
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) void send()
          }}
          rows={3}
          placeholder="Write a comment… ⌘↵ to send"
          className={`flex-1 ${INPUT}`}
        />
        <button type="button" onClick={() => void send()} disabled={!text.trim() || busy} className={BUTTON}>
          Send comment
        </button>
      </div>
    </div>
  )
}
