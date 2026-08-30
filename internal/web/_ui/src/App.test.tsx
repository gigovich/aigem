import { render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import App from './App'

afterEach(() => {
  vi.unstubAllGlobals()
})

// The page signs in first and asks after: /api/auth/session answers 204 with no
// body, /healthz answers the JSON.
const answer = (body: unknown, init?: ResponseInit) =>
  vi.fn((path: string) =>
    Promise.resolve(
      path === '/api/auth/session'
        ? new Response(null, { status: 204 })
        : new Response(JSON.stringify(body), { status: 200, ...init }),
    ),
  )

test('reports the daemon as reachable once /healthz answers', async () => {
  vi.stubGlobal('fetch', answer({ ok: true, ui: true }))
  render(<App />)
  expect(await screen.findByRole('status')).toHaveTextContent('daemon reachable')
})

// A refused sign-in is not an unreachable daemon, and telling the operator the
// wrong one sends them to check the network instead of the token.
test('says so when the sign-in is refused', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(new Response(null, { status: 401 }))),
  )
  render(<App />)
  expect(await screen.findByRole('status')).toHaveTextContent('sign-in refused: 401')
})

// A blank page is the failure this catches: the bundle loads, the daemon does
// not answer, and without this the page says nothing at all.
test('says so when the daemon does not answer', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn((path: string) =>
      Promise.resolve(
        path === '/api/auth/session'
          ? new Response(null, { status: 204 })
          : new Response(null, { status: 503, statusText: 'Service Unavailable' }),
      ),
    ),
  )
  render(<App />)
  expect(await screen.findByRole('status')).toHaveTextContent('daemon unreachable')
})

test('says it is connecting before the daemon answers', () => {
  vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(() => {})))
  render(<App />)
  expect(screen.getByRole('status')).toHaveTextContent('connecting…')
})

// An unaborted request outlives the component: it resolves into a setState on
// something that no longer exists, and under StrictMode's double mount the
// discarded first mount does it on every render.
test('aborts the request when it unmounts before the answer arrives', () => {
  const signals: AbortSignal[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn((_url: string, init?: RequestInit) => {
      if (init?.signal) signals.push(init.signal)
      return new Promise<Response>(() => {})
    }),
  )
  const { unmount } = render(<App />)
  unmount()
  expect(signals.length).toBeGreaterThan(0)
  expect(signals.every((s) => s.aborted)).toBe(true)
})
