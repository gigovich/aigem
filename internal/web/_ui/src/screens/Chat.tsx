import { useMemo } from 'react'
import { ago, percent } from '@/lib/format'
import { navigate } from '@/lib/route'
import { runStatus } from '@/lib/wire'
import type { Decision, Run, RunOp } from '@/lib/wire'
import { setActiveRun, setDraft, useApp } from '@/state/app'
import { toRows } from '@/state/eventrow'
import { usePublishInspector } from '@/state/inspector'
import type { RunView } from '@/state/run'
import type { RunSocketState } from '@/lib/socket'
import { EmptyState } from '@/ui/EmptyState'
import { EventStream } from '@/ui/EventStream'
import { StatusChip } from '@/ui/StatusChip'
import { ApprovalCard } from './ApprovalCard'

type Props = {
  run: RunView
  runId: string
  state: RunSocketState
  send: (op: RunOp) => boolean
  onNew: () => void
  onClose: (id: string) => void
}

/**
 * The conversation, with the list of them beside it.
 *
 * A "session" here and a "run" are the same thing: one conversation, one
 * journal, one approval queue. The canvas draws them as two screens because in
 * a later phase a run is a ticket's autonomous execution and a session is the
 * one a person steers; in phase one only the second exists, and this is it.
 */
export function Chat({ run, runId, state, send, onNew, onClose }: Props) {
  const { runs, text } = useApp((s) => ({ runs: s.runs, text: s.draft }))
  const record = runs.find((r) => r.id === runId)

  const rows = useMemo(() => toRows(run.events), [run.events])
  const pending = run.pending[0]

  const panel = useMemo(() => {
    if (!record) return null
    return {
      kind: 'session',
      id: record.id,
      title: run.title || record.title || 'Untitled session',
      status: runStatus(record),
      fields: [
        { key: 'model', value: run.model || record.model || '—' },
        { key: 'mode', value: record.mode },
        { key: 'root', value: record.root ?? '—' },
        { key: 'journal', value: run.sessionId || record.sessionId || 'not yet written' },
        { key: 'events', value: String(run.seq) },
        { key: 'updated', value: ago(record.updated) },
      ],
      progress: run.ctxSize
        ? { used: run.contextTokens, total: run.ctxSize }
        : undefined,
      listTitle: run.files.length > 0 ? 'Files touched' : undefined,
      list: run.files.map((f) => ({
        icon: f.created ? '+' : '~',
        color: f.created ? 'var(--added)' : 'var(--modified)',
        text: f.path,
      })),
      actions: [{ label: 'Open run', onClick: () => navigate({ screen: 'run', id: record.id }) }],
    }
  }, [record, run])
  usePublishInspector(panel)

  const submit = () => {
    const value = text.trim()
    if (!value) return
    if (value.startsWith('/')) {
      const [name, ...args] = value.slice(1).split(' ')
      if (name) send({ op: 'command', name, args: args.join(' ') })
    } else {
      send({ op: 'submit', text: value })
    }
    setDraft('')
  }

  const decide = (id: string, decision: Decision) =>
    send({ op: 'resolve', id, decision, label: 'browser' })

  return (
    <div className="flex min-h-0 flex-1">
      <div className="flex w-[238px] flex-none flex-col overflow-hidden border-r border-line">
        <div className="flex flex-none items-center gap-2 border-b border-line py-[11px] pr-[10px] pl-3">
          <h2 className="m-0 text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
            Sessions
          </h2>
          <span className="font-mono text-[10px] text-fg-subtle">{runs.length}</span>
          <button
            type="button"
            onClick={onNew}
            title="New session"
            className="ml-auto h-5 cursor-pointer rounded-[5px] border border-line px-[7px] text-[10.5px] text-fg-muted hover:border-line-strong hover:text-fg"
          >
            New
          </button>
        </div>
        <div role="listbox" aria-label="Sessions" className="flex-1 overflow-y-auto py-1">
          {runs.map((r) => (
            <SessionRow
              key={r.id}
              run={r}
              active={r.id === runId}
              onOpen={() => setActiveRun(r.id)}
              onClose={() => onClose(r.id)}
            />
          ))}
          {runs.length === 0 && <EmptyState inline title="No sessions in this project yet." />}
        </div>
      </div>

      <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
        {!record ? (
          <EmptyState
            title="No session yet."
            detail="Start one to drive an agent step by step."
            action={{ label: 'New session', onClick: onNew }}
          />
        ) : (
          <>
            <div className="flex-none border-b border-line px-[18px] pt-[14px] pb-[11px]">
              <div className="flex flex-wrap items-center gap-[10px]">
                <h1 className="m-0 text-[15px] font-semibold tracking-[-0.015em]">
                  {run.title || record.title || 'Untitled session'}
                </h1>
                <StatusChip status={runStatus(record)} />
                {state !== 'open' && (
                  <span className="font-mono text-[10.5px] text-warning" role="status">
                    {state === 'gone' ? 'stream ended' : 'reconnecting'}
                  </span>
                )}
                <div className="ml-auto flex flex-none gap-[6px]">
                  <button
                    type="button"
                    onClick={() => send({ op: 'step_mode', on: !record.step })}
                    title="Pause before each tool call"
                    className="h-[26px] cursor-pointer rounded-md border px-[10px] text-[11.5px] whitespace-nowrap"
                    style={{
                      background: record.step ? 'var(--s0)' : 'transparent',
                      borderColor: record.step ? 'var(--primary)' : 'var(--border)',
                      color: record.step ? 'var(--fg)' : 'var(--fg-muted)',
                    }}
                    aria-pressed={record.step === true}
                  >
                    Step mode
                  </button>
                  {run.running && (
                    <button
                      type="button"
                      onClick={() => send({ op: 'interrupt' })}
                      className="h-[26px] cursor-pointer rounded-md border border-line px-[10px] text-[11.5px] whitespace-nowrap text-fg-muted hover:border-line-strong hover:text-fg"
                    >
                      Interrupt
                    </button>
                  )}
                  <button
                    type="button"
                    onClick={() => navigate({ screen: 'run', id: record.id })}
                    className="h-[26px] cursor-pointer rounded-md border border-line px-[10px] text-[11.5px] whitespace-nowrap text-fg-muted hover:border-line-strong hover:text-fg"
                  >
                    Open run
                  </button>
                </div>
              </div>
              {/* Every value here is something the daemon chose, and two of
                  them - a model reference and a working directory - are as long
                  as the machine makes them. Each cell is clipped on its own so
                  one long path cannot push the rest off the row. */}
              <div className="mt-[9px] flex flex-wrap gap-x-4 gap-y-[6px] font-mono text-[10.5px] text-fg-subtle">
                <Meta label="model" value={run.model || record.model || '—'} />
                <Meta label="root" value={record.root ?? '—'} />
                <Meta label="mode" value={record.mode} />
                <Meta
                  label="context"
                  value={run.ctxSize ? `${percent(run.contextTokens, run.ctxSize)}%` : '—'}
                />
              </div>
            </div>

            {rows.length === 0 ? (
              <div className="flex-1 overflow-y-auto px-[18px]">
                <div className="max-w-[84ch] py-6">
                  <div className="text-[13px] text-fg-muted">Nothing said yet.</div>
                  <div className="mt-1 max-w-[56ch] text-[12px] text-pretty text-fg-subtle">
                    Describe what the agent should do. With step mode on it will stop before every
                    tool call and wait for you.
                  </div>
                </div>
              </div>
            ) : (
              <EventStream rows={rows} label="Transcript" live={null} />
            )}

            {pending && (
              <div className="flex-none px-[18px] pb-2">
                <div className="max-w-[84ch]">
                  <ApprovalCard
                    approval={pending.approval}
                    onDecide={(d) => decide(pending.id, d)}
                    disabled={state !== 'open'}
                  />
                </div>
              </div>
            )}

            <div className="flex-none border-t border-line bg-shell px-[18px] pt-[10px] pb-3">
              <div className="max-w-[84ch]">
                <div className="flex gap-2">
                  <textarea
                    data-composer
                    rows={2}
                    value={text}
                    onChange={(e) => setDraft(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key !== 'Enter' || !(e.metaKey || e.ctrlKey)) return
                      e.preventDefault()
                      submit()
                    }}
                    placeholder="Direct the agent — constraints, corrections, next step. ⌘↵ to send."
                    aria-label="Message"
                    className="min-w-0 flex-1 resize-y rounded-md border border-line bg-bg px-[10px] py-2 text-[12.5px] leading-[1.5] outline-none focus:border-primary"
                  />
                  <button
                    type="button"
                    onClick={submit}
                    disabled={!text.trim() || state !== 'open'}
                    className="flex-none self-stretch rounded-md border border-primary bg-primary px-[14px] text-[12px] font-medium text-bg enabled:cursor-pointer enabled:hover:brightness-110 disabled:opacity-50"
                  >
                    Send
                  </button>
                </div>
              </div>
            </div>
          </>
        )}
      </div>
    </div>
  )
}

function Meta({ label, value }: { label: string; value: string }) {
  return (
    <span className="flex min-w-0 max-w-full items-baseline gap-1 whitespace-nowrap">
      {label}
      <span title={value} className="min-w-0 overflow-hidden text-ellipsis text-fg-muted">
        {value}
      </span>
    </span>
  )
}

function SessionRow({
  run,
  active,
  onOpen,
  onClose,
}: {
  run: Run
  active: boolean
  onOpen: () => void
  onClose: () => void
}) {
  const status = runStatus(run)
  return (
    <div
      role="option"
      aria-selected={active}
      tabIndex={0}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key !== 'Enter' && e.key !== ' ') return
        e.preventDefault()
        onOpen()
      }}
      className={`cursor-default border-l-2 py-[7px] pr-[10px] pl-3 hover:bg-s0 focus-visible:bg-s0 focus-visible:outline-none ${
        active ? 'border-l-primary bg-s0' : 'border-l-transparent'
      }`}
    >
      <div className="flex items-center gap-[7px]">
        <StatusChip status={status} compact />
        <span
          className="min-w-0 overflow-hidden text-[12px] text-ellipsis whitespace-nowrap"
          style={{ color: active ? 'var(--fg)' : 'var(--fg-muted)', fontWeight: active ? 500 : 400 }}
        >
          {run.title || 'Untitled session'}
        </span>
        {run.waiting && (
          <span className="flex-none rounded-[3px] border border-attention px-1 font-mono text-[9px] text-attention">
            wait
          </span>
        )}
      </div>
      <div className="mt-[2px] flex items-baseline gap-2 font-mono text-[10px] text-fg-subtle">
        <span>{run.mode}</span>
        <span className="ml-auto">{ago(run.updated)}</span>
        <button
          type="button"
          onClick={(e) => {
            e.stopPropagation()
            onClose()
          }}
          aria-label={`Close ${run.title || run.id}`}
          title="Close session"
          className="grid size-[14px] cursor-pointer place-items-center text-[11px] text-fg-subtle hover:text-danger"
        >
          <span aria-hidden="true">×</span>
        </button>
      </div>
    </div>
  )
}
