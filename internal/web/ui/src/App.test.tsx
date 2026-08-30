import { render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import App from './App'

afterEach(() => {
  vi.unstubAllGlobals()
})

test('reports the daemon as reachable once /healthz answers', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(JSON.stringify({ ok: true, ui: true }), { status: 200 })),
  )
  render(<App />)
  expect(await screen.findByText('daemon reachable')).toBeDefined()
})

// A blank page is the failure this catches: the bundle loads, the daemon does
// not answer, and without this the page says nothing at all.
test('says so when the daemon does not answer', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response('', { status: 503, statusText: 'Service Unavailable' })),
  )
  render(<App />)
  expect(await screen.findByText(/daemon unreachable/)).toBeDefined()
})
