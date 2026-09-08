import { runCounts, useApp } from '@/state/app'

/**
 * The 24px footer: what the appearance is set to, what is happening, and the
 * three keys that are worth knowing.
 *
 * The control stream's state lives here too. A page whose socket is down still
 * works - every mutation is an HTTP request - but it stops learning about
 * anything a terminal or another tab does, and the person is entitled to know
 * that before they wonder why nothing is updating.
 */
export function StatusBar() {
  const { theme, density, counts, control } = useApp((s) => ({
    theme: s.theme,
    density: s.density,
    counts: runCounts(s),
    control: s.control,
  }))

  return (
    <footer className="flex h-[24px] flex-none items-center gap-[14px] overflow-hidden border-t border-line bg-shell px-3 font-mono text-[10.5px] whitespace-nowrap text-fg-subtle">
      <span>
        {theme} · {density}
      </span>
      <span style={{ color: 'var(--running)' }}>● {counts.running} running</span>
      <span style={{ color: 'var(--attention)' }}>! {counts.waiting} needs attention</span>
      {/* The region is permanent; only the sentence inside it appears. */}
      <span role="status" aria-live="polite" style={{ color: 'var(--warning)' }}>
        {control === 'open' ? '' : '◐ reconnecting'}
      </span>
      <span className="ml-auto">⌘K commands · ⌘J quick chat · / filter</span>
    </footer>
  )
}
