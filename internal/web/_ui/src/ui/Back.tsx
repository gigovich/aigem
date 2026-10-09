import { navigate } from '@/lib/route'
import type { Route } from '@/lib/route'

/** The way back to the list a phone is not showing beside this. */
export function Back({ label, to }: { label: string; to: Route }) {
  return (
    <button
      type="button"
      onClick={() => navigate(to)}
      className="h-6.5 rounded-md border border-line px-2.5 text-[0.78125rem] whitespace-nowrap text-fg-muted hover:border-line-strong hover:text-fg"
    >
      ‹ {label}
    </button>
  )
}
