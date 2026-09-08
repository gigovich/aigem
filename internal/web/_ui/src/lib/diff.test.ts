import { expect, test } from 'vitest'
import { diffLines, MAX_DIFF_LINES, unified } from './diff'

const lines = (before: string, after: string) => {
  const d = diffLines(before, after)
  if (d.kind !== 'lines') throw new Error('expected a diff')
  return d
}

test('marks what was added, removed and kept', () => {
  const d = lines('a\nb\nc\n', 'a\nB\nc\n')
  expect(unified(d.lines)).toEqual([' a', '-b', '+B', ' c'])
  expect(d.added).toBe(1)
  expect(d.removed).toBe(1)
})

test('a new file is all additions and a deleted one all removals', () => {
  expect(unified(lines('', 'a\nb\n').lines)).toEqual(['+a', '+b'])
  expect(unified(lines('a\nb\n', '').lines)).toEqual(['-a', '-b'])
})

test('an unchanged file has no changes at all', () => {
  const d = lines('a\nb\n', 'a\nb\n')
  expect(d.added).toBe(0)
  expect(d.removed).toBe(0)
  expect(unified(d.lines)).toEqual([' a', ' b'])
})

// The empty string after a trailing newline is not a line anybody wrote, and
// counting it makes every file that gained one look like it changed twice.
test('a trailing newline is not a line', () => {
  expect(lines('a\n', 'a').added).toBe(0)
  expect(lines('a\n', 'a').removed).toBe(0)
})

test('reads either line ending', () => {
  expect(unified(lines('a\r\nb\r\n', 'a\r\nB\r\n').lines)).toEqual([' a', '-b', '+B'])
})

// The table is O(n·m), which is fine for a file a person is about to read and
// hopeless for a generated one. Past the ceiling the sizes are reported rather
// than the tab freezing.
test('refuses to diff a file too long to diff', () => {
  const long = `${'x\n'.repeat(MAX_DIFF_LINES + 1)}`
  const d = diffLines(long, 'y\n')
  expect(d.kind).toBe('too-large')
  if (d.kind !== 'too-large') return
  expect(d.oldLines).toBe(MAX_DIFF_LINES + 1)
  expect(d.newLines).toBe(1)
})

// An insertion in the middle must not be reported as "everything after it
// changed", which is what a diff that does not find the common subsequence does.
test('finds the common lines around an insertion', () => {
  const d = lines('a\nb\nc\nd\n', 'a\nb\nNEW\nc\nd\n')
  expect(unified(d.lines)).toEqual([' a', ' b', '+NEW', ' c', ' d'])
  expect(d.added).toBe(1)
  expect(d.removed).toBe(0)
})
