import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'
import { mountApp, RUN } from '@/test/harness'
import { filter, ordered, score } from './commands'
import type { PaletteItem } from './commands'

afterEach(() => {
  vi.unstubAllGlobals()
})

const item = (label: string, hint = '', group: PaletteItem['group'] = 'Navigate'): PaletteItem => ({
  id: label,
  label,
  hint,
  icon: '›',
  group,
  run: vi.fn(),
})

// Subsequence rather than substring is what makes a palette worth having:
// "nsn" has to find "New session".
test('matches on a subsequence and ranks the tighter match first', () => {
  const items = [item('New session'), item('Activity feed'), item('Open agent session')]
  // "Open agent session" is a subsequence match too - that is the point - but
  // the tighter one comes first.
  expect(filter(items, 'nsn').map((i) => i.label)[0]).toBe('New session')
  expect(filter(items, 'nsn').map((i) => i.label)).not.toContain('Activity feed')
  expect(filter(items, 'session').map((i) => i.label)[0]).toBe('New session')
  expect(filter(items, 'zzz')).toEqual([])
})

test('scores an early, tight match above a late, scattered one', () => {
  const early = item('New session')
  const late = item('Toggle density', 'creates a new session eventually')
  expect(score(early, 'new')).toBeLessThan(score(late, 'new'))
  expect(score(early, 'zzz')).toBe(-1)
  // No query is no ranking: the unfiltered list keeps its group order.
  expect(score(early, '')).toBe(0)
})

test('groups the unfiltered list in the canvas order', () => {
  const items = [
    item('Toggle theme', '', 'Preferences'),
    item('New session', '', 'Create'),
    item('Open agent session', '', 'Navigate'),
    item('Switch model', '', 'Execute'),
  ]
  expect(ordered(items, '').map((i) => i.group)).toEqual([
    'Navigate',
    'Create',
    'Execute',
    'Preferences',
  ])
})

test('opens on the shortcut, filters, and runs the highlighted command', async () => {
  const user = userEvent.setup()
  await mountApp()

  await user.keyboard('{Control>}k{/Control}')
  const palette = await screen.findByRole('dialog', { name: 'Command palette' })
  expect(palette).toBeInTheDocument()

  await user.keyboard('activity')
  const options = screen.getAllByRole('option')
  expect(options).toHaveLength(1)
  expect(options[0]).toHaveTextContent('Activity feed')

  await user.keyboard('{Enter}')
  await waitFor(() => expect(window.location.pathname).toBe('/activity'))
})

test('the arrows move the highlight, and Escape closes it', async () => {
  const user = userEvent.setup()
  await mountApp()

  await user.keyboard('{Control>}k{/Control}')
  await screen.findByRole('dialog', { name: 'Command palette' })
  const first = screen.getAllByRole('option')[0]
  expect(first).toHaveAttribute('aria-selected', 'true')

  await user.keyboard('{ArrowDown}')
  expect(screen.getAllByRole('option')[1]).toHaveAttribute('aria-selected', 'true')
  expect(screen.getAllByRole('option')[0]).toHaveAttribute('aria-selected', 'false')

  // The highlight never runs off either end.
  await user.keyboard('{ArrowUp}{ArrowUp}{ArrowUp}')
  expect(screen.getAllByRole('option')[0]).toHaveAttribute('aria-selected', 'true')

  await user.keyboard('{Escape}')
  await waitFor(() =>
    expect(screen.queryByRole('dialog', { name: 'Command palette' })).not.toBeInTheDocument(),
  )
})

test('says so when nothing matches', async () => {
  const user = userEvent.setup()
  await mountApp()

  await user.keyboard('{Control>}k{/Control}')
  await screen.findByRole('dialog', { name: 'Command palette' })
  await user.keyboard('zzzzzz')

  expect(screen.getByText('No matching command.')).toBeInTheDocument()
  expect(screen.queryAllByRole('option')).toHaveLength(0)
})

// The palette must not offer what this daemon cannot do: a command that
// navigates to a screen the feature map has withdrawn is a dead end.
test('offers only what the feature map allows', async () => {
  const user = userEvent.setup()
  await mountApp({ meta: { features: { controlSocket: true, runs: true } } })

  await user.keyboard('{Control>}k{/Control}')
  await screen.findByRole('dialog', { name: 'Command palette' })
  const labels = screen.getAllByRole('option').map((o) => o.textContent ?? '')

  expect(labels.some((l) => l.includes('New session'))).toBe(true)
  expect(labels.some((l) => l.includes('Activity feed'))).toBe(false)
  expect(labels.some((l) => l.includes('Switch model'))).toBe(false)
})

// The daemon spells its commands with the slash already on ("/new"). A palette
// that added another would list "//new", which nobody finds by typing its name.
test('lists the daemon catalogue without doubling the slash', async () => {
  const user = userEvent.setup()
  await mountApp({
    commands: [{ name: '/new', description: 'Start a fresh session' }],
  })

  await user.keyboard('{Control>}k{/Control}')
  await screen.findByRole('dialog', { name: 'Command palette' })
  const labels = screen.getAllByRole('option').map((o) => o.textContent ?? '')

  expect(labels.some((l) => l.includes('/new'))).toBe(true)
  expect(labels.some((l) => l.includes('//new'))).toBe(false)
})

// Choosing one drops the person into the composer with it typed, which is where
// a slash command is entered anyway.
test('a command from the palette lands in the composer', async () => {
  const user = userEvent.setup()
  await mountApp({
    runs: [RUN],
    commands: [{ name: '/compact', description: 'Compact the conversation' }],
  })

  await user.keyboard('{Control>}k{/Control}')
  await screen.findByRole('dialog', { name: 'Command palette' })
  await user.keyboard('compact{Enter}')

  await waitFor(() =>
    expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('/compact '),
  )
})
