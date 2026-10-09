import { useEffect, useRef } from 'react'
import { setPalette } from '@/state/app'
import type { PaletteItem } from './commands'

type Props = {
  /** Already filtered and ordered by the shell, which also owns the query. */
  matches: PaletteItem[]
  query: string
  onQuery: (query: string) => void
  index: number
  onIndex: (i: number) => void
  grouped: boolean
}

/**
 * ⌘K.
 *
 * The index is owned by the shell rather than by this component: the arrow keys
 * arrive at the window, where the global keymap has to know how far the list
 * runs, and two copies of "which row is highlighted" is how a palette ends up
 * running the row above the one that looks selected.
 */
export function CommandPalette({ matches, query, onQuery, index, onIndex, grouped }: Props) {
  const input = useRef<HTMLInputElement>(null)
  const shown = matches[index]

  useEffect(() => {
    input.current?.focus()
  }, [])

  // The group headings are worked out before the render rather than by carrying
  // a variable through the map: a closure that mutates as it maps is a render
  // that depends on the order React happens to call it in.
  const withHeaders = matches.map((item, i) => ({
    item,
    header: grouped && item.group !== matches[i - 1]?.group ? item.group : '',
  }))

  return (
    <div className="fixed inset-0 z-[80] flex justify-center pt-[12vh]">
      {/* The backdrop is its own element so the panel needs no click handler of
          its own to stop the close from propagating out of it. Its keyboard
          equivalent is Escape, which the shell's keymap owns. */}
      <div
        role="presentation"
        onClick={() => setPalette(false)}
        className="absolute inset-0"
        style={{ background: 'color-mix(in oklab, var(--bg) 55%, transparent)' }}
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Command palette"
        className="relative flex max-h-[60vh] w-140 max-w-[92vw] flex-col overflow-hidden rounded-[0.625rem] border border-line-strong bg-surface shadow-panel"
        style={{ animation: 'aigem-in .12s ease-out' }}
      >
        <div className="flex h-11 items-center gap-2.25 border-b border-line px-3">
          <span aria-hidden="true" className="font-mono text-[0.8125rem] text-fg-subtle">
            ›
          </span>
          {/* The combobox pattern, declared rather than implied: without the
              role a screen reader never enters list-following mode, so the
              highlight moves visibly and silently as the arrows are pressed. */}
          <input
            ref={input}
            value={query}
            onChange={(e) => onQuery(e.target.value)}
            placeholder="Type a command or search…"
            aria-label="Type a command or search"
            role="combobox"
            aria-expanded="true"
            aria-autocomplete="list"
            aria-controls="palette-list"
            aria-activedescendant={shown ? `palette-${shown.id}` : undefined}
            className="h-10.5 flex-1 border-none bg-transparent text-[0.90625rem] outline-none"
          />
          <span
            aria-hidden="true"
            className="rounded-[0.1875rem] border border-line px-1.25 py-px font-mono text-[0.6875rem] text-fg-subtle"
          >
            esc
          </span>
        </div>

        <div id="palette-list" role="listbox" aria-label="Commands" className="flex-1 overflow-y-auto p-1.25">
          {withHeaders.map(({ item, header }, i) => {
            return (
              // Presentational: a listbox owns options, and a wrapper div
              // between them breaks that ownership.
              <div key={item.id} role="presentation">
                {header && (
                  <div
                    role="presentation"
                    className="px-2.25 pt-2 pb-1 text-[0.6875rem] font-semibold tracking-[.07em] text-fg-subtle uppercase"
                  >
                    {header}
                  </div>
                )}
                {/* The listbox is driven from the input through
                    aria-activedescendant - which is the pattern for a combobox
                    and is why an option here is deliberately not a tab stop:
                    focus stays in the field the person is typing in. */}
                {/* eslint-disable-next-line jsx-a11y/click-events-have-key-events, jsx-a11y/interactive-supports-focus */}
                <div
                  id={`palette-${item.id}`}
                  role="option"
                  aria-selected={i === index}
                  onClick={item.run}
                  onMouseEnter={() => onIndex(i)}
                  className={`flex h-8 items-center gap-2.5 rounded-md px-2.25 hover:bg-s0 ${
                    i === index ? 'bg-s0' : ''
                  }`}
                >
                  <span aria-hidden="true" className="w-3.5 text-center font-mono text-[0.75rem] text-fg-subtle">
                    {item.icon}
                  </span>
                  <span className="text-[0.84375rem] text-fg">{item.label}</span>
                  <span className="font-mono text-[0.71875rem] text-fg-subtle">{item.hint}</span>
                  <span className="ml-auto font-mono text-[0.6875rem] text-fg-subtle">{item.group}</span>
                </div>
              </div>
            )
          })}
          {matches.length === 0 && (
            <div
              role="presentation"
              className="px-2.5 py-5 text-center text-[0.8125rem] text-fg-subtle"
            >
              No matching command.
            </div>
          )}
        </div>

        <div className="flex h-7.5 items-center gap-3.5 border-t border-line px-3 font-mono text-[0.6875rem] text-fg-subtle">
          <span>↑↓ navigate</span>
          <span>↵ run</span>
          <span className="ml-auto">{matches.length}</span>
        </div>
      </div>
    </div>
  )
}
