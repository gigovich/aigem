import { useCallback, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { api } from '@/lib/api'
import type { Dir } from '@/lib/wire'
import { explain } from '@/state/app'

type Node = { dirs: Dir[]; truncated?: boolean } | { error: string }

type Props = {
  /** The chosen path relative to the root; '' is the root itself, null is nothing yet. */
  selected: string | null
  /** git is whether the directory is a repository; unknown for the root. */
  onSelect: (path: string, git?: boolean) => void
}

const ROW = 'flex h-6 w-full items-center gap-1.5 rounded-[0.3125rem] px-1.5 text-left hover:bg-s0'

/** The directories under the daemon's root, each read when it is first opened. */
export function DirTree({ selected, onSelect }: Props) {
  const [root, setRoot] = useState('')
  const [nodes, setNodes] = useState<Record<string, Node>>({})
  const [open, setOpen] = useState<Set<string>>(() => new Set())
  const abort = useRef<AbortController | null>(null)

  const load = useCallback(async (path: string) => {
    const signal = abort.current?.signal
    try {
      const listing = await api.dirs(path, signal)
      setRoot(listing.root)
      setNodes((n) => ({ ...n, [path]: { dirs: listing.dirs, truncated: listing.truncated } }))
    } catch (err) {
      if (signal?.aborted) return
      setNodes((n) => ({ ...n, [path]: { error: explain(err) } }))
    }
  }, [])

  useEffect(() => {
    abort.current = new AbortController()
    void load('')
    return () => abort.current?.abort()
  }, [load])

  const toggle = (path: string) => {
    const next = new Set(open)
    if (next.has(path)) {
      next.delete(path)
    } else {
      next.add(path)
      const node = nodes[path]
      if (!node || 'error' in node) void load(path)
    }
    setOpen(next)
  }

  const note = (depth: number, text: string, alert = false) => (
    <li
      role={alert ? 'alert' : undefined}
      style={{ paddingLeft: `${depth * 1.125 + 1.75}rem` }}
      className={`py-0.5 text-[0.75rem] ${alert ? 'text-attention' : 'text-fg-subtle'}`}
    >
      {text}
    </li>
  )

  const branch = (path: string, depth: number): ReactNode => {
    const node = nodes[path]
    if (!node) return note(depth, 'loading…')
    if ('error' in node) return note(depth, node.error, true)
    if (node.dirs.length === 0) return note(depth, 'no sub-directories')
    return (
      <>
        {node.dirs.map((d) => {
          const p = path ? `${path}/${d.name}` : d.name
          const expanded = open.has(p)
          return (
            <li key={p}>
              <div className="flex items-center" style={{ paddingLeft: `${depth * 1.125}rem` }}>
                <button
                  type="button"
                  aria-label={`${expanded ? 'Collapse' : 'Expand'} ${d.name}`}
                  aria-expanded={expanded}
                  onClick={() => toggle(p)}
                  className="grid size-5 flex-none place-items-center rounded text-fg-subtle hover:bg-s0"
                >
                  <span aria-hidden="true">{expanded ? '▾' : '▸'}</span>
                </button>
                <button
                  type="button"
                  aria-pressed={selected === p}
                  onClick={() => onSelect(p, !!d.git)}
                  className={`${ROW} ${selected === p ? 'bg-s0 font-medium text-fg' : 'text-fg-muted'}`}
                >
                  <span className="min-w-0 flex-1 overflow-hidden text-ellipsis whitespace-nowrap">{d.name}</span>
                  {d.git && <span className="text-[0.6875rem] text-fg-subtle">git</span>}
                </button>
              </div>
              {expanded && <ul className="m-0 list-none p-0">{branch(p, depth + 1)}</ul>}
            </li>
          )
        })}
        {node.truncated && note(depth, 'more not shown')}
      </>
    )
  }

  return (
    <div className="max-h-72 overflow-auto rounded-md border border-line bg-bg p-1 font-mono text-[0.8125rem]">
      <button
        type="button"
        aria-pressed={selected === ''}
        onClick={() => onSelect('')}
        title={root}
        className={`${ROW} ${selected === '' ? 'bg-s0 font-medium text-fg' : 'text-fg-muted'}`}
      >
        <span className="min-w-0 flex-1 overflow-hidden text-ellipsis whitespace-nowrap">{root || '…'}</span>
      </button>
      <ul aria-label="Directories" className="m-0 list-none p-0">
        {branch('', 0)}
      </ul>
    </div>
  )
}
