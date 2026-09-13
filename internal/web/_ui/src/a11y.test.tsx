import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'
import { EventKind } from '@/lib/wire'
import { mountApp, RUN } from '@/test/harness'

afterEach(() => {
  vi.unstubAllGlobals()
})

/*
 * The design is dense with icon-only controls - a gear, a chevron, an x, a
 * theme glyph - and with states drawn in colour. Neither survives being read by
 * anything other than an eye unless it is made to, and these are the checks
 * that keep it that way as screens are added.
 */

test('every control on the shell has a name that can be read out', async () => {
  await mountApp({ runs: [RUN] })
  const unnamed = screen
    .getAllByRole('button')
    .filter((b) => (b.textContent ?? '').trim() === '' && !b.getAttribute('aria-label'))
  expect(unnamed.map((b) => b.outerHTML)).toEqual([])
})

// The glyph is what keeps colour from being the only carrier of a state. Where
// the design shows the glyph alone, the label is still in the document for
// anything that is not looking at it.
test('a status is readable without its colour', async () => {
  await mountApp({ runs: [RUN] })
  const list = await screen.findByRole('list', { name: 'Sessions' })
  // RUN is live with nothing in flight, which is "Idle".
  expect(list).toHaveTextContent('Idle')
})

test('the whole shell can be reached from the keyboard', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [RUN] })

  const reached = new Set<string>()
  for (let i = 0; i < 25; i++) {
    await user.tab()
    const active = document.activeElement
    if (active && active !== document.body) {
      reached.add(active.getAttribute('aria-label') ?? active.textContent?.trim() ?? '')
    }
  }
  expect(reached.has('Search or run a command⌘K')).toBe(true)
  expect(reached.has('Theme: mocha')).toBe(true)
  expect([...reached].some((label) => label.includes('Models'))).toBe(true)
})

// A confirmation is told to a screen reader only if the region announces
// itself; without this the toast is a flash that only someone watching sees.
test('a confirmation announces itself', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [RUN] })

  await user.keyboard('{Control>}k{/Control}')
  await screen.findByRole('dialog', { name: 'Command palette' })
  await user.keyboard('toggle theme{Enter}')

  // The announcement is the region that was already in the document, not the
  // visible plate beside it: a region inserted together with its text is
  // announced by nothing.
  const toast = await screen.findByRole('status', { name: 'Notifications' })
  await waitFor(() => expect(toast).toHaveTextContent('Theme: latte'))
})

// The context bar is a number, and a number drawn as a coloured strip is not a
// number to anything that is not looking at it.
test('the context bar reports its value', async () => {
  const h = await mountApp({ runs: [RUN] })
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  act(() => h.runSocket()?.open())
  h.emit({
    seq: 1,
    time: '2026-09-08T12:00:01Z',
    kind: 'session_meta',
    id: 's-1',
    text: 'Rotate the signing keys',
    ctx: 200000,
  })
  h.emit({ seq: 2, time: '2026-09-08T12:00:02Z', kind: 'usage', tokens: 50000 })

  const bar = await screen.findByRole('progressbar')
  expect(bar).toHaveAttribute('aria-valuenow', '25')
  expect(bar).toHaveAccessibleName(/50,000 \/ 200,000 tokens/)
})

// Each region has to be in the document before its text arrives, and each is
// asserted by name: over "some status region exists" the check passes on any
// one of the three, so removing another silences it with the test still green.
test('every announcement region is present before there is anything to announce', async () => {
  const h = await mountApp({ runs: [RUN] })
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  act(() => h.runSocket()?.open())
  for (const name of ['Notifications', 'Connection', 'Stream']) {
    const region = screen.getByRole('status', { name })
    expect(region, name).toHaveAttribute('aria-live', 'polite')
    expect(region.textContent, name).toBe('')
  }
  // The approval region is assertive: it is the moment the agent stops and
  // waits for a person, and a polite one waits for a pause that may not come.
  const approval = screen.getByRole('region', { name: 'Approval' })
  expect(approval).toHaveAttribute('aria-live', 'assertive')
  expect(approval.textContent).toBe('')
})

// A run whose transcript is folded outside React must not fold twice under
// StrictMode, which invokes a state updater a second time to find exactly this.
test('an event is applied once under StrictMode', async () => {
  const h = await mountApp({ runs: [RUN], strict: true })
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  act(() => h.runSocket()?.open())
  h.emit({
    seq: 1,
    time: '2026-09-08T12:00:01Z',
    kind: EventKind.UserMessage,
    text: 'said once',
  })

  const log = await screen.findByRole('log')
  expect(within(log).getAllByText('said once')).toHaveLength(1)
})
