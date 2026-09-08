import { shortVersion } from '@/lib/format'
import { runCounts } from '@/state/app'
import { LiveDot } from '@/ui/StatusChip'
import { setPalette, setQuick, toggleDensity, toggleInspector, toggleTheme, useApp } from '@/state/app'
import { navigate } from '@/lib/route'
import type { Route } from '@/lib/route'

type Props = { crumbs: { label: string; route?: Route }[] }

/**
 * The 38px header: the product on the left in a column the width of the
 * sidebar, then the command button, the breadcrumbs, and the controls.
 *
 * Below the narrow breakpoint the breadcrumbs and the run pill go, because the
 * command button and the four controls are what a person actually reaches for
 * and they are what has to survive.
 */
export function Header({ crumbs }: Props) {
  const { version, theme, density, narrow, quickOpen, inspectorOpen, counts } = useApp((s) => ({
    version: s.meta?.version ?? '',
    theme: s.theme,
    density: s.density,
    narrow: s.narrow,
    quickOpen: s.quickOpen,
    inspectorOpen: s.inspectorOpen,
    counts: runCounts(s),
  }))

  return (
    <header className="flex h-[38px] flex-none items-stretch border-b border-line bg-shell">
      <div
        className="flex flex-none items-center gap-2 border-r border-line px-[10px]"
        style={{ width: 'var(--rail)' }}
      >
        <span aria-hidden="true" className="size-[15px] flex-none rounded-[3px] bg-agent opacity-90" />
        <span className="font-semibold tracking-[-0.01em]">Aigem</span>
        <span
          title={version}
          className="ml-auto min-w-0 overflow-hidden font-mono text-[10px] text-ellipsis whitespace-nowrap text-fg-subtle"
        >
          {shortVersion(version)}
        </span>
      </div>

      <div className="flex min-w-0 flex-1 items-center gap-[10px] px-3">
        <button
          type="button"
          onClick={() => setPalette(true)}
          className="flex h-[24px] flex-none cursor-pointer items-center gap-[7px] rounded-[5px] border border-line bg-surface pr-2 pl-[7px] text-fg-subtle hover:border-line-strong hover:text-fg-muted"
        >
          <span className="text-[11px]">Search or run a command</span>
          <span
            aria-hidden="true"
            className="rounded-[3px] border border-line px-1 py-px font-mono text-[10px]"
          >
            ⌘K
          </span>
        </button>

        {!narrow && (
          <nav aria-label="Breadcrumb" className="ml-1 flex items-center gap-[2px]">
            {crumbs.map((c, i) => {
              const to = c.route
              return (
              <button
                key={c.label}
                type="button"
                disabled={!to}
                onClick={to ? () => navigate(to) : undefined}
                className="flex h-[24px] items-center gap-[6px] rounded-[4px] px-[7px] font-mono text-[11px] text-fg-muted enabled:cursor-pointer enabled:hover:bg-s0 enabled:hover:text-fg"
              >
                <span>{c.label}</span>
                {i < crumbs.length - 1 && (
                  <span aria-hidden="true" className="text-fg-subtle opacity-50">
                    /
                  </span>
                )}
              </button>
              )
            })}
          </nav>
        )}

        <div className="ml-auto flex flex-none items-center gap-[6px]">
          {!narrow && (
            <div className="flex h-[22px] items-center gap-[6px] rounded-full border border-line px-2 font-mono text-[10.5px] whitespace-nowrap text-fg-muted">
              <LiveDot />
              <span>
                {counts.running} running · {counts.live} open
              </span>
            </div>
          )}
          <button
            type="button"
            onClick={() => setQuick(!quickOpen)}
            title="Quick chat  ⌘J"
            className="flex h-[24px] cursor-pointer items-center gap-[6px] rounded-[5px] border border-line px-[9px] text-[11.5px] hover:border-line-strong hover:text-fg"
            style={{
              background: quickOpen ? 'var(--s0)' : 'transparent',
              color: quickOpen ? 'var(--fg)' : 'var(--fg-muted)',
            }}
          >
            <span aria-hidden="true" className="font-mono text-[10px]">
              ▭
            </span>
            <span>Chat</span>
          </button>
          <button
            type="button"
            onClick={toggleInspector}
            title="Inspector"
            aria-pressed={inspectorOpen}
            aria-label={`Inspector: ${inspectorOpen ? 'shown' : 'hidden'}`}
            className="grid size-[24px] cursor-pointer place-items-center rounded-[5px] border border-line text-[11px] text-fg-muted hover:border-line-strong hover:text-fg"
          >
            <span aria-hidden="true">▤</span>
          </button>
          <button
            type="button"
            onClick={toggleDensity}
            title="Density"
            aria-label={`Density: ${density === 'dense' ? 'dense' : 'comfy'}`}
            className="h-[24px] cursor-pointer rounded-[5px] border border-line px-2 font-mono text-[10.5px] text-fg-muted hover:border-line-strong hover:text-fg"
          >
            {density === 'dense' ? 'dense' : 'comfy'}
          </button>
          <button
            type="button"
            onClick={toggleTheme}
            title="Theme"
            aria-label={`Theme: ${theme}`}
            className="grid size-[24px] cursor-pointer place-items-center rounded-[5px] border border-line text-[11px] text-fg-muted hover:border-line-strong hover:text-fg"
          >
            <span aria-hidden="true">{theme === 'mocha' ? '◐' : '◑'}</span>
          </button>
        </div>
      </div>
    </header>
  )
}
