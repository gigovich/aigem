import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { signIn } from './auth'

beforeEach(() => {
  window.history.replaceState(null, '', '/')
})

afterEach(() => {
  vi.unstubAllGlobals()
})

// Typed as fetch is, so the recorded call has a shape the assertions can read
// rather than an implicit any pair.
const answers = (status: number) =>
  vi.fn<(path: string, init?: RequestInit) => Promise<Response>>(() =>
    Promise.resolve(new Response(null, { status })),
  )

test('spends the token from the address bar and takes it back out', async () => {
  const fetchMock = answers(204)
  vi.stubGlobal('fetch', fetchMock)
  window.history.replaceState(null, '', '/models?token=secret-token')

  await signIn()

  const call = fetchMock.mock.calls[0]
  if (!call) throw new Error('the exchange was never attempted')
  const [path, init] = call
  expect(path).toBe('/api/auth/session')
  expect(init?.method).toBe('POST')
  expect(init?.headers).toEqual({ Authorization: 'Bearer secret-token' })
  // The credential is gone from the URL, and the route the person opened is not.
  expect(window.location.search).toBe('')
  expect(window.location.pathname).toBe('/models')
})

// The token is the only credential the page holds. Stripping it from a refused
// exchange would leave a reload with nothing to retry with, which is a sign-in
// that can only be fixed by going back to the terminal.
test('keeps the token when the exchange is refused', async () => {
  vi.stubGlobal('fetch', answers(401))
  window.history.replaceState(null, '', '/?token=secret-token')

  await expect(signIn()).rejects.toThrow('sign-in refused: 401')
  expect(window.location.search).toBe('?token=secret-token')
})

// Every load runs the exchange, not just the one that arrives from the printed
// link: that is what renews a cookie close to expiry.
test('runs without a token, on the cookie alone', async () => {
  const fetchMock = answers(204)
  vi.stubGlobal('fetch', fetchMock)

  await signIn()

  const call = fetchMock.mock.calls[0]
  if (!call) throw new Error('the exchange was never attempted')
  expect(call[1]?.headers).toBeUndefined()
})
