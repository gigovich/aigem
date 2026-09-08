import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, test, vi } from 'vitest'
import { useRoving } from './roving'

/**
 * The hook is exercised through a widget rather than a hook harness, because
 * what it promises is about focus and the tab order - properties of a rendered
 * document and not of a returned value.
 */
function Rows({ count, onPick = vi.fn() }: { count: number; onPick?: (i: number) => void }) {
  const { container, active, setActive, onKeyDown } = useRoving(count)
  return (
    <div role="listbox" aria-label="Rows" tabIndex={-1} ref={container} onKeyDown={onKeyDown}>
      {Array.from({ length: count }, (_, i) => (
        <div
          key={i}
          role="option"
          aria-selected={false}
          data-roving=""
          tabIndex={i === active ? 0 : -1}
          onFocus={() => setActive(i)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') onPick(i)
          }}
          onClick={() => onPick(i)}
        >
          {`row ${i}`}
        </div>
      ))}
    </div>
  )
}

const tabbable = () =>
  screen.getAllByRole('option').filter((o) => o.getAttribute('tabindex') === '0')

// One tab stop for the widget is the whole point: forty rows each taking a Tab
// is a table a keyboard user cannot get past.
test('exposes exactly one tab stop', () => {
  render(<Rows count={4} />)
  expect(tabbable()).toHaveLength(1)
  expect(tabbable()[0]).toHaveTextContent('row 0')
})

test('the arrows move the focus, and the tab stop with it', async () => {
  const user = userEvent.setup()
  render(<Rows count={3} />)
  screen.getAllByRole('option')[0]?.focus()

  await user.keyboard('{ArrowDown}')
  expect(screen.getAllByRole('option')[1]).toHaveFocus()
  expect(tabbable()[0]).toHaveTextContent('row 1')

  await user.keyboard('{ArrowUp}')
  expect(screen.getAllByRole('option')[0]).toHaveFocus()
})

test('Home and End reach the ends', async () => {
  const user = userEvent.setup()
  render(<Rows count={5} />)
  screen.getAllByRole('option')[0]?.focus()

  await user.keyboard('{End}')
  expect(screen.getAllByRole('option')[4]).toHaveFocus()
  await user.keyboard('{Home}')
  expect(screen.getAllByRole('option')[0]).toHaveFocus()
})

// Arrowing past either end stays put rather than running off: an index out of
// range leaves *no* row tabbable, which drops the widget out of the tab order
// entirely.
test('never runs off either end', async () => {
  const user = userEvent.setup()
  render(<Rows count={2} />)
  screen.getAllByRole('option')[0]?.focus()

  await user.keyboard('{ArrowUp}{ArrowUp}')
  expect(tabbable()).toHaveLength(1)
  expect(screen.getAllByRole('option')[0]).toHaveFocus()

  await user.keyboard('{ArrowDown}{ArrowDown}{ArrowDown}')
  expect(tabbable()).toHaveLength(1)
  expect(screen.getAllByRole('option')[1]).toHaveFocus()
})

// The list can shrink under the widget - a run's agents come and go - and the
// clamp is what keeps a tab stop after it does.
test('keeps a tab stop when the list shrinks under it', async () => {
  const user = userEvent.setup()
  const { rerender } = render(<Rows count={5} />)
  screen.getAllByRole('option')[0]?.focus()
  await user.keyboard('{End}')

  rerender(<Rows count={2} />)
  expect(tabbable()).toHaveLength(1)
  expect(tabbable()[0]).toHaveTextContent('row 1')
})

test('an empty widget exposes no tab stop at all', () => {
  render(<Rows count={0} />)
  expect(screen.queryAllByRole('option')).toHaveLength(0)
})

// It swallows the keys it handles and nothing else, so a key it does not bind
// still reaches the page.
test('leaves the keys it does not handle alone', async () => {
  const user = userEvent.setup()
  const onPick = vi.fn()
  render(<Rows count={2} onPick={onPick} />)
  screen.getAllByRole('option')[0]?.focus()

  await user.keyboard('a')
  expect(screen.getAllByRole('option')[0]).toHaveFocus()
  expect(onPick).not.toHaveBeenCalled()
})
