import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, test, vi } from 'vitest'
import { DataGrid } from './DataGrid'
import type { Column } from './DataGrid'

type Row = { id: string; name: string; n: number }

const rows: Row[] = [
  { id: 'a', name: 'Alpha', n: 1 },
  { id: 'b', name: 'Beta', n: 2 },
]

const columns: Column<Row>[] = [
  { key: 'name', header: 'Model', width: 'minmax(160px,1.4fr)', cell: (r) => r.name },
  { key: 'n', header: 'Context', width: '72px', cell: (r) => String(r.n), align: 'right' },
]

const grid = (over: Partial<Parameters<typeof DataGrid<Row>>[0]> = {}) => (
  <DataGrid label="Models" columns={columns} rows={rows} rowKey={(r) => r.id} {...over} />
)

// A grid of divs is invisible to a screen reader without the roles put back by
// hand, and this is a table in every sense but its layout.
test('is a table to anything reading the page', () => {
  render(grid())
  expect(screen.getByRole('grid', { name: 'Models' })).toBeInTheDocument()
  expect(screen.getAllByRole('columnheader').map((h) => h.textContent)).toEqual([
    'Model',
    'Context',
  ])
  // Two rows plus the header.
  expect(screen.getAllByRole('row')).toHaveLength(3)
  expect(screen.getAllByRole('gridcell')).toHaveLength(4)
})

// The canvas fixes the columns in pixels and lets exactly one give, which is a
// grid template and not something a table's layout can be talked into. The
// header and the rows have to use the same one or nothing lines up.
test('draws the header and the rows on one column template', () => {
  render(grid())
  const template = 'minmax(160px,1.4fr) 72px'
  for (const row of screen.getAllByRole('row')) {
    expect(row.style.gridTemplateColumns).toBe(template)
  }
})

test('rows are selectable by mouse and by keyboard', async () => {
  const user = userEvent.setup()
  const onSelect = vi.fn()
  render(grid({ onSelect, selected: (r) => r.id === 'b' }))

  await user.click(screen.getByText('Alpha'))
  expect(onSelect).toHaveBeenCalledWith(rows[0])

  const [, first] = screen.getAllByRole('row')
  first?.focus()
  await user.keyboard('{Enter}')
  await user.keyboard(' ')
  expect(onSelect).toHaveBeenCalledTimes(3)

  expect(screen.getAllByRole('row')[2]).toHaveAttribute('aria-selected', 'true')
})

// A table nothing can select must not advertise a selection or take a tab stop:
// tabbing through a read-only table is how a keyboard user loses the page.
test('a table with no selection is not focusable and claims no selected row', () => {
  render(grid())
  const [, first] = screen.getAllByRole('row')
  expect(first).not.toHaveAttribute('tabindex')
  expect(first).not.toHaveAttribute('aria-selected')
})

test('shows what it was given for empty instead of an empty table', () => {
  render(grid({ rows: [], empty: <p>Nothing here.</p> }))
  expect(screen.getByText('Nothing here.')).toBeInTheDocument()
  expect(screen.getAllByRole('row')).toHaveLength(1)
})

// Below the minimum the table scrolls sideways rather than crushing columns the
// canvas fixed in pixels.
test('holds its minimum width so the columns cannot be crushed', () => {
  render(grid({ minWidth: 720 }))
  expect(screen.getByRole('grid')).toHaveStyle({ minWidth: '720px' })
})
