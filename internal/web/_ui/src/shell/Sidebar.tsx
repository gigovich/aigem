import { navigate } from '@/lib/route'
import type { Route, Screen } from '@/lib/route'
import type { Feature } from '@/lib/wire'
import { runCounts, useApp } from '@/state/app'
import type { AppState } from '@/state/app'

type Item = { screen: Screen; label: string; icon: string; feature?: Feature }

const TOP: Item[] = [
  { screen: 'tickets', label: 'Tickets', icon: '▤' },
  { screen: 'chat', label: 'Sessions', icon: '▭', feature: 'runs' },
  { screen: 'activity', label: 'Activity', icon: '≡', feature: 'activity' },
]

const BOTTOM: Item[] = [
  { screen: 'models', label: 'Models', icon: '◇', feature: 'models' },
  { screen: 'skills', label: 'Skills', icon: '◈', feature: 'skills' },
  { screen: 'repos', label: 'Worktrees', icon: '⌥' },
]

type Props = { route: Route; onNewProject: () => void }

/**
 * The navigation column.
 *
 * A row whose feature the daemon does not serve is not drawn at all. That is
 * the feature map doing its job: a build without a state directory has no
 * activity feed, and offering the screen would be offering one that can never
 * hold anything.
 */
function countsOf(s: AppState): Partial<Record<Screen, number>> {
  return { chat: runCounts(s).live, models: s.models.length, skills: s.skills.items.length }
}

export function Sidebar({ route, onNewProject }: Props) {
  const { narrow, features, counts } = useApp((s) => ({
    narrow: s.narrow,
    features: s.meta?.features ?? {},
    // Partial on purpose: three of the rows have nothing to count, and a full
    // record would need a zero for each - which the row would then draw.
    counts: countsOf(s),
  }))

  const row = (item: Item) => {
    if (item.feature && features[item.feature] !== true) return null
    const active = route.screen === item.screen
    const count = counts[item.screen]
    return (
      <button
        key={item.screen}
        type="button"
        aria-current={active ? 'page' : undefined}
        onClick={() => navigate({ screen: item.screen })}
        className={`flex h-[26px] w-full cursor-pointer items-center gap-2 rounded-[5px] px-2 text-left hover:bg-s0 ${
          active ? 'bg-s0 font-medium text-fg' : 'text-fg-muted'
        }`}
      >
        <span aria-hidden="true" className="w-[13px] text-center text-[11px] text-fg-subtle">
          {item.icon}
        </span>
        <span>{item.label}</span>
        {count ? (
          <span className="ml-auto font-mono text-[10px] text-fg-subtle">{count}</span>
        ) : null}
      </button>
    )
  }

  return (
    <nav
      aria-label="Navigation"
      className="flex flex-none flex-col overflow-y-auto border-r border-line bg-shell"
      style={{ width: narrow ? '168px' : '208px' }}
    >
      <div className="px-[6px] pt-2 pb-[6px]">{TOP.map(row)}</div>

      <div aria-hidden="true" className="mx-[10px] mt-[2px] mb-2 h-px bg-line" />

      <div className="px-[6px]">
        <div className="flex items-center pt-0 pr-1 pb-[5px] pl-2">
          <h2 className="m-0 text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
            Projects
          </h2>
          <button
            type="button"
            onClick={onNewProject}
            aria-label="New project"
            title="New project"
            className="ml-auto grid size-[18px] cursor-pointer place-items-center rounded-[4px] text-[13px] text-fg-subtle hover:bg-s0 hover:text-fg"
          >
            <span aria-hidden="true">+</span>
          </button>
        </div>
        {/* Phase one runs against the one directory the daemon was started in;
            projects are what phase two adds, and the button above says so. */}
        <p className="m-0 px-2 pb-1 text-[11px] text-fg-subtle">This daemon's directory.</p>
      </div>

      <div aria-hidden="true" className="mx-[10px] my-2 h-px bg-line" />

      <div className="px-[6px] pb-[10px]">{BOTTOM.map(row)}</div>

      <div className="mt-auto flex items-center gap-2 border-t border-line p-[10px]">
        <span
          aria-hidden="true"
          className="grid size-5 flex-none place-items-center rounded-[4px] bg-s1 text-[10px] font-semibold text-fg-muted"
        >
          ⌂
        </span>
        <div className="min-w-0">
          <div className="overflow-hidden text-[11.5px] text-ellipsis whitespace-nowrap">Local</div>
          <div className="font-mono text-[9.5px] text-fg-subtle">local · agent host</div>
        </div>
      </div>
    </nav>
  )
}
