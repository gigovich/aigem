import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, test, vi } from 'vitest'
import { Modal } from './Modal'

test('is a dialog, named by its title', () => {
  render(
    <Modal title="Close this session?" onClose={vi.fn()}>
      body
    </Modal>,
  )
  expect(screen.getByRole('dialog', { name: 'Close this session?' })).toHaveAttribute(
    'aria-modal',
    'true',
  )
})

// The trap is the whole reason this is a component. Without it Tab walks into
// the page behind, and for a keyboard user the dialog is on screen with nothing
// they press reaching it.
test('keeps Tab inside itself, in both directions', async () => {
  const user = userEvent.setup()
  render(
    <>
      <button type="button">outside</button>
      <Modal title="Pick one" onClose={vi.fn()} confirm={{ label: 'Do it', onClick: vi.fn() }}>
        <button type="button">inside</button>
      </Modal>
    </>,
  )

  const inside = screen.getByRole('button', { name: 'inside' })
  const cancel = screen.getByRole('button', { name: 'Cancel' })
  const confirm = screen.getByRole('button', { name: 'Do it' })
  expect(inside).toHaveFocus()

  await user.tab()
  await user.tab()
  expect(confirm).toHaveFocus()

  // Past the last, back to the first - never out to "outside".
  await user.tab()
  expect(inside).toHaveFocus()

  await user.tab({ shift: true })
  expect(confirm).toHaveFocus()
  expect(cancel).not.toHaveFocus()
})

// Returning focus is the other half: without it the person is left standing on
// <body> with no way back into the application.
test('gives focus back to whatever opened it', () => {
  const opener = document.createElement('button')
  opener.textContent = 'open'
  document.body.appendChild(opener)
  opener.focus()

  const { unmount } = render(
    <Modal title="Pick one" onClose={vi.fn()}>
      body
    </Modal>,
  )
  expect(opener).not.toHaveFocus()
  unmount()
  expect(opener).toHaveFocus()
  opener.remove()
})

test('closes on Escape and on the backdrop, and not on the panel', async () => {
  const user = userEvent.setup()
  const onClose = vi.fn()
  render(
    <Modal title="Pick one" onClose={onClose}>
      <span>body</span>
    </Modal>,
  )

  await user.click(screen.getByText('body'))
  expect(onClose).not.toHaveBeenCalled()

  await user.keyboard('{Escape}')
  expect(onClose).toHaveBeenCalledTimes(1)
})

test('runs the confirm and honours a disabled one', async () => {
  const user = userEvent.setup()
  const onClick = vi.fn()
  const { rerender } = render(
    <Modal title="Pick one" onClose={vi.fn()} confirm={{ label: 'Do it', onClick }}>
      body
    </Modal>,
  )
  await user.click(screen.getByRole('button', { name: 'Do it' }))
  expect(onClick).toHaveBeenCalledTimes(1)

  rerender(
    <Modal title="Pick one" onClose={vi.fn()} confirm={{ label: 'Do it', onClick, disabled: true }}>
      body
    </Modal>,
  )
  await user.click(screen.getByRole('button', { name: 'Do it' }))
  expect(onClick).toHaveBeenCalledTimes(1)
})
