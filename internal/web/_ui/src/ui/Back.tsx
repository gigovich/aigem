import { navigate } from '@/lib/route'
import type { Route } from '@/lib/route'

/** The way back to the list a phone is not showing beside this. */
export function Back({ label, to }: { label: string; to: Route }) {
  return (
    <button
      type="button"
      onClick={() => navigate(to)}
      className="h-[26px] cursor-pointer rounded-md border border-line px-[8px] text-[11.5px] whitespace-nowrap text-fg-muted hover:border-line-strong hover:text-fg"
    >
      ‹ {label}
    </button>
  )
}
