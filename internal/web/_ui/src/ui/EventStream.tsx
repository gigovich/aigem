import { memo, useEffect, useRef } from 'react'
import type { EventRow } from '@/state/eventrow'
import { Markdown } from './Markdown'
import { LiveDot } from './StatusChip'

type Props = {
  rows: EventRow[]
  /** Scroll to the newest row as it arrives. */
  follow?: boolean
  /** The line under the stream while a turn is in flight. */
  live?: { time: string; text: string } | null
  label: string
  /** Open the whole body of a tool result whose timeline form was trimmed. */
  onOpenBlob?: (seq: number) => void
}

const Row = memo(function Row({
  row: r,
  onOpenBlob,
}: {
  row: EventRow
  onOpenBlob?: (seq: number) => void
}) {
  return (
    <div
      className="grid items-baseline gap-2 px-4.5 py-0.75 hover:bg-s0"
      style={{
        gridTemplateColumns: '4.75rem 1rem 1fr',
        background: r.glyph === '!' ? 'color-mix(in oklab, var(--attention) 9%, transparent)' : undefined,
      }}
    >
      <span className="font-mono text-[0.71875rem] text-fg-subtle opacity-75">{r.time}</span>
      <span aria-hidden="true" className="text-center font-mono text-[0.75rem]" style={{ color: r.color }}>
        {r.glyph}
      </span>
      <div className="flex min-w-0 items-baseline gap-2" style={{ paddingLeft: `${r.level * 0.875}rem` }}>
        {r.prose ? (
          <Markdown source={r.text} className={r.phase ? 'min-w-0 font-medium text-fg' : 'min-w-0'} />
        ) : (
          <span
            className="overflow-hidden text-ellipsis whitespace-nowrap"
            style={{
              fontFamily: r.mono ? 'var(--mono)' : 'var(--sans)',
              fontSize: r.mono ? '0.78125rem' : '0.84375rem',
              fontWeight: r.phase ? 500 : 400,
              color: r.phase ? 'var(--fg)' : 'var(--fg-muted)',
            }}
          >
            {r.text}
          </span>
        )}
        {r.meta && <span className="flex-none font-mono text-[0.6875rem] text-fg-subtle">{r.meta}</span>}
        {r.blob !== undefined && onOpenBlob && (
          <button
            type="button"
            onClick={() => onOpenBlob(r.blob ?? 0)}
            aria-label={`Show all output of ${r.text || 'the tool'} at ${r.time}`}
            className="flex-none font-mono text-[0.6875rem] text-primary hover:underline"
          >
            show all
          </button>
        )}
      </div>
    </div>
  )
})

/**
 * The conversation, one event to a line.
 *
 * Three columns, fixed: the time, the glyph, and everything else. The
 * timestamps line up down the left because that is how a person scans for when
 * something happened, and the glyph column is what carries the state when the
 * colour cannot.
 */
export function EventStream({ rows, follow = true, live, label, onOpenBlob }: Props) {
  const end = useRef<HTMLDivElement>(null)
  const last = rows[rows.length - 1]?.key

  useEffect(() => {
    if (!follow) return
    // Guarded: jsdom's elements have no scrollIntoView, and a component that
    // threw here would take the whole screen down in every test that renders it.
    end.current?.scrollIntoView?.({ block: 'end' })
  }, [follow, last, live?.text])

  return (
    <div className="flex-1 overflow-y-auto pt-1.5 pb-10" role="log" aria-label={label}>
      {rows.map((r) => (
        <Row key={r.key} row={r} onOpenBlob={onOpenBlob} />
      ))}
      {live && (
        <div className="grid gap-2 px-4.5 py-1.25" style={{ gridTemplateColumns: '4.75rem 1rem 1fr' }}>
          <span className="font-mono text-[0.71875rem] text-fg-subtle opacity-75">{live.time}</span>
          <span className="text-center">
            <LiveDot />
          </span>
          <span className="text-fg-muted">{live.text}</span>
        </div>
      )}
      <div ref={end} />
    </div>
  )
}
