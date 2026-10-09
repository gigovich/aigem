import type { ReactNode } from 'react'

type Props = {
  title: string
  detail?: ReactNode
  action?: { label: string; onClick: () => void; busy?: boolean }
  /** The flush form the session list uses inside a narrow column. */
  inline?: boolean
}

/** A heading, an explanation and at most one thing to do about it. */
export function EmptyState({ title, detail, action, inline = false }: Props) {
  if (inline) {
    return <div className="px-3 py-4 text-[0.78125rem] text-fg-subtle">{title}</div>
  }
  return (
    <div className="px-5 py-10 text-center">
      <div className="text-[0.875rem] text-fg-muted">{title}</div>
      {detail && (
        <div className="mx-auto mt-1 max-w-[56ch] text-[0.8125rem] text-pretty text-fg-subtle">
          {detail}
        </div>
      )}
      {action && (
        <button
          type="button"
          onClick={action.onClick}
          disabled={action.busy}
          className="mt-3 h-6.75 rounded-md border border-primary bg-primary px-2.75 text-[0.78125rem] font-medium text-bg enabled:hover:brightness-110 disabled:opacity-50"
        >
          {action.label}
        </button>
      )}
    </div>
  )
}
