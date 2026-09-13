import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'
import { mountApp, RUN, setViewport } from '@/test/harness'
import { NARROW_AT } from '@/state/app'

afterEach(() => {
  vi.unstubAllGlobals()
  setViewport(1440)
})

const width = (el: Element | null) => (el as HTMLElement | null)?.style.width

// The two vertical rules come from one token each, so the header's logo column
// and the navigation column beneath it cannot drift apart. The values behind
// the tokens are pinned against the canvas in test/design-values.test.ts.
test('the shell draws its two columns from the layout tokens', async () => {
  await mountApp({ runs: [RUN] })
  expect(width(screen.getByRole('navigation', { name: 'Navigation' }))).toBe('var(--rail)')
  expect(width(screen.getByRole('banner').firstElementChild)).toBe('var(--rail)')
  await waitFor(() =>
    expect(width(screen.getByRole('complementary', { name: 'Inspector' }))).toBe('var(--panel)'),
  )
})

// The breakpoint is 1120. Below it the columns narrow, the inspector closes,
// and the breadcrumbs and the run pill go: what has to survive is the command
// button and the four controls.
test('narrows the columns and closes the inspector below the breakpoint', async () => {
  await mountApp({ runs: [RUN] })
  await screen.findByRole('complementary', { name: 'Inspector' })
  expect(screen.getByRole('navigation', { name: 'Breadcrumb' })).toBeInTheDocument()

  act(() => setViewport(NARROW_AT - 1))

  await waitFor(() =>
    expect(screen.queryByRole('complementary', { name: 'Inspector' })).not.toBeInTheDocument(),
  )
  expect(screen.queryByRole('navigation', { name: 'Breadcrumb' })).not.toBeInTheDocument()
  expect(screen.getByRole('button', { name: /Search or run a command/ })).toBeInTheDocument()
})

// Exactly at the breakpoint is the wide layout: the canvas's rule is
// `window.innerWidth < 1120`.
test('the breakpoint itself is the wide layout', async () => {
  await mountApp({ runs: [RUN] })
  act(() => setViewport(NARROW_AT - 1))
  await waitFor(() =>
    expect(screen.queryByRole('complementary', { name: 'Inspector' })).not.toBeInTheDocument(),
  )
  act(() => setViewport(NARROW_AT))
  await waitFor(() =>
    expect(screen.getByRole('navigation', { name: 'Breadcrumb' })).toBeInTheDocument(),
  )
})

// Closing the panel below the breakpoint is the layout's decision, not the
// person's, and a window dragged narrow and back should not cost them the panel.
test('widening brings the inspector back', async () => {
  await mountApp({ runs: [RUN] })
  await screen.findByRole('complementary', { name: 'Inspector' })

  act(() => setViewport(900))
  await waitFor(() =>
    expect(screen.queryByRole('complementary', { name: 'Inspector' })).not.toBeInTheDocument(),
  )

  act(() => setViewport(1440))
  await waitFor(() =>
    expect(screen.getByRole('complementary', { name: 'Inspector' })).toBeInTheDocument(),
  )
})

// It can also be closed on purpose, and brought back from the header.
test('the inspector can be closed and reopened', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [RUN] })
  await screen.findByRole('complementary', { name: 'Inspector' })

  await user.click(screen.getByRole('button', { name: 'Close inspector' }))
  expect(screen.queryByRole('complementary', { name: 'Inspector' })).not.toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: 'Inspector: hidden' }))
  expect(screen.getByRole('complementary', { name: 'Inspector' })).toBeInTheDocument()
})

test('the theme and density toggles change the document and the footer', async () => {
  const user = userEvent.setup()
  await mountApp()
  expect(document.documentElement.dataset.theme).toBe('mocha')
  expect(screen.getByRole('contentinfo')).toHaveTextContent('mocha · dense')

  await user.click(screen.getByRole('button', { name: 'Theme: mocha' }))
  expect(document.documentElement.dataset.theme).toBe('latte')

  await user.click(screen.getByRole('button', { name: 'Density: dense' }))
  expect(document.documentElement.dataset.density).toBe('comfortable')
  expect(screen.getByRole('contentinfo')).toHaveTextContent('latte · comfortable')
})

// A page whose control socket is down still works - every mutation is an HTTP
// request - but it stops learning about anything a terminal does, and the
// person is entitled to know before they wonder why nothing updates.
test('the status bar says when the control stream is down', async () => {
  await mountApp()
  expect(screen.getByRole('contentinfo')).not.toHaveTextContent('reconnecting')

  const { FakeSocket } = await import('@/test/fakesocket')
  const control = FakeSocket.instances.find((s) => s.url.includes('/api/socket'))
  act(() => control?.drop())

  await waitFor(() => expect(screen.getByRole('contentinfo')).toHaveTextContent('reconnecting'))
})

// Below the phone width there is one column. The navigation is a drawer the
// header opens, and choosing a screen in it closes it.
test('a phone keeps the navigation in a drawer', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [RUN] })
  act(() => setViewport(400))
  await waitFor(() =>
    expect(screen.queryByRole('navigation', { name: 'Navigation' })).not.toBeInTheDocument(),
  )

  await user.click(screen.getByRole('button', { name: 'Open navigation' }))
  const nav = screen.getByRole('navigation', { name: 'Navigation' })
  await user.click(within(nav).getByRole('button', { name: /Activity/ }))

  expect(window.location.pathname).toBe('/activity')
  expect(screen.queryByRole('navigation', { name: 'Navigation' })).not.toBeInTheDocument()

  // Choosing the screen already shown is a navigation nowhere, and still closes it.
  await user.click(screen.getByRole('button', { name: 'Open navigation' }))
  await user.click(
    within(screen.getByRole('navigation', { name: 'Navigation' })).getByRole('button', {
      name: /Activity/,
    }),
  )
  expect(screen.queryByRole('navigation', { name: 'Navigation' })).not.toBeInTheDocument()
})

// A list beside its detail needs 406px of chrome before any content; a phone
// shows one or the other, with a way back.
test('a phone shows the session list or the conversation, not both', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [RUN] })
  act(() => setViewport(400))

  await waitFor(() => expect(screen.queryByRole('textbox', { name: 'Message' })).not.toBeInTheDocument())
  const list = screen.getByRole('list', { name: 'Sessions' })
  await user.click(within(list).getAllByRole('button', { name: /Rotate the signing keys/ })[0]!)

  expect(window.location.pathname).toBe('/chat/r-1')
  expect(screen.getByRole('textbox', { name: 'Message' })).toBeInTheDocument()
  expect(screen.queryByRole('list', { name: 'Sessions' })).not.toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: '‹ Sessions' }))
  expect(window.location.pathname).toBe('/chat')
  expect(screen.getByRole('list', { name: 'Sessions' })).toBeInTheDocument()
})

// A phone starts with the inspector closed; a selection is the moment it is
// wanted, and a tap that changes nothing visible reads as broken.
test('on a phone, selecting a row opens the inspector', async () => {
  const user = userEvent.setup()
  await mountApp({
    runs: [RUN],
    models: [
      {
        ref: 'openai/gpt-5',
        provider: 'openai',
        name: 'GPT-5',
        contextWindow: 400000,
        needsAuth: true,
        authenticated: true,
        default: false,
      },
    ],
    activity: [{ seq: 1, at: '2026-09-08T12:00:00Z', kind: 'run.opened', text: 'older', runRef: 'r-1' }],
  })
  act(() => setViewport(400))
  await user.click(screen.getByRole('button', { name: 'Open navigation' }))
  await user.click(within(screen.getByRole('navigation', { name: 'Navigation' })).getByRole('button', { name: /Models/ }))
  expect(screen.queryByRole('complementary', { name: 'Inspector' })).not.toBeInTheDocument()

  await user.click(await screen.findByRole('row', { name: /GPT-5/ }))
  const aside = await screen.findByRole('complementary', { name: 'Inspector' })
  expect(within(aside).getByRole('button', { name: 'Make default' })).toBeInTheDocument()

  await user.click(within(aside).getByRole('button', { name: 'Close inspector' }))
  await user.click(screen.getByRole('button', { name: 'Open navigation' }))
  await user.click(within(screen.getByRole('navigation', { name: 'Navigation' })).getByRole('button', { name: /Activity/ }))
  await user.click(await screen.findByRole('button', { name: /older/ }))
  expect(await screen.findByRole('complementary', { name: 'Inspector' })).toHaveTextContent('run.opened')
})

test('the drawer takes focus when it opens and gives it back when it closes', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [RUN] })
  act(() => setViewport(400))
  const menu = screen.getByRole('button', { name: 'Open navigation' })
  await user.click(menu)
  const nav = screen.getByRole('navigation', { name: 'Navigation' })
  await waitFor(() => expect(nav.contains(document.activeElement)).toBe(true))
  await user.keyboard('{Escape}')
  await waitFor(() => expect(document.activeElement).toBe(menu))
})
