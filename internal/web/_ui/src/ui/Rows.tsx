export type Row = { icon: string; color?: string; text: string; meta?: string }

type Props = {
  title?: string
  rows: Row[]
  mono?: boolean
  /** "h2" beside a screen's own headings, "h3" (the default) inside the inspector. */
  heading?: 'h2' | 'h3'
  /** Announced in place of an empty `meta`, for a caller whose rows carry state in it. */
  fallbackMeta?: string
}

export function Rows({ title, rows, mono = false, heading = 'h3', fallbackMeta }: Props) {
  const Heading = heading
  return (
    <div className="p-3">
      <Heading className="mb-[7px] text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
        {title}
      </Heading>
      {rows.map((row, i) => (
        <div
          key={i}
          className={`flex min-h-[26px] items-center gap-2 ${mono ? 'font-mono' : ''}`}
        >
          <span
            aria-hidden="true"
            className="text-[10px]"
            style={{ color: row.color ?? 'var(--fg-subtle)' }}
          >
            {row.icon}
          </span>
          {fallbackMeta !== undefined && !row.meta && (
            <span className="sr-only">{fallbackMeta}</span>
          )}
          <span className="overflow-hidden text-[11.5px] text-ellipsis whitespace-nowrap text-fg-muted">
            {row.text}
          </span>
          {row.meta && (
            <span className="ml-auto font-mono text-[10px] text-fg-subtle">{row.meta}</span>
          )}
        </div>
      ))}
    </div>
  )
}
