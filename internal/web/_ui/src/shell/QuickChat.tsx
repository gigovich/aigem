import { useEffect, useRef, useState } from 'react'
import { navigate } from '@/lib/route'
import { EventKind } from '@/lib/wire'
import { setActiveRun, setQuick } from '@/state/app'
import type { RunView } from '@/state/run'

type Props = {
  run: RunView
  runId: string
  /** Reports whether the socket took it; false leaves the text where it is. */
  onSubmit: (text: string) => boolean
  ready: boolean
}

/**
 * ⌘J: the same conversation as the chat screen, in a corner.
 *
 * It is deliberately not a second agent. A quick question that turns out to
 * matter should be continuable, and "Open session" is that - the transcript is
 * already the run's, so continuing it is a navigation and not a hand-off.
 */
export function QuickChat({ run, runId, onSubmit, ready }: Props) {
  const [text, setText] = useState('')
  const feed = useRef<HTMLDivElement>(null)

  const rows = run.events.filter(
    (e) =>
      e.kind === EventKind.UserMessage ||
      (e.kind === EventKind.AssistantMessage && !!e.text?.trim()),
  )
  const last = rows[rows.length - 1]?.seq

  useEffect(() => {
    if (feed.current) feed.current.scrollTop = feed.current.scrollHeight
  }, [last])

  const send = () => {
    const value = text.trim()
    if (!value || !runId) return
    if (onSubmit(value)) setText('')
  }

  return (
    <div
      className="fixed right-3.5 bottom-8.5 z-[70] flex max-h-[62vh] w-93 max-w-[calc(100vw-1.75rem)] flex-col overflow-hidden rounded-[0.5625rem] border border-line-strong bg-surface shadow-panel"
      style={{ animation: 'aigem-in .12s ease-out' }}
      role="dialog"
      aria-label="Quick chat"
    >
      <div className="flex flex-none items-center gap-2 border-b border-line px-2.75 py-2.25">
        <span className="text-[0.84375rem] font-semibold">Quick chat</span>
        <span className="font-mono text-[0.6875rem] text-fg-subtle">
          {runId ? run.title || runId : 'no session'}
        </span>
        <button
          type="button"
          disabled={!runId}
          onClick={() => {
            setQuick(false)
            setActiveRun(runId)
            navigate({ screen: 'chat', id: runId })
          }}
          title="Continue in the session"
          className="ml-auto h-5.25 rounded-[0.3125rem] border border-line px-2 text-[0.71875rem] text-fg-subtle enabled:hover:border-line-strong enabled:hover:text-fg disabled:opacity-50"
        >
          Open session
        </button>
        <button
          type="button"
          onClick={() => setQuick(false)}
          aria-label="Close quick chat"
          className="grid size-5 place-items-center rounded-[0.25rem] text-[0.875rem] text-fg-subtle hover:bg-s0 hover:text-fg"
        >
          <span aria-hidden="true">×</span>
        </button>
      </div>

      <div ref={feed} className="flex-1 overflow-y-auto px-2.75 py-2.5">
        {rows.length === 0 && (
          <p className="m-0 text-[0.8125rem] text-fg-subtle">
            {!runId
              ? 'Start a session to ask anything.'
              : ready
                ? 'Nothing said yet.'
                : 'This conversation is not connected.'}
          </p>
        )}
        {rows.map((e) => {
          const mine = e.kind === EventKind.UserMessage
          return (
            <div key={e.seq} className="py-1.5">
              <div
                className="font-mono text-[0.65625rem] tracking-[.06em] uppercase"
                style={{ color: mine ? 'var(--primary)' : 'var(--agent)' }}
              >
                {mine ? 'you' : 'agent'}
              </div>
              <div
                className="mt-0.75 text-[0.84375rem] text-pretty"
                style={{ color: mine ? 'var(--fg)' : 'var(--fg-muted)' }}
              >
                {e.text}
              </div>
            </div>
          )
        })}
      </div>

      <div className="flex-none border-t border-line bg-shell px-2.75 py-2.25">
        <div className="flex gap-1.75">
          <input
            value={text}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key !== 'Enter') return
              e.preventDefault()
              send()
            }}
            disabled={!runId || !ready}
            placeholder="Ask anything, or say what to do…"
            aria-label="Ask anything, or say what to do"
            className="h-7 min-w-0 flex-1 rounded-md border border-line bg-bg px-2.25 text-[0.8125rem] outline-none focus:border-primary disabled:opacity-50"
          />
          <button
            type="button"
            onClick={send}
            disabled={!runId || !ready || !text.trim()}
            className="h-7 flex-none rounded-md border border-line bg-s0 px-2.75 text-[0.78125rem] enabled:hover:bg-s1 disabled:opacity-50"
          >
            Send
          </button>
        </div>
      </div>
    </div>
  )
}
