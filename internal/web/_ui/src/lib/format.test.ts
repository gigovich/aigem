import { expect, test } from 'vitest'
import { ago, bytes, clock, compact, count, elapsed, hhmm, percent, tokens } from './format'

test('groups the numbers the context label is read from', () => {
  expect(count(144208)).toBe('144,208')
  // The exact string the design's context line shows.
  expect(tokens(144208, 200000)).toBe('144,208 / 200,000 tokens')
})

test('shortens a context window the way the models column does', () => {
  expect(compact(200000)).toBe('200K')
  expect(compact(400000)).toBe('400K')
  expect(compact(32768)).toBe('33K')
  expect(compact(1000000)).toBe('1M')
  expect(compact(512)).toBe('512')
})

test('formats a timestamp for the event stream and for the feed', () => {
  const at = '2026-09-08T14:32:01Z'
  // Rendered in the viewer's zone, so the assertion is on the shape rather than
  // on a fixed hour - a test that pinned one would fail everywhere but here.
  expect(clock(at)).toMatch(/^\d{2}:\d{2}:\d{2}$/)
  expect(hhmm(at)).toMatch(/^\d{2}:\d{2}$/)
})

// A date the daemon could not fill in reaches this as an empty string or a zero
// time. Rendering "Invalid Date" into a table is worse than rendering nothing.
test('renders nothing for a time that is not one', () => {
  expect(clock('')).toBe('')
  expect(hhmm('nonsense')).toBe('')
  expect(ago('')).toBe('')
  expect(elapsed('')).toBe('')
})

test('says how long ago in the units the design uses', () => {
  const now = Date.parse('2026-09-08T12:00:00Z')
  const at = (secs: number) => new Date(now - secs * 1000).toISOString()
  expect(ago(at(3), now)).toBe('now')
  expect(ago(at(12 * 60), now)).toBe('12m')
  expect(ago(at(2 * 3600), now)).toBe('2h')
  expect(ago(at(3 * 86400), now)).toBe('3d')
  // A clock that is behind the daemon's must not produce a negative age.
  expect(ago(at(-60), now)).toBe('now')
})

test('counts elapsed time as the run header shows it', () => {
  const now = Date.parse('2026-09-08T12:00:00Z')
  expect(elapsed(new Date(now - 134 * 1000), now)).toBe('02:14')
  expect(elapsed(new Date(now - 3725 * 1000), now)).toBe('01:02:05')
})

test('sizes a trimmed tool result', () => {
  expect(bytes(512)).toBe('512 B')
  expect(bytes(2048)).toBe('2.0 kB')
  expect(bytes(3 * 1024 * 1024)).toBe('3.0 MB')
})

// A progress bar whose width came out above 100% escapes its own track, and a
// context window the daemon reported as zero must not divide by it.
test('clamps a percentage into the bar', () => {
  expect(percent(50, 200)).toBe(25)
  expect(percent(300, 200)).toBe(100)
  expect(percent(-5, 200)).toBe(0)
  expect(percent(10, 0)).toBe(0)
})
