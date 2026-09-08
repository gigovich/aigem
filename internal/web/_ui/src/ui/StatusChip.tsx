import { STATUS } from '@/lib/wire'
import type { StatusKey } from '@/lib/wire'

type Props = {
  status: StatusKey
  /** The row form: glyph only, with the label carried by the title. */
  compact?: boolean
  className?: string
}

/**
 * One state, drawn the way the canvas's `S` says.
 *
 * The glyph is not decoration. Colour alone would leave the state unreadable to
 * anyone who cannot tell `--danger` from `--success`, and `blocked` and `failed`
 * are the same red - only the glyph separates them.
 */
export function StatusChip({ status, compact = false, className = '' }: Props) {
  const s = STATUS[status]
  return (
    <span
      className={`flex items-center gap-[6px] text-[11.5px] ${className}`}
      style={{ color: s.color }}
    >
      <span aria-hidden="true" className="text-[10px] leading-none">
        {s.icon}
      </span>
      {compact ? <span className="sr-only">{s.label}</span> : <span>{s.label}</span>}
    </span>
  )
}

/** The pulsing dot the header and the run screen use for live work. */
export function LiveDot({ className = '' }: { className?: string }) {
  return (
    <span
      aria-hidden="true"
      className={`inline-block size-[6px] flex-none rounded-full bg-running ${className}`}
      style={{ animation: 'aigem-pulse 2.4s ease-in-out infinite' }}
    />
  )
}
