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
        <span>{path}</span>
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
            style={{ gridTemplateColumns: '12px 1fr', background: tint(sign) }}
          >
            <span aria-hidden="true" style={{ color: SIGN_COLOR[sign] ?? 'var(--fg-subtle)' }}>
              {sign === '@' ? '' : sign.trim()}
            </span>
            <span
              className="whitespace-pre-wrap"
              style={{ color: sign === '@' ? 'var(--fg-subtle)' : 'var(--fg-muted)' }}
            >
              {text}
            </span>
          </div>
        )
      })}
    </div>
  )
}
