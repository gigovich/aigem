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
