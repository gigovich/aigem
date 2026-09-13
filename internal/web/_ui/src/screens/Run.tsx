import { useMemo, useState } from 'react'
import { clock, elapsed, tokens as tokenLabel } from '@/lib/format'
import { navigate } from '@/lib/route'
import { readable } from '@/lib/text'
import type { RunOp } from '@/lib/wire'
import type { RunSocketState } from '@/lib/socket'
import { useApp } from '@/state/app'
import { usePublishInspector } from '@/state/inspector'
import { visibleRows } from '@/state/eventrow'
import { agentTree, liveStatus, planRows } from '@/state/run'
import type { RunView } from '@/state/run'
import { SegmentedControl } from '@/ui/SegmentedControl'
import { AgentTree } from '@/ui/AgentTree'
import { EmptyState } from '@/ui/EmptyState'
import { EventStream } from '@/ui/EventStream'
import { FieldList } from '@/ui/FieldList'
import { ProgressBar } from '@/ui/ProgressBar'
import { Rows } from '@/ui/Rows'
import { StatusChip } from '@/ui/StatusChip'
import { BlobDialog } from './BlobDialog'
import { Changes } from './Changes'

type Props = {
  run: RunView
  runId: string
  state: RunSocketState
  send: (op: RunOp) => boolean
}

/**
 * One run, read rather than steered.
 *
 * It is the same event stream the chat screen draws - the same renderer over
 * the same events - with the room a transcript does not have: the agent tree,
 * the run's own fields, the context window and the files it touched.
 *
 * There is no "Stop run" button. Stopping a run is bound up with discarding the
 * worktree it was working in, and neither exists yet; a button that quietly did
 * half of that would be worse than its absence.
 */
export function Run({ run, runId, state, send }: Props) {
  const { record, narrow } = useApp((s) => ({
    record: s.runs.find((r) => r.id === runId),
    narrow: s.narrow,
  }))
  const [follow, setFollow] = useState(true)
  const [detail, setDetail] = useState(true)
  const [view, setView] = useState<'events' | 'changes'>('events')
  const [blob, setBlob] = useState<number | null>(null)
  // The tree is a listbox, and a listbox with nothing selectable is a role that
  // promises a keyboard contract it does not keep. Selecting a node narrows the
  // stream to what that agent did.
  const [agent, setAgent] = useState('root')

  // Not memoised: the fold appends to `run.rows` in place, so the array is the
  // same object from one event to the next and a memo on it would never
  // recompute. The filter itself is a walk over rows that are already built.
  const rows = visibleRows(run.rows, detail).filter(
    (r) => agent === 'root' || r.event.run_id === agent || r.event.id === agent,
  )
  const tree = useMemo(() => agentTree(run), [run])

  const fields = record
    ? [
        { key: 'mode', value: record.mode },
        { key: 'model', value: run.model || record.model || '—' },
        { key: 'status', value: record.status },
        { key: 'journal', value: run.sessionId || record.sessionId || '—' },
        { key: 'events', value: String(run.seq) },
      ]
    : []
  const files = run.files.map((f) => ({
    icon: f.created ? '+' : '~',
    color: f.created ? 'var(--added)' : 'var(--modified)',
    text: readable(f.path),
    created: f.created,
  }))

  // Below the breakpoint the column is not drawn, and what it held goes to the
  // inspector instead, which the header can open.
  const panel = useMemo(() => {
    if (!narrow || !record) return null
    return {
      kind: 'run',
      id: record.id,
      title: run.title || record.title || 'Untitled run',
      status: liveStatus(record, run),
      fields: [...fields, { key: 'root', value: record.root ?? '—' }],
      plan: planRows(run.todos),
      progress: run.ctxSize > 0 ? { used: run.contextTokens, total: run.ctxSize } : undefined,
      listTitle: files.length > 0 ? 'Files touched' : undefined,
      list: files,
      actions: [{ label: 'Steer in chat', onClick: () => navigate({ screen: 'chat', id: record.id }) }],
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- fields and files are derived from run and record
  }, [narrow, record, run])
  usePublishInspector(panel)

  if (!record) {
    return (
      <EmptyState
        title="No such run."
        detail="It may have been closed on a daemon that has since restarted."
        action={{ label: 'Back to sessions', onClick: () => navigate({ screen: 'chat' }) }}
      />
    )
  }

  return (
    <>
      <div className="flex-none border-b border-line px-[18px] pt-[14px] pb-3">
        <div className="flex flex-wrap items-center gap-[10px]">
          <span className="font-mono text-[12px] text-fg-subtle">{record.id}</span>
          <h1 className="m-0 text-[15px] font-semibold">
            {run.title || record.title || 'Untitled run'}
          </h1>
          <StatusChip status={liveStatus(record, run)} />
          <span className="font-mono text-[10.5px] text-fg-subtle">
            {elapsed(record.created)} elapsed
          </span>
          <div className="ml-auto flex gap-[6px]">
            <button
              type="button"
              onClick={() => setFollow(!follow)}
              aria-pressed={follow}
              className="h-[26px] cursor-pointer rounded-md border border-line px-[10px] text-[11.5px] text-fg-muted hover:border-line-strong hover:text-fg"
              style={{ background: follow ? 'var(--s0)' : 'transparent' }}
            >
              Follow
            </button>
            <button
              type="button"
              disabled={!run.running || state !== 'open'}
              onClick={() => send({ op: 'interrupt' })}
              className="h-[26px] rounded-md border border-line px-[10px] text-[11.5px] text-fg-muted enabled:cursor-pointer enabled:hover:border-line-strong enabled:hover:text-fg disabled:opacity-50"
            >
              Interrupt
            </button>
          </div>
        </div>
      </div>

      <div className="flex min-h-0 flex-1">
        <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
          <div className="flex flex-none items-center gap-[10px] border-b border-line px-[18px] py-2">
            <SegmentedControl
              label="What to show"
              value={view}
              onChange={setView}
              segments={[
                { value: 'events', label: 'Execution' },
                { value: 'changes', label: 'Changes' },
              ]}
            />
            {view === 'events' && (
              <button
                type="button"
                onClick={() => setDetail(!detail)}
                aria-pressed={detail}
                className="h-[22px] cursor-pointer rounded-[5px] border border-line px-2 font-mono text-[10.5px] text-fg-muted hover:border-line-strong hover:text-fg"
              >
                {detail ? 'detail' : 'phases'}
              </button>
            )}
            <span className="ml-auto font-mono text-[10.5px] text-fg-subtle">
              {view === 'events' ? `${rows.length} events` : `${run.files.length} files`}
            </span>
          </div>
          {view === 'changes' && <Changes runId={runId} seq={run.writes} />}
          {view === 'events' && (
          <EventStream
            rows={rows}
            follow={follow}
            label="Run events"
            onOpenBlob={setBlob}
            live={
              // The stamp is the last event's, not the wall clock: a component
              // that read the time as it rendered would show a different one
              // every time anything else on the page moved.
              run.running
                ? {
                    time: clock(run.events[run.events.length - 1]?.time ?? ''),
                    text: 'The agent is working…',
                  }
                : null
            }
          />
          )}
        </div>

        {/* The run's own panel, not the shell's inspector: the canvas gives
            this screen a permanent right column - agent tree, fields, context
            and working directory - beside the inspector rather than instead of
            it. Same width token, so the two line up. */}
        {!narrow && (
          <div
            className="flex-none overflow-y-auto border-l border-line bg-shell"
            style={{ width: 'var(--panel)' }}
          >
            <h2 className="m-0 border-b border-line px-3 py-[10px] text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
              Agent tree
            </h2>
            <AgentTree nodes={tree} selected={agent} onSelect={setAgent} />

            {run.todos.length > 0 && (
              <>
                <div aria-hidden="true" className="h-px bg-line" />
                <Rows title="Plan" rows={planRows(run.todos)} heading="h2" fallbackMeta="pending" />
              </>
            )}

            <div aria-hidden="true" className="h-px bg-line" />
            <div className="p-3">
              <FieldList keyWidth={78} fields={fields} />
              {run.ctxSize > 0 && (
                <div className="mt-3">
                  <ProgressBar
                    used={run.contextTokens}
                    total={run.ctxSize}
                    label={tokenLabel(run.contextTokens, run.ctxSize)}
                    title="Context window"
                  />
                </div>
              )}
            </div>

            <div aria-hidden="true" className="h-px bg-line" />
            <div className="p-3">
              <h2 className="mb-2 text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
                Working directory
              </h2>
              <div className="font-mono text-[11px] break-all text-fg-muted">
                {record.root ?? '—'}
              </div>
              <div className="mt-2 font-mono text-[11px] text-fg-subtle">
                {run.files.length} file{run.files.length === 1 ? '' : 's'} touched
              </div>
              {files.map((f) => (
                <div key={f.text} className="flex h-6 items-center gap-2 font-mono text-[11px]">
                  <span aria-hidden="true" className="w-2" style={{ color: f.color }}>
                    {f.icon}
                  </span>
                  <span className="sr-only">{f.created ? 'created' : 'modified'}</span>
                  <span className="overflow-hidden text-ellipsis whitespace-nowrap text-fg-muted">
                    {f.text}
                  </span>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
      {blob !== null && (
        <BlobDialog runId={runId} seq={blob} onClose={() => setBlob(null)} />
      )}
    </>
  )
}
