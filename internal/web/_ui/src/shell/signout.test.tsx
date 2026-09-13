import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'
import { mountApp } from '@/test/harness'

afterEach(() => {
  vi.unstubAllGlobals()
})

// The one security action a person can take from the page: forgetting this
// browser's cookie on the daemon, which is where a cookie is honoured.
test('signing out revokes the session on the daemon and reloads without it', async () => {
  const user = userEvent.setup()
  const replace = vi.fn()
  vi.stubGlobal('location', { ...window.location, replace })
  const h = await mountApp({
    routes: { 'DELETE /api/auth/session': () => new Response(null, { status: 204 }) },
  })

  await user.click(screen.getByRole('button', { name: 'Sign out' }))

  await waitFor(() =>
    expect(h.sent).toContainEqual({ path: '/api/auth/session', method: 'DELETE', body: '' }),
  )
  await waitFor(() => expect(replace).toHaveBeenCalledWith('/'))
})
