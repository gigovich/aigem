/**
 * A line diff, computed here because the daemon sends both sides and not a
 * patch.
 *
 * It is the plain longest-common-subsequence table. That is O(n·m) in the two
 * line counts, which is fine for a file a person is about to read and hopeless
 * for a generated one - so there is a ceiling, and past it the caller is told
 * the sizes instead of being handed a diff the tab would freeze computing.
 */

export type DiffLine = { sign: '+' | '-' | ' '; text: string }

/**
 * The ceilings, in lines and in bytes.
 *
 * Measured: 4000x4000 with nothing in common is 439ms and 108MB for one file,
 * synchronously, on the thread that draws - and a run can change ten. 1500 is
 * an eighth of that table. The byte ceiling is the other half of the same
 * question: 1499 lines of 200 kB each passes a line count and is a diff nobody
 * would read anyway.
 */
export const MAX_DIFF_LINES = 1500
export const MAX_DIFF_BYTES = 512 * 1024

export type Diff =
  | { kind: 'lines'; lines: DiffLine[]; added: number; removed: number }
  | { kind: 'too-large'; oldLines: number; newLines: number }
  /**
   * Every line is the same and the bytes are not: the line endings changed, or
   * the file gained or lost its final newline.
   */
  | { kind: 'invisible' }

export function diffLines(before: string, after: string): Diff {
  const a = split(before)
  const b = split(after)
  if (
    a.length > MAX_DIFF_LINES ||
    b.length > MAX_DIFF_LINES ||
    before.length > MAX_DIFF_BYTES ||
    after.length > MAX_DIFF_BYTES
  ) {
    return { kind: 'too-large', oldLines: a.length, newLines: b.length }
  }
  // Both sides are read with CRLF normalised and without their trailing empty
  // line, so a file that changed only in those would otherwise come out as a
  // diff with nothing in it - a change the daemon reported and the page saying
  // it did not happen.
  if (before !== after && same(a, b)) return { kind: 'invisible' }

  // table[i][j] is the length of the longest common subsequence of a[i:] and
  // b[j:]. Built backwards so the walk below can go forwards, which is the
  // order the lines have to come out in.
  const table: number[][] = Array.from({ length: a.length + 1 }, () =>
    new Array<number>(b.length + 1).fill(0),
  )
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      const row = table[i]
      const next = table[i + 1]
      if (!row || !next) continue
      row[j] = a[i] === b[j] ? (next[j + 1] ?? 0) + 1 : Math.max(next[j] ?? 0, row[j + 1] ?? 0)
    }
  }

  const lines: DiffLine[] = []
  let added = 0
  let removed = 0
  let i = 0
  let j = 0
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      lines.push({ sign: ' ', text: a[i] ?? '' })
      i++
      j++
      continue
    }
    if ((table[i + 1]?.[j] ?? 0) >= (table[i]?.[j + 1] ?? 0)) {
      lines.push({ sign: '-', text: a[i] ?? '' })
      removed++
      i++
    } else {
      lines.push({ sign: '+', text: b[j] ?? '' })
      added++
      j++
    }
  }
  for (; i < a.length; i++) {
    lines.push({ sign: '-', text: a[i] ?? '' })
    removed++
  }
  for (; j < b.length; j++) {
    lines.push({ sign: '+', text: b[j] ?? '' })
    added++
  }
  return { kind: 'lines', lines, added, removed }
}

function same(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((line, i) => line === b[i])
}

/**
 * The lines of a file, with the trailing empty one a final newline produces
 * dropped: it is not a line anybody wrote and it appears as a change whenever
 * one side ends with a newline and the other does not.
 *
 * Only CRLF is normalised. A lone carriage return is a character inside a line -
 * a progress bar's output, most often - and treating it as a break invents
 * lines the file does not have.
 */
function split(text: string): string[] {
  if (text === '') return []
  const lines = text.replace(/\r\n/g, '\n').split('\n')
  if (lines[lines.length - 1] === '') lines.pop()
  return lines
}

/** The unified form `DiffView` draws: a sign character in front of each line. */
export function unified(lines: DiffLine[]): string[] {
  return lines.map((l) => `${l.sign}${l.text}`)
}
