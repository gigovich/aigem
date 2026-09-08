import { percent as pct, tokens as tokenLabel } from '@/lib/format'

type Props = {
  used: number
  total: number
  label?: string
  /** The heading over the bar; the canvas writes "Context". */
  title?: string
}

/**
 * The context bar.
 *
 * The colour is the warning the number carries, so it is derived here rather
 * than passed in: a screen that chose its own would eventually disagree with
 * another screen about when a context window is nearly full.
 */
export function ProgressBar({ used, total, label, title = 'Context' }: Props) {
  const value = pct(used, total)
  const color = value >= 90 ? 'var(--danger)' : value >= 70 ? 'var(--warning)' : 'var(--success)'
  // One caption for the eye and for the accessible name: a bar announced as
  // "25%" tells a screen-reader user less than the page tells everyone else.
  const caption = label ?? (total > 0 ? tokenLabel(used, total) : `${value}%`)
  return (
    <div>
      <div className="flex justify-between text-[11px] text-fg-subtle">
        <span>{title}</span>
        <span className="font-mono" style={{ color }}>
          {value}%
        </span>
      </div>
      <div
        className="mt-[5px] h-[5px] overflow-hidden rounded-[3px] bg-s0"
        role="progressbar"
        aria-valuenow={value}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={`${title}: ${caption}`}
      >
        <div
          className="h-full transition-[width] duration-300 ease-out"
          style={{ width: `${value}%`, background: color }}
        />
      </div>
      {total > 0 && (
        <div className="mt-1 font-mono text-[10px] text-fg-subtle">{caption}</div>
      )}
    </div>
  )
}
