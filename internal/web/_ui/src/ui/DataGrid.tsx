import type { ReactNode } from 'react'

export type Column<T> = {
  key: string
  /** The header, drawn in the canvas's uppercase micro-caps. */
  header: string
  /** A CSS grid track: a fixed px column, or a minmax for the one that gives. */
  width: string
  cell: (row: T) => ReactNode
  align?: 'right'
}

type Props<T> = {
  columns: Column<T>[]
  rows: T[]
  rowKey: (row: T) => string
  onSelect?: (row: T) => void
  selected?: (row: T) => boolean
  /** Below this the table scrolls sideways rather than crushing its columns. */
  minWidth?: number
  label: string
  empty?: ReactNode
}

/**
 * The table every collection screen uses.
 *
 * It is CSS grid rather than a `<table>` because the canvas fixes the column
 * widths in pixels and lets exactly one of them give - which is a
 * `grid-template-columns` and not something a table's layout algorithm can be
 * talked into. The row is `min-height: var(--row-h)`, so the density toggle
 * changes the table without a re-render, and the header is sticky because a
 * page of models is longer than a screen.
 *
 * The roles are put back by hand: a grid of divs is invisible to a screen
 * reader otherwise, and this is a table in every sense but its layout.
 */
export function DataGrid<T>({
  columns,
  rows,
  rowKey,
  onSelect,
  selected,
  minWidth,
  label,
  empty,
}: Props<T>) {
  const template = columns.map((c) => c.width).join(' ')
  return (
    <div className="min-h-0 flex-1 overflow-auto">
      <div role="grid" aria-label={label} style={minWidth ? { minWidth: `${minWidth}px` } : undefined}>
        <div
          role="row"
          className="sticky top-0 z-[2] grid gap-3 border-b border-line bg-surface px-[18px] pt-[9px] pb-[6px] text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase"
          style={{ gridTemplateColumns: template }}
        >
          {columns.map((c) => (
            <div key={c.key} role="columnheader" className={c.align === 'right' ? 'text-right' : ''}>
              {c.header}
            </div>
          ))}
        </div>
        {rows.map((row) => {
          const isSelected = selected?.(row) ?? false
          return (
            <div
              key={rowKey(row)}
              role="row"
              aria-selected={onSelect ? isSelected : undefined}
              tabIndex={onSelect ? 0 : undefined}
              onClick={onSelect ? () => onSelect(row) : undefined}
              onKeyDown={
                onSelect
                  ? (e) => {
                      if (e.key !== 'Enter' && e.key !== ' ') return
                      e.preventDefault()
                      onSelect(row)
                    }
                  : undefined
              }
              className={`grid min-h-row items-center gap-3 border-b border-line px-[18px] ${
                onSelect ? 'cursor-default hover:bg-s0 focus-visible:bg-s0 focus-visible:outline-none' : ''
              } ${isSelected ? 'bg-s0' : ''}`}
              style={{ gridTemplateColumns: template }}
            >
              {columns.map((c) => (
                <div
                  key={c.key}
                  role="gridcell"
                  className={`min-w-0 ${c.align === 'right' ? 'text-right' : ''}`}
                >
                  {c.cell(row)}
                </div>
              ))}
            </div>
          )
        })}
      </div>
      {rows.length === 0 && empty}
    </div>
  )
}
