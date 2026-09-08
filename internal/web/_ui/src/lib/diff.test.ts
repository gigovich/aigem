import { expect, test } from 'vitest'
import { diffLines, MAX_DIFF_BYTES, MAX_DIFF_LINES, unified } from './diff'

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

// The empty string after a trailing newline is not a line anybody wrote, so it
// is not counted as one - but the bytes did change, and saying nothing happened
// would be a change the daemon reported and the page denying.
test('a trailing newline is not a line, and is not nothing either', () => {
  expect(diffLines('a\n', 'a').kind).toBe('invisible')
  expect(lines('a\nb\n', 'a\nB\n').added).toBe(1)
})

test('reads either line ending', () => {
  expect(unified(lines('a\r\nb\r\n', 'a\r\nB\r\n').lines)).toEqual([' a', '-b', '+B'])
})

// Both sides are read with CRLF normalised, so a file whose only change is its
// line endings comes out as a diff with nothing in it - a change the daemon
// reported and the page saying it did not happen.
test('says so when only the line endings changed', () => {
  expect(diffLines('a\r\nb\r\n', 'a\nb\n').kind).toBe('invisible')
  // Not for a file that did not change at all.
  expect(diffLines('a\nb\n', 'a\nb\n').kind).toBe('lines')
})

// A lone carriage return is a character inside a line - a progress bar's
// output, most often - and treating it as a break invents lines the file has
// not got.
test('a bare carriage return is content, not a line break', () => {
  const d = lines('done\rdone\n', 'done\n')
  expect(d.removed).toBe(1)
  expect(d.added).toBe(1)
  expect(unified(d.lines)).toEqual(['-done\rdone', '+done'])
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

// The byte ceiling is the other half of the same question: a few hundred very
// long lines passes a line count and is a table nobody wants built.
test('refuses to diff a file too large to diff, however few lines it has', () => {
  const wide = `${'x'.repeat(MAX_DIFF_BYTES + 1)}\n`
  expect(diffLines(wide, 'y\n').kind).toBe('too-large')
})

// The ceiling exists to keep the thread that draws responsive. This is the
// worst case it allows, and it has to stay in the tens of milliseconds.
test('the worst diff it will attempt is quick', () => {
  const a = Array.from({ length: MAX_DIFF_LINES }, (_, i) => `a${i}`).join('\n')
  const b = Array.from({ length: MAX_DIFF_LINES }, (_, i) => `b${i}`).join('\n')
  const started = performance.now()
  const d = diffLines(a, b)
  const took = performance.now() - started
  expect(d.kind).toBe('lines')
  expect(took).toBeLessThan(1000)
})

// An insertion in the middle must not be reported as "everything after it
// changed", which is what a diff that does not find the common subsequence does.
test('finds the common lines around an insertion', () => {
  const d = lines('a\nb\nc\nd\n', 'a\nb\nNEW\nc\nd\n')
  expect(unified(d.lines)).toEqual([' a', ' b', '+NEW', ' c', ' d'])
  expect(d.added).toBe(1)
  expect(d.removed).toBe(0)
})
