import { shortVersion } from '@/lib/format'
import { runCounts } from '@/state/app'
import { LiveDot } from '@/ui/StatusChip'
import {
  setNav,
  setPalette,
  setQuick,
  toggleInspector,
  toggleTheme,
  useApp,
} from '@/state/app'
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
  const { version, theme, narrow, phone, quickOpen, inspectorOpen, counts } = useApp(
    (s) => ({
      version: s.meta?.version ?? '',
      theme: s.theme,
      narrow: s.narrow,
      phone: s.phone,
      quickOpen: s.quickOpen,
      inspectorOpen: s.inspectorOpen,
      counts: runCounts(s),
    }),
  )

  return (
    <header className="flex h-9.5 flex-none items-stretch border-b border-line bg-shell">
      <div
        className="flex flex-none items-center gap-2 border-r border-line px-2.5"
        style={{ width: phone ? 'auto' : 'var(--rail)' }}
      >
        {phone ? (
          <button
            type="button"
            onClick={() => setNav(true)}
            aria-label="Open navigation"
            className="grid size-6 place-items-center rounded-[0.3125rem] border border-line text-[0.875rem] text-fg-muted hover:border-line-strong hover:text-fg"
          >
            <span aria-hidden="true">☰</span>
          </button>
        ) : (
          <span
            aria-hidden="true"
            className="size-3.75 flex-none rounded-[0.1875rem] bg-agent opacity-90"
          />
        )}
        <span className="font-semibold tracking-[-0.01em]">Aigem</span>
        {!phone && (
          <span
            title={version}
            className="ml-auto min-w-0 overflow-hidden font-mono text-[0.6875rem] text-ellipsis whitespace-nowrap text-fg-subtle"
          >
            {shortVersion(version)}
          </span>
        )}
      </div>

      <div className="flex min-w-0 flex-1 items-center gap-2.5 px-3">
        <button
          type="button"
          onClick={() => setPalette(true)}
          className="flex h-6 min-w-0 flex-none items-center gap-1.75 rounded-[0.3125rem] border border-line bg-surface pr-2 pl-1.75 text-fg-subtle hover:border-line-strong hover:text-fg-muted"
        >
          <span className="overflow-hidden text-[0.75rem] text-ellipsis whitespace-nowrap">
            {phone ? 'Search' : 'Search or run a command'}
          </span>
          <span
            aria-hidden="true"
            className="rounded-[0.1875rem] border border-line px-1 py-px font-mono text-[0.6875rem]"
          >
            ⌘K
          </span>
        </button>

        {!narrow && (
          <nav aria-label="Breadcrumb" className="ml-1 flex items-center gap-0.5">
            {crumbs.map((c, i) => {
              const to = c.route
              return (
                <button
                  key={c.label}
                  type="button"
                  disabled={!to}
                  onClick={to ? () => navigate(to) : undefined}
                  className="flex h-6 items-center gap-1.5 rounded-[0.25rem] px-1.75 font-mono text-[0.75rem] text-fg-muted enabled:hover:bg-s0 enabled:hover:text-fg"
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

        <div className="ml-auto flex flex-none items-center gap-1.5">
          {!narrow && (
            <div className="flex h-5.5 items-center gap-1.5 rounded-full border border-line px-2 font-mono text-[0.71875rem] whitespace-nowrap text-fg-muted">
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
            className="flex h-6 items-center gap-1.5 rounded-[0.3125rem] border border-line px-2.25 text-[0.78125rem] hover:border-line-strong hover:text-fg"
            style={{
              background: quickOpen ? 'var(--s0)' : 'transparent',
              color: quickOpen ? 'var(--fg)' : 'var(--fg-muted)',
            }}
          >
            <span aria-hidden="true" className="font-mono text-[0.6875rem]">
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
            className="grid size-6 place-items-center rounded-[0.3125rem] border border-line text-[0.75rem] text-fg-muted hover:border-line-strong hover:text-fg"
          >
            <span aria-hidden="true">▤</span>
          </button>
          <button
            type="button"
            onClick={toggleTheme}
            title="Theme"
            aria-label={`Theme: ${theme}`}
            className="grid size-6 place-items-center rounded-[0.3125rem] border border-line text-[0.75rem] text-fg-muted hover:border-line-strong hover:text-fg"
          >
            <span aria-hidden="true">{theme === 'mocha' ? '◐' : '◑'}</span>
          </button>
        </div>
      </div>
    </header>
  )
}
