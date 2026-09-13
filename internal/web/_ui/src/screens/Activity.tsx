import { useMemo, useState } from 'react'
import { clock, hhmm } from '@/lib/format'
import { navigate } from '@/lib/route'
import type { Activity as Entry } from '@/lib/wire'
import { reveal, useApp } from '@/state/app'
import { usePublishInspector } from '@/state/inspector'
import { EmptyState } from '@/ui/EmptyState'
import { FilterInput } from '@/ui/FilterInput'

/** The glyph and colour for one feed entry, by the kind the daemon wrote. */
function mark(kind: string): { glyph: string; color: string } {
  if (kind.includes('fail') || kind.includes('error')) return { glyph: '×', color: 'var(--danger)' }
  if (kind.includes('approv') || kind.includes('trust')) {
    return { glyph: '!', color: 'var(--attention)' }
  }
  if (kind.includes('open') || kind.includes('start') || kind.includes('creat')) {
    return { glyph: '●', color: 'var(--running)' }
  }
  if (kind.includes('close') || kind.includes('stop')) {
    return { glyph: '■', color: 'var(--fg-subtle)' }
  }
  if (kind.includes('skill')) return { glyph: '◈', color: 'var(--agent)' }
  if (kind.includes('model')) return { glyph: '◇', color: 'var(--primary)' }
  return { glyph: '✓', color: 'var(--success)' }
}

const keyOf = (a: Entry) => (a.seq !== undefined ? String(a.seq) : `${a.at ?? ''}${a.text}`)

/**
 * What this daemon has done, newest first.
 *
 * It is a record rather than an audit log - the daemon trims it at thirty days
 * on startup - so the screen shows the recent end of it and does not offer a
 * way to page back into what is no longer there.
 */
export function Activity() {
  const activity = useApp((s) => s.activity)
  const [filter, setFilter] = useState('')
  const [selected, setSelected] = useState('')

  const needle = filter.trim().toLowerCase()
  const shown = needle
    ? activity.filter((a) => `${a.kind} ${a.text} ${a.runRef ?? ''}`.toLowerCase().includes(needle))
    : activity
  const chosen = activity.find((a) => keyOf(a) === selected)

  const panel = useMemo(() => {
    if (!chosen) return null
    const ref = chosen.runRef
    return {
      kind: 'activity',
      id: keyOf(chosen),
      title: chosen.text,
      fields: [
        { key: 'at', value: chosen.at ? clock(chosen.at) : '—' },
        { key: 'kind', value: chosen.kind },
        { key: 'run', value: ref ?? '—' },
      ],
      actions: ref ? [{ label: 'Open run', onClick: () => navigate({ screen: 'run', id: ref }) }] : [],
    }
  }, [chosen])
  usePublishInspector(panel)

  return (
    <>
      <div className="flex-none border-b border-line px-[18px] pt-[14px] pb-3">
        <h1 className="m-0 text-[16px] font-semibold tracking-[-0.015em]">Activity</h1>
        <p className="mt-1 mb-0 text-[12px] text-fg-muted">
          Everything this daemon did, newest first. Entries older than thirty days are dropped when
          it restarts.
        </p>
        <FilterInput value={filter} onChange={setFilter} label="Filter activity" className="mt-[10px] w-[280px]" />
      </div>
      <div className="flex-1 overflow-y-auto pt-2 pb-10">
        {shown.map((a) => {
          const m = mark(a.kind)
          const ref = a.runRef
          const key = keyOf(a)
          const active = key === selected
          return (
            <div
              key={key}
              className={`flex min-h-row items-center border-b border-line pr-[18px] hover:bg-s0 ${active ? 'bg-s0' : ''}`}
            >
              <button
                type="button"
                onClick={() => {
                  setSelected(key)
                  reveal()
                }}
                aria-current={active ? 'true' : undefined}
                className="grid min-h-row min-w-0 flex-1 cursor-default items-center gap-[10px] pl-[18px] text-left focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
                style={{ gridTemplateColumns: '62px 96px 16px minmax(0,1fr)' }}
              >
                <span className="font-mono text-[10.5px] text-fg-subtle">{a.at ? hhmm(a.at) : ''}</span>
                <span className="overflow-hidden font-mono text-[10.5px] text-ellipsis whitespace-nowrap text-fg-subtle">
                  {a.kind}
                </span>
                <span aria-hidden="true" className="text-center text-[10px]" style={{ color: m.color }}>
                  {m.glyph}
                </span>
                <span className="min-w-0 overflow-hidden text-ellipsis whitespace-nowrap text-fg-muted">
                  {a.text}
                </span>
              </button>
              <span className="w-[100px] flex-none text-right">
                {ref && (
                  <button
                    type="button"
                    onClick={() => navigate({ screen: 'run', id: ref })}
                    className="cursor-pointer font-mono text-[10.5px] text-primary hover:underline"
                  >
                    {ref}
                  </button>
                )}
              </span>
            </div>
          )
        })}
        {activity.length === 0 && <EmptyState title="Nothing has run here yet." />}
        {activity.length > 0 && shown.length === 0 && (
          <EmptyState inline title="Nothing matches that filter." />
        )}
      </div>
    </>
  )
}
