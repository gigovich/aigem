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
  return (
    <div role="listbox" aria-label="Agent tree" className="py-[6px]">
      {nodes.map((n) => (
        <div
          key={n.id}
          role="option"
          aria-selected={n.id === selected}
          tabIndex={onSelect ? 0 : undefined}
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
          className={`flex min-h-[28px] cursor-default items-center gap-2 px-3 hover:bg-s0 focus-visible:bg-s0 focus-visible:outline-none ${
            n.id === selected ? 'bg-s0' : ''
          }`}
          style={{ paddingLeft: `${12 + n.level * 14}px` }}
        >
          <span
            aria-hidden="true"
            className="text-[10px]"
            style={{ color: n.running ? 'var(--running)' : 'var(--fg-subtle)' }}
          >
            {n.running ? '●' : '✓'}
          </span>
          <span
            className="text-[12px]"
            style={{
              color: n.id === selected ? 'var(--fg)' : 'var(--fg-muted)',
              fontWeight: n.id === selected ? 500 : 400,
            }}
          >
            {n.name}
          </span>
          <span className="sr-only">{n.running ? 'running' : 'finished'}</span>
          {n.tokens ? (
            <span className="ml-auto font-mono text-[10px] text-fg-subtle">
              {n.tokens.toLocaleString('en-US')} tok
            </span>
          ) : null}
        </div>
      ))}
    </div>
  )
}
