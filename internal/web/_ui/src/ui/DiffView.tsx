import { readable } from '@/lib/text'

type Props = {
  path: string
  /** The unified body: each line starts with ' ', '+' or '-'. */
  lines: string[]
  /** `~` modified, `+` created, `-` deleted, as the canvas marks the header. */
  change?: '~' | '+' | '-'
}

const SIGN_COLOR: Record<string, string> = {
  '+': 'var(--added)',
  '-': 'var(--deleted)',
  '~': 'var(--modified)',
  '@': 'var(--fg-subtle)',
}

const SIGN_LABEL: Record<string, string> = {
  '+': 'added',
  '-': 'removed',
  '@': 'hunk',
  ' ': 'context',
}

function tint(sign: string): string | undefined {
  if (sign === '+') return 'color-mix(in oklab, var(--added) 10%, transparent)'
  if (sign === '-') return 'color-mix(in oklab, var(--deleted) 10%, transparent)'
  return undefined
}

/**
 * A file's change: a header naming it, then the body a line at a time.
 *
 * The sign is its own grid column rather than the first character of the text,
 * so a line that begins with a `+` in the source is not drawn as an addition -
 * and so selecting the body copies the code without the diff markers.
 */
export function DiffView({ path, lines, change = '~' }: Props) {
  return (
    <div className="overflow-hidden rounded-md border border-line bg-bg">
      <div className="flex h-[30px] items-center gap-2 border-b border-line bg-shell px-[10px] font-mono text-[11px] text-fg-muted">
        <span aria-hidden="true" style={{ color: SIGN_COLOR[change] }}>
          {change}
        </span>
        <span className="break-all">{readable(path)}</span>
      </div>
      {lines.map((line, i) => {
        const sign = line.startsWith('@@') ? '@' : (line[0] ?? ' ')
        const text = sign === '@' ? line : line.slice(1)
        return (
          <div
            // The index is the identity: two identical lines in a diff are two
            // different lines, and nothing here reorders.
            key={i}
            className="grid gap-2 px-[10px] py-px font-mono text-[11.5px]"
            style={{ gridTemplateColumns: '12px 0 1fr', background: tint(sign) }}
          >
            {/* The sign is the content on a diff: a line that reads the same
                added and removed is a diff with the diff taken out. The glyph
                is hidden and the word takes its place for a reader. */}
            <span aria-hidden="true" style={{ color: SIGN_COLOR[sign] ?? 'var(--fg-subtle)' }}>
              {sign === '@' ? '' : sign.trim()}
            </span>
            {/* Only where the sign carries something. A five-hundred-line diff
                that says "context" before four hundred and eighty of them is a
                reader wading through the word rather than the file. */}
            {sign !== ' ' && <span className="sr-only">{SIGN_LABEL[sign] ?? 'changed'} </span>}
            {/* Broken anywhere rather than clipped: the card is
                `overflow-hidden`, and an unbroken run pushes the text track
                past it - a minified line appended to a file would simply not be
                on the screen a person is auditing it on. */}
            <span
              className="break-all whitespace-pre-wrap"
              style={{ color: sign === '@' ? 'var(--fg-subtle)' : 'var(--fg-muted)' }}
            >
              {text.replace(/\r/g, '␍')}
            </span>
          </div>
        )
      })}
    </div>
  )
}
