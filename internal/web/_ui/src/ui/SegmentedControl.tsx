export type Segment<T extends string> = { value: T; label: string }

type Props<T extends string> = {
  segments: Segment<T>[]
  value: T
  onChange: (value: T) => void
  label: string
}

/**
 * One group of buttons in one border, as the canvas draws the status filters.
 *
 * It is a radiogroup rather than a row of buttons: what it expresses is one
 * choice out of a set, and a screen reader that is told that can say which of
 * the five is current instead of reading five unrelated buttons.
 */
export function SegmentedControl<T extends string>({ segments, value, onChange, label }: Props<T>) {
  const at = Math.max(0, segments.indexOf(segments.find((s) => s.value === value) ?? segments[0]!))
  const move = (delta: number, e: React.KeyboardEvent) => {
    const next = segments[(at + delta + segments.length) % segments.length]
    if (!next) return
    e.preventDefault()
    onChange(next.value)
    ;(e.currentTarget.parentElement?.children[segments.indexOf(next)] as HTMLElement | undefined)?.focus()
  }
  return (
    <div
      role="radiogroup"
      aria-label={label}
      className="flex overflow-hidden rounded-md border border-line"
    >
      {segments.map((s, i) => (
        <button
          key={s.value}
          type="button"
          role="radio"
          aria-checked={s.value === value}
          // One tab stop for the group; the arrows choose within it.
          tabIndex={s.value === value ? 0 : -1}
          onKeyDown={(e) => {
            if (e.key === 'ArrowRight' || e.key === 'ArrowDown') move(1, e)
            if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') move(-1, e)
          }}
          onClick={() => onChange(s.value)}
          className={`h-[24px] cursor-pointer px-[9px] text-[11px] ${
            i > 0 ? 'border-l border-line' : ''
          } ${s.value === value ? 'bg-s0 text-fg' : 'bg-transparent text-fg-subtle hover:text-fg'}`}
        >
          {s.label}
        </button>
      ))}
    </div>
  )
}
