import { useApp } from '@/state/app'

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
  const { theme, density, running, attention, control } = useApp((s) => ({
    theme: s.theme,
    density: s.density,
    running: s.runs.filter((r) => r.running).length,
    attention: s.runs.filter((r) => r.waiting).length,
    control: s.control,
  }))

  return (
    <footer className="flex h-[24px] flex-none items-center gap-[14px] overflow-hidden border-t border-line bg-shell px-3 font-mono text-[10.5px] whitespace-nowrap text-fg-subtle">
      <span>
        {theme} · {density}
      </span>
      <span style={{ color: 'var(--running)' }}>● {running} running</span>
      <span style={{ color: 'var(--attention)' }}>! {attention} needs attention</span>
      {control !== 'open' && (
        <span style={{ color: 'var(--warning)' }} role="status">
          ◐ reconnecting
        </span>
      )}
      <span className="ml-auto">⌘K commands · ⌘J quick chat · / filter</span>
    </footer>
  )
}
