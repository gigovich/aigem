import { act, screen, waitFor } from '@testing-library/react'
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
