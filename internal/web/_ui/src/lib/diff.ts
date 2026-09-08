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

/** Past this many lines on either side the table is not worth building. */
export const MAX_DIFF_LINES = 4000

export type Diff =
  | { kind: 'lines'; lines: DiffLine[]; added: number; removed: number }
  | { kind: 'too-large'; oldLines: number; newLines: number }

export function diffLines(before: string, after: string): Diff {
  const a = split(before)
  const b = split(after)
  if (a.length > MAX_DIFF_LINES || b.length > MAX_DIFF_LINES) {
    return { kind: 'too-large', oldLines: a.length, newLines: b.length }
  }

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

/**
 * The lines of a file, with the trailing empty one a final newline produces
 * dropped: it is not a line anybody wrote and it appears as a change whenever
 * one side ends with a newline and the other does not.
 */
function split(text: string): string[] {
  if (text === '') return []
  const lines = text.replace(/\r\n?/g, '\n').split('\n')
  if (lines[lines.length - 1] === '') lines.pop()
  return lines
}

/** The unified form `DiffView` draws: a sign character in front of each line. */
export function unified(lines: DiffLine[]): string[] {
  return lines.map((l) => `${l.sign}${l.text}`)
}
