import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'
import { EventKind } from '@/lib/wire'
import { mountApp, RUN } from '@/test/harness'

afterEach(() => {
  vi.unstubAllGlobals()
})

async function open() {
  const user = userEvent.setup()
  const h = await mountApp({ runs: [RUN] })
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  act(() => h.runSocket()?.open())
  await user.keyboard('{Control>}j{/Control}')
  const panel = await screen.findByRole('dialog', { name: 'Quick chat' })
  return { user, h, panel }
}

test('sends on Enter, because it is a line and not a composer', async () => {
  const { user, h, panel } = await open()

  await user.type(
    within(panel).getByRole('textbox', { name: /Ask anything/ }),
    'which runs are blocked?{Enter}',
  )

  await waitFor(() => expect(h.runSocket()?.sent).toHaveLength(1))
  expect(JSON.parse(h.runSocket()?.sent[0] ?? '{}')).toEqual({
    op: 'submit',
    text: 'which runs are blocked?',
  })
})

// It is the same conversation as the chat screen, not a second agent - which is
// what makes "Open session" a navigation rather than a hand-off.
test('shows the same conversation the chat screen does', async () => {
  const { h, panel } = await open()
  h.emit({ seq: 1, time: '2026-09-08T12:00:01Z', kind: EventKind.UserMessage, text: 'a question' })
  h.emit({
    seq: 2,
    time: '2026-09-08T12:00:02Z',
    kind: EventKind.AssistantMessage,
    text: 'an answer',
  })

  await waitFor(() => expect(panel).toHaveTextContent('a question'))
  expect(panel).toHaveTextContent('an answer')
  // The tool detail belongs in the transcript, not in a corner panel.
  h.emit({ seq: 3, time: '2026-09-08T12:00:03Z', kind: EventKind.ToolStart, name: 'read_file' })
  expect(panel).not.toHaveTextContent('read_file')
})

test('continues into the session it is showing', async () => {
  const { user, panel } = await open()
  await user.click(within(panel).getByRole('button', { name: 'Open session' }))

  await waitFor(() => expect(window.location.pathname).toBe('/chat/r-1'))
  expect(screen.queryByRole('dialog', { name: 'Quick chat' })).not.toBeInTheDocument()
})

test('closes on Escape and on its own button', async () => {
  const { user, panel } = await open()
  await user.click(within(panel).getByRole('button', { name: 'Close quick chat' }))
  await waitFor(() =>
    expect(screen.queryByRole('dialog', { name: 'Quick chat' })).not.toBeInTheDocument(),
  )

  await user.keyboard('{Control>}j{/Control}')
  await screen.findByRole('dialog', { name: 'Quick chat' })
  await user.keyboard('{Escape}')
  await waitFor(() =>
    expect(screen.queryByRole('dialog', { name: 'Quick chat' })).not.toBeInTheDocument(),
  )
})

// With no conversation there is nothing to ask, and a field that accepted text
// and dropped it would be worse than one that says why it is closed.
test('says there is nothing to ask when no session is open', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [] })
  await user.keyboard('{Control>}j{/Control}')

  const panel = await screen.findByRole('dialog', { name: 'Quick chat' })
  expect(panel).toHaveTextContent('no session')
  expect(within(panel).getByRole('textbox', { name: /Ask anything/ })).toBeDisabled()
  expect(within(panel).getByRole('button', { name: 'Open session' })).toBeDisabled()
})
