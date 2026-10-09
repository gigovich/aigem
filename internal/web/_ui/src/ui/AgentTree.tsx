import { count } from '@/lib/format'
import { useRoving } from './roving'
import type { AgentNode } from '@/state/run'

type Props = {
  nodes: AgentNode[]
  selected?: string
  onSelect?: (id: string) => void
}

/**
 * The root conversation and whatever it delegated to.
 *
 * The dot says running or finished and the glyph differs with it, so the state
 * survives being read without colour. It is a listbox because selecting a node
 * is what fills the panel beside it.
 */
export function AgentTree({ nodes, selected, onSelect }: Props) {
  const { container, active, setActive, onKeyDown: rovingKeys } = useRoving(nodes.length)
  return (
    <div
      ref={container}
      role="listbox"
      tabIndex={-1}
      aria-label="Agent tree"
      onKeyDown={onSelect ? rovingKeys : undefined}
      className="py-1.5"
    >
      {nodes.map((n, i) => (
        <div
          key={n.id}
          role="option"
          aria-selected={n.id === selected}
          data-roving={onSelect ? '' : undefined}
          tabIndex={onSelect ? (i === active ? 0 : -1) : undefined}
          onFocus={onSelect ? () => setActive(i) : undefined}
          onClick={onSelect ? () => onSelect(n.id) : undefined}
          onKeyDown={
            onSelect
              ? (e) => {
                  if (e.key !== 'Enter' && e.key !== ' ') return
                  e.preventDefault()
                  onSelect(n.id)
                }
              : undefined
          }
          className={`flex min-h-7 items-center gap-2 px-3 hover:bg-s0 focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary ${
            n.id === selected ? 'bg-s0' : ''
          }`}
          style={{ paddingLeft: `${0.75 + n.level * 0.875}rem` }}
        >
          <span
            aria-hidden="true"
            className="text-[0.6875rem]"
            style={{ color: n.running ? 'var(--running)' : 'var(--fg-subtle)' }}
          >
            {n.running ? '●' : '✓'}
          </span>
          <span
            className="text-[0.8125rem]"
            style={{
              color: n.id === selected ? 'var(--fg)' : 'var(--fg-muted)',
              fontWeight: n.id === selected ? 500 : 400,
            }}
          >
            {n.name}
          </span>
          <span className="sr-only">{n.running ? 'running' : 'finished'}</span>
          {n.tokens ? (
            <span className="ml-auto font-mono text-[0.6875rem] text-fg-subtle">
              {count(n.tokens)} tok
            </span>
          ) : null}
        </div>
      ))}
    </div>
  )
}
