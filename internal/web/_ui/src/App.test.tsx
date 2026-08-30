import { render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import App from './App'

afterEach(() => {
  vi.unstubAllGlobals()
})

const answer = (body: unknown, init?: ResponseInit) =>
  vi.fn(async () => new Response(JSON.stringify(body), { status: 200, ...init }))

test('reports the daemon as reachable once /healthz answers', async () => {
  vi.stubGlobal('fetch', answer({ ok: true, ui: true }))
  render(<App />)
  expect(await screen.findByRole('status')).toHaveTextContent('daemon reachable')
})

// A blank page is the failure this catches: the bundle loads, the daemon does
// not answer, and without this the page says nothing at all.
test('says so when the daemon does not answer', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response('', { status: 503, statusText: 'Service Unavailable' })),
  )
  render(<App />)
  expect(await screen.findByRole('status')).toHaveTextContent('daemon unreachable')
})

test('says it is connecting before the daemon answers', () => {
  vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(() => {})))
  render(<App />)
  expect(screen.getByRole('status')).toHaveTextContent('connecting…')
})

// StrictMode mounts twice, so an unaborted request from the discarded mount
// would resolve into a component that no longer exists.
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
