import { render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import App from './App'

afterEach(() => {
  vi.unstubAllGlobals()
})

// The page signs in first and asks after: /api/auth/session answers 204 with no
// body, and the route it asks next answers the JSON.
const answer = (body: unknown, init?: ResponseInit) =>
  vi.fn((path: string) =>
    Promise.resolve(
      path === '/api/auth/session'
        ? new Response(null, { status: 204 })
        : new Response(JSON.stringify(body), { status: 200, ...init }),
    ),
  )

// The version has to come out of the answer, or the spine goes green against a
// daemon that said nothing at all.
test('reports the daemon as reachable once the meta route answers', async () => {
  vi.stubGlobal('fetch', answer({ version: '1.2.3-test' }))
  render(<App />)
  expect(await screen.findByRole('status')).toHaveTextContent('daemon reachable: 1.2.3-test')
})

// It asks a route the credential is required for on purpose: /healthz needs
// none, so a spine built on that one reports a reachable daemon while the
// sign-in is broken.
test('asks a route the sign-in is required for', async () => {
  const fetched: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn((path: string) => {
      fetched.push(path)
      return Promise.resolve(
        path === '/api/auth/session'
          ? new Response(null, { status: 204 })
          : new Response(JSON.stringify({ version: '1.2.3-test' }), { status: 200 }),
      )
    }),
  )
  render(<App />)
  await screen.findByText(/daemon reachable/)
  expect(fetched).toEqual(['/api/auth/session', '/api/meta'])
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
