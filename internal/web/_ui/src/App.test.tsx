import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'
import App from './App'
import { installDaemon, META, mountApp, RUN } from '@/test/harness'
import { navigate } from '@/lib/route'
import { EventKind } from '@/lib/wire'

afterEach(() => {
  vi.unstubAllGlobals()
})

test('signs in, reads the daemon and draws the shell', async () => {
  const h = await mountApp()

  // The exchange comes first: everything else needs the cookie it buys.
  expect(h.paths[0]).toBe('/api/auth/session')
  expect(screen.getByRole('banner')).toHaveTextContent('Aigem')
  expect(screen.getByRole('banner')).toHaveTextContent(META.version)
  expect(screen.getByRole('navigation', { name: 'Navigation' })).toBeInTheDocument()
  expect(screen.getByRole('contentinfo')).toHaveTextContent('⌘K commands')
})

// A refused sign-in is not an unreachable daemon, and telling the operator the
// wrong one sends them to check the network instead of the token.
test('says so when the sign-in is refused', async () => {
  installDaemon({ routes: { '/api/auth/session': () => new Response(null, { status: 401 }) } })
  render(<App />)
  expect(await screen.findByRole('status')).toHaveTextContent('sign-in refused: 401')
})

test('says it is connecting before the daemon answers', () => {
  installDaemon()
  vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(() => {})))
  render(<App />)
  expect(screen.getByRole('status')).toHaveTextContent('connecting…')
})

// The feature map is what a page reads to decide which screens exist. A daemon
// with no state directory has no activity feed, and offering the screen would
// be offering one that can never hold anything.
test('hides the screens this daemon does not serve', async () => {
  await mountApp({ meta: { features: { controlSocket: true, runs: true } } })

  const nav = screen.getByRole('navigation', { name: 'Navigation' })
  expect(nav).toHaveTextContent('Sessions')
  expect(nav).not.toHaveTextContent('Activity')
  expect(nav).not.toHaveTextContent('Models')
  expect(nav).not.toHaveTextContent('Skills')
})

test('navigates to a screen and puts it in the address bar', async () => {
  const user = userEvent.setup()
  await mountApp({ models: [] })

  await user.click(screen.getByRole('button', { name: /Models/ }))
  expect(window.location.pathname).toBe('/models')
  expect(await screen.findByRole('heading', { name: 'Models', level: 1 })).toBeInTheDocument()
})

// The whole point of the control stream: something moved somewhere else in the
// daemon and this page has to notice without being asked.
test('refetches a collection when the control stream says it moved', async () => {
  const h = await mountApp({ runs: [] })
  const before = h.paths.filter((p) => p === '/api/runs').length

  h.publish('run.updated', 2, { id: 'r-1' })

  await waitFor(() => {
    expect(h.paths.filter((p) => p === '/api/runs').length).toBe(before + 1)
  })
})

// A gap means this page missed something it can never be told about, so the
// answer is to read everything again rather than to apply the frame that
// happened to arrive.
test('rereads everything when a revision skips', async () => {
  const h = await mountApp()
  const before = h.paths.filter((p) => p === '/api/models').length

  h.publish('run.updated', 9, { id: 'r-1' })

  await waitFor(() => {
    expect(h.paths.filter((p) => p === '/api/models').length).toBeGreaterThan(before)
  })
})

test('opens a run socket for the conversation it is showing', async () => {
  await mountApp({ runs: [RUN] })
  await waitFor(() => {
    expect(document.querySelector('main')).toBeTruthy()
  })
  const { FakeSocket } = await import('@/test/fakesocket')
  await waitFor(() => {
    expect(FakeSocket.instances.some((s) => s.url.includes('/api/runs/r-1/socket'))).toBe(true)
  })
})

// One socket per tab, not one per component. The chat screen, the run screen
// and the quick chat all draw the same conversation.
test('opens one run socket even with the quick chat also showing', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [RUN] })
  const { FakeSocket } = await import('@/test/fakesocket')
  await waitFor(() => {
    expect(FakeSocket.instances.filter((s) => s.url.includes('/socket?')).length).toBe(1)
  })

  await user.keyboard('{Control>}j{/Control}')
  expect(await screen.findByRole('dialog', { name: 'Quick chat' })).toBeInTheDocument()
  expect(FakeSocket.instances.filter((s) => s.url.includes('/socket?')).length).toBe(1)
})

test('draws the events the run stream sends', async () => {
  const h = await mountApp({ runs: [RUN] })
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  h.runSocket()?.open()
  h.emit({ seq: 1, time: '2026-09-08T12:00:01Z', kind: EventKind.UserMessage, text: 'rotate them' })

  expect(await screen.findByText('rotate them')).toBeInTheDocument()
})

// The refusal of an op is not something that happened in the conversation, so
// it is shown as a message about the request and never folded into the log.
test('shows a refused operation as a notice, not as an event', async () => {
  const h = await mountApp({ runs: [RUN] })
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  h.runSocket()?.open()
  h.emit({ seq: 1, time: '2026-09-08T12:00:01Z', kind: EventKind.UserMessage, text: 'rotate them' })
  await screen.findByRole('log')

  act(() => {
    h.runSocket()?.deliver({
      kind: 'client_error',
      op: 'resolve',
      error: 'approval already decided',
    })
  })

  expect(await screen.findByRole('alert')).toHaveTextContent('approval already decided')
  const log = screen.getByRole('log')
  expect(log).toHaveTextContent('rotate them')
  expect(log).not.toHaveTextContent('approval already decided')
})

// A socket left attached after the component is gone writes operations into a
// conversation nobody is looking at.
test('lets go of the run socket when the run changes', async () => {
  const h = await mountApp({ runs: [RUN, { ...RUN, id: 'r-2', title: 'Another' }] })
  const { FakeSocket } = await import('@/test/fakesocket')
  // The shell attaches to the newest run, which is r-2.
  await waitFor(() => expect(h.runSocket()?.url).toContain('/api/runs/r-2/socket'))
  const first = h.runSocket()
  if (!first) throw new Error('no run socket')
  act(() => first.open())

  act(() => navigate({ screen: 'run', id: 'r-1' }))
  await waitFor(() => expect(h.runSocket()?.url).toContain('/api/runs/r-1/socket'))
  expect(first.readyState).toBe(FakeSocket.CLOSED)
})
