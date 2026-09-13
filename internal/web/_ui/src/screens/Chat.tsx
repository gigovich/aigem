import { useMemo, useState } from 'react'
import { ago, percent } from '@/lib/format'
import { navigate } from '@/lib/route'
import { runStatus } from '@/lib/wire'
import type { Decision, Run, RunOp } from '@/lib/wire'
import { setActiveRun, useApp } from '@/state/app'
import { slash } from '@/state/slash'
import { tabLabel } from '@/hooks/useRunEvents'

import { usePublishInspector } from '@/state/inspector'
import { liveStatus, planRows } from '@/state/run'
import type { RunView } from '@/state/run'
import type { RunSocketState } from '@/lib/socket'
import { Back } from '@/ui/Back'
import { EmptyState } from '@/ui/EmptyState'
import { FilterInput } from '@/ui/FilterInput'
import { EventStream } from '@/ui/EventStream'
import { StatusChip } from '@/ui/StatusChip'
import { ApprovalCard } from './ApprovalCard'
import { BlobDialog } from './BlobDialog'

type Props = {
  run: RunView
  runId: string
  state: RunSocketState
  /** Why the stream ended, when the socket could say. */
  reason: string
  send: (op: RunOp) => boolean
  /** The run the address bar names; a phone shows the list without one. */
  selected?: string
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
export function Chat({ run, runId, state, reason, send, selected, onNew, onClose }: Props) {
  const { runs, models, pendingCommand, opening, phone } = useApp((s) => ({
    runs: s.runs,
    phone: s.phone,
    // Only the ones that can actually be opened: offering a model with no
    // credential is offering a switch that comes back refused.
    models: s.models.filter((m) => !m.needsAuth || m.authenticated),
    pendingCommand: s.pendingCommand,
    opening: s.opening,
  }))
  const record = runs.find((r) => r.id === runId)
  const showList = !phone || !selected
  const showDetail = !phone || !!selected

  // The composer's own text. Kept here rather than in the application store,
  // which the shell subscribes to whole: a store that moved on every keystroke
  // would re-render the shell, the palette and the transcript per character.
  //
  // A command chosen in the palette has to land in a composer that is already
  // mounted - the ordinary case - and in one that mounts afterwards, which is
  // what choosing it from the models screen does. The first is the adjustment
  // below, during render so the text is never painted a frame late; the second
  // is the initial value.
  const [text, setText] = useState(pendingCommand.text)
  const [adopted, setAdopted] = useState(pendingCommand.nth)
  if (pendingCommand.nth !== adopted) {
    setAdopted(pendingCommand.nth)
    setText(pendingCommand.text)
  }
  const [blob, setBlob] = useState<number | null>(null)
  const [filter, setFilter] = useState('')
  const needle = filter.trim().toLowerCase()
  const shown = needle
    ? runs.filter((r) => `${r.title ?? ''} ${r.id} ${r.mode}`.toLowerCase().includes(needle))
    : runs

  const rows = run.rows
  const pending = run.pending[0]

  const panel = useMemo(() => {
    if (!record) return null
    return {
      kind: 'session',
      id: record.id,
      title: run.title || record.title || 'Untitled session',
      status: liveStatus(record, run),
      fields: [
        { key: 'model', value: run.model || record.model || '—' },
        { key: 'mode', value: record.mode },
        { key: 'root', value: record.root ?? '—' },
        {
          key: 'journal',
          value: run.sessionId || record.sessionId || 'not yet written',
        },
        { key: 'events', value: String(run.seq) },
        { key: 'updated', value: ago(record.updated) },
      ],
      progress: run.ctxSize ? { used: run.contextTokens, total: run.ctxSize } : undefined,
      plan: planRows(run.todos),
      listTitle: run.files.length > 0 ? 'Files touched' : undefined,
      list: run.files.map((f) => ({
        icon: f.created ? '+' : '~',
        color: f.created ? 'var(--added)' : 'var(--modified)',
        text: f.path,
      })),
      actions: [
        {
          label: 'Open run',
          onClick: () => navigate({ screen: 'run', id: record.id }),
        },
      ],
    }
  }, [record, run])
  usePublishInspector(panel)

  // The composer is cleared only once the socket has actually taken the
  // message. `send` drops an operation while the socket is down - queueing one
  // would replay it into a conversation the person has since left - and a
  // composer that emptied anyway would be destroying what they typed.
  const submit = () => {
    const value = text.trim()
    if (!value) return
    const sent = value.startsWith('/') ? slash(value, runId, send) : send({ op: 'submit', text: value })
    if (sent) setText('')
  }

  // Labelled with the tab, not with "browser": the point of saying who decided
  // is that the other clients can tell, and every browser tab saying the same
  // word tells them nothing.
  const decide = (id: string, decision: Decision) =>
    send({ op: 'resolve', id, decision, label: tabLabel() })

  return (
    <div className="flex min-h-0 flex-1">
      {showList && (
        <div
          className={`flex flex-none flex-col overflow-hidden border-r border-line ${phone ? 'w-full' : 'w-[238px]'}`}
        >
          <div className="flex flex-none items-center gap-2 border-b border-line py-[11px] pr-[10px] pl-3">
            <h2 className="m-0 text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
              Sessions
            </h2>
            <span className="font-mono text-[10px] text-fg-subtle">{runs.length}</span>
            <button
              type="button"
              onClick={onNew}
              disabled={opening}
              title="New session"
              className="ml-auto h-5 rounded-[5px] border border-line px-[7px] text-[10.5px] text-fg-muted enabled:cursor-pointer enabled:hover:border-line-strong enabled:hover:text-fg disabled:opacity-50"
            >
              New
            </button>
          </div>
          <div className="flex-none border-b border-line px-2 py-[6px]">
            <FilterInput
              value={filter}
              onChange={setFilter}
              label="Filter sessions"
              className="w-full"
            />
          </div>
          {/* A list of links and not a listbox: each row carries a second control
            - the close button - and an option is a leaf in the accessibility
            tree, so a button inside one is unreachable from the keyboard. */}
          <div className="flex-1 overflow-y-auto py-1">
            <ul aria-label="Sessions" className="m-0 list-none p-0">
              {shown.map((r) => (
                <SessionRow
                  key={r.id}
                  run={r}
                  active={r.id === runId}
                  // Both: the address bar so the conversation can be linked to,
                  // and the store so it survives a move to a screen that names no
                  // run of its own.
                  onOpen={() => {
                    setActiveRun(r.id)
                    navigate({ screen: 'chat', id: r.id })
                  }}
                  onClose={() => onClose(r.id)}
                />
              ))}
            </ul>
            {runs.length === 0 && <EmptyState inline title="No sessions in this project yet." />}
            {runs.length > 0 && shown.length === 0 && (
              <EmptyState inline title="Nothing matches that filter." />
            )}
          </div>
        </div>
      )}

      {showDetail && (
        <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
          {!record ? (
            <EmptyState
              title="No session yet."
              detail="Start one to drive an agent step by step."
              action={{
                label: opening ? 'Starting…' : 'New session',
                onClick: onNew,
                busy: opening,
              }}
            />
          ) : (
            <>
              <div className="flex-none border-b border-line px-[18px] pt-[14px] pb-[11px]">
                <div className="flex flex-wrap items-center gap-[10px]">
                  {phone && <Back label="Sessions" to={{ screen: 'chat' }} />}
                  <h1 className="m-0 text-[15px] font-semibold tracking-[-0.015em]">
                    {run.title || record.title || 'Untitled session'}
                  </h1>
                  <StatusChip status={liveStatus(record, run)} />
                  {/* The presence event exists for exactly this: an approval
                    blocks the turn, and knowing whether anyone else is here is
                    the difference between "thinking" and "waiting for somebody
                    who walked away". */}
                  {run.clients.length > 1 && (
                    <span
                      className="font-mono text-[10.5px] text-fg-subtle"
                      title={run.clients.map((c) => c.label || c.kind || c.id).join(', ')}
                    >
                      {run.clients.length} watching
                    </span>
                  )}
                  {/* Mounted always, with only the sentence appearing: a live
                    region inserted together with its text is announced by
                    nothing. */}
                  <span
                    className="font-mono text-[10.5px] text-warning"
                    role="status"
                    aria-live="polite"
                    aria-label="Stream"
                  >
                    {state === 'open'
                      ? ''
                      : state === 'gone'
                        ? reason || 'stream ended'
                        : 'reconnecting'}
                  </span>
                  <div className="ml-auto flex flex-none gap-[6px]">
                    <button
                      type="button"
                      // Nothing is read back: the daemon announces the changed
                      // record, and this tab applies it like any other. Asking
                      // over HTTP as well would be racing the socket this line
                      // just wrote to - the request can be answered before the
                      // operation is applied, and the answer would then arrive
                      // after the announcement and undo it.
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
                  <label className="flex min-w-0 items-baseline gap-1 whitespace-nowrap">
                    model
                    <select
                      value={run.model || record.model || ''}
                      disabled={run.running || state !== 'open'}
                      aria-label="Model for this conversation"
                      onChange={(e) => send({ op: 'switch_model', ref: e.target.value })}
                      className="min-w-0 max-w-[28ch] cursor-pointer truncate border-none bg-transparent font-mono text-[10.5px] text-fg-muted outline-none disabled:cursor-not-allowed"
                    >
                      {/* The run's own model first, because it may be one the
                        registry no longer lists - a model removed from
                        models.json does not end the conversation using it. */}
                      {!models.some((m) => m.ref === (run.model || record.model)) && (
                        <option value={run.model || record.model || ''}>
                          {run.model || record.model || '—'}
                        </option>
                      )}
                      {models.map((m) => (
                        <option key={m.ref} value={m.ref}>
                          {m.ref}
                        </option>
                      ))}
                    </select>
                  </label>
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
                <EventStream rows={rows} label="Transcript" live={null} onOpenBlob={setBlob} />
              )}

              {/* The region is permanent and only the card inside it appears:
                a live region inserted together with its content is announced by
                nothing, and this is the one moment the agent stops and waits
                for a person. */}
              <div
                role="region"
                aria-live="assertive"
                aria-label="Approval"
                className="flex-none px-[18px]"
              >
                {pending && (
                  <div className="max-w-[84ch] pb-2">
                    <ApprovalCard
                      approval={pending.approval}
                      onDecide={(d) => decide(pending.id, d)}
                      disabled={state !== 'open'}
                    />
                  </div>
                )}
              </div>

              <div className="flex-none border-t border-line bg-shell px-[18px] pt-[10px] pb-3">
                <div className="max-w-[84ch]">
                  <div className="flex gap-2">
                    <textarea
                      data-composer
                      rows={2}
                      value={text}
                      onChange={(e) => setText(e.target.value)}
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
      )}
      {blob !== null && runId && (
        <BlobDialog runId={runId} seq={blob} onClose={() => setBlob(null)} />
      )}
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
    <li
      className={`border-l-2 ${active ? 'border-l-primary bg-s0' : 'border-l-transparent'} hover:bg-s0`}
    >
      <button
        type="button"
        onClick={onOpen}
        aria-current={active ? 'true' : undefined}
        className="w-full cursor-default py-[7px] pr-[10px] pl-3 text-left focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
      >
        <span className="flex items-center gap-[7px]">
          <StatusChip status={status} compact />
          <span
            className="min-w-0 overflow-hidden text-[12px] text-ellipsis whitespace-nowrap"
            style={{
              color: active ? 'var(--fg)' : 'var(--fg-muted)',
              fontWeight: active ? 500 : 400,
            }}
          >
            {run.title || 'Untitled session'}
          </span>
          {run.waiting && (
            <span className="flex-none rounded-[3px] border border-attention px-1 font-mono text-[9px] text-attention">
              wait
            </span>
          )}
        </span>
        <span className="mt-[2px] flex items-baseline gap-2 font-mono text-[10px] text-fg-subtle">
          <span>{run.mode}</span>
          <span className="ml-auto">{ago(run.updated)}</span>
        </span>
      </button>
      <div className="flex justify-end px-[10px] pb-1">
        <button
          type="button"
          onClick={onClose}
          aria-label={`Close ${run.title || run.id}`}
          title="Close session"
          className="grid size-6 cursor-pointer place-items-center text-[11px] text-fg-subtle hover:text-danger focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
        >
          <span aria-hidden="true">×</span>
        </button>
      </div>
    </li>
  )
}
