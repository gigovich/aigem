import { expect, test } from 'vitest'
import { runStatus, STATUS } from './wire'
import type { Run, StatusKey } from './wire'

/**
 * The status dictionary as the canvas declares it - the object `S` in
 * `Aigem.dc.html`. Label, glyph and colour are transcribed here so a drift
 * shows up as a failed assertion rather than as a screen that calls a state
 * something the rest of the design does not.
 */
const CANVAS: Record<StatusKey, { label: string; icon: string; color: string }> = {
  running: { label: 'Running', icon: '●', color: 'var(--running)' },
  waiting: { label: 'Waiting', icon: '◐', color: 'var(--warning)' },
  attention: { label: 'Needs attention', icon: '!', color: 'var(--attention)' },
  failed: { label: 'Failed', icon: '×', color: 'var(--danger)' },
  completed: { label: 'Completed', icon: '✓', color: 'var(--success)' },
  stopped: { label: 'Stopped', icon: '■', color: 'var(--fg-subtle)' },
  blocked: { label: 'Blocked', icon: '◇', color: 'var(--danger)' },
  planned: { label: 'Planned', icon: '○', color: 'var(--fg-subtle)' },
  progress: { label: 'In progress', icon: '◑', color: 'var(--primary)' },
}

test('the status dictionary is the canvas dictionary, all nine keys', () => {
  expect(Object.keys(STATUS).sort()).toEqual(Object.keys(CANVAS).sort())
  for (const [key, want] of Object.entries(CANVAS)) {
    expect(STATUS[key as StatusKey], key).toEqual(want)
  }
})

// The glyph is what keeps colour from being the only carrier of the state.
// Two states sharing one would put that back: `blocked` and `failed` are
// already the same red, and only the glyph tells them apart.
test('every status has a glyph, and no two share one', () => {
  const icons = Object.values(STATUS).map((s) => s.icon)
  expect(icons.every((i) => i.length > 0)).toBe(true)
  expect(new Set(icons).size).toBe(icons.length)
})

const run = (over: Partial<Run>): Run => ({
  id: 'r1',
  mode: 'interactive',
  status: 'open',
  created: '',
  updated: '',
  live: true,
  ...over,
})

test('a run record maps onto the dictionary', () => {
  expect(runStatus(run({ running: true }))).toBe('running')
  // "waiting" on the wire means parked on an approval nobody has answered,
  // which is the design's `attention` - its `waiting` is an open session with
  // nothing in flight, which is what a person sees between turns.
  expect(runStatus(run({ waiting: true }))).toBe('attention')
  expect(runStatus(run({}))).toBe('waiting')
  expect(runStatus(run({ live: false, status: 'closed' }))).toBe('stopped')
})

// An approval nobody has answered is the one thing on this screen a person has
// to act on, so it outranks the turn that is still notionally running.
test('an unanswered approval outranks a running turn', () => {
  expect(runStatus(run({ running: true, waiting: true }))).toBe('attention')
})

// A closed run has no session to be running in, whatever the record's stale
// booleans say.
test('a run with no session reads as stopped whatever else it claims', () => {
  expect(runStatus(run({ live: false, running: true, waiting: true }))).toBe('stopped')
})
