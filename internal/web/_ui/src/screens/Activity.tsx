import { hhmm } from '@/lib/format'
import { useApp } from '@/state/app'
import { EmptyState } from '@/ui/EmptyState'
import { navigate } from '@/lib/route'

/** The glyph and colour for one feed entry, by the kind the daemon wrote. */
function mark(kind: string): { glyph: string; color: string } {
  if (kind.includes('fail') || kind.includes('error')) return { glyph: '×', color: 'var(--danger)' }
  if (kind.includes('approv') || kind.includes('trust')) {
    return { glyph: '!', color: 'var(--attention)' }
  }
  if (kind.includes('open') || kind.includes('start')) {
    return { glyph: '●', color: 'var(--running)' }
  }
  if (kind.includes('close') || kind.includes('stop')) {
    return { glyph: '■', color: 'var(--fg-subtle)' }
  }
  if (kind.includes('skill')) return { glyph: '◈', color: 'var(--agent)' }
  if (kind.includes('model')) return { glyph: '◇', color: 'var(--primary)' }
  return { glyph: '✓', color: 'var(--success)' }
}

/**
 * What this daemon has done, newest first.
 *
 * It is a record rather than an audit log - the daemon trims it at thirty days
 * on startup - so the screen shows the recent end of it and does not offer a
 * way to page back into what is no longer there.
 */
export function Activity() {
  const activity = useApp((s) => s.activity)

  return (
    <>
      <div className="flex-none border-b border-line px-[18px] pt-[14px] pb-3">
        <h1 className="m-0 text-[16px] font-semibold tracking-[-0.015em]">Activity</h1>
        <p className="mt-1 mb-0 text-[12px] text-fg-muted">
          Everything this daemon did, newest first. Entries older than thirty days are dropped when
          it restarts.
        </p>
      </div>
      <div className="flex-1 overflow-y-auto pt-2 pb-10">
        {activity.map((a) => {
          const m = mark(a.kind)
          return (
            <div
              key={a.seq ?? `${a.at ?? ''}${a.text}`}
              className="grid min-h-row items-center gap-[10px] border-b border-line px-[18px] hover:bg-s0"
              style={{ gridTemplateColumns: '62px 96px 16px minmax(0,1fr) 100px' }}
            >
              <span className="font-mono text-[10.5px] text-fg-subtle">
                {a.at ? hhmm(a.at) : ''}
              </span>
              <span className="overflow-hidden font-mono text-[10.5px] text-ellipsis whitespace-nowrap text-fg-subtle">
                {a.kind}
              </span>
              <span aria-hidden="true" className="text-center text-[10px]" style={{ color: m.color }}>
                {m.glyph}
              </span>
              <span className="min-w-0 overflow-hidden text-ellipsis whitespace-nowrap text-fg-muted">
                {a.text}
              </span>
              {a.runRef ? (
                <button
                  type="button"
                  onClick={() => navigate({ screen: 'run', id: a.runRef! })}
                  className="cursor-pointer text-right font-mono text-[10.5px] text-primary hover:underline"
                >
                  {a.runRef}
                </button>
              ) : (
                <span />
              )}
            </div>
          )
        })}
        {activity.length === 0 && <EmptyState title="Nothing has happened here yet." />}
      </div>
    </>
  )
}
