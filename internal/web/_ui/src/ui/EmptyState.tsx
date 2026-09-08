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
    return <div className="px-3 py-4 text-[11.5px] text-fg-subtle">{title}</div>
  }
  return (
    <div className="px-5 py-10 text-center">
      <div className="text-[13px] text-fg-muted">{title}</div>
      {detail && (
        <div className="mx-auto mt-1 max-w-[56ch] text-[12px] text-pretty text-fg-subtle">
          {detail}
        </div>
      )}
      {action && (
        <button
          type="button"
          onClick={action.onClick}
          disabled={action.busy}
          className="mt-3 h-[27px] rounded-md border border-primary bg-primary px-[11px] text-[11.5px] font-medium text-bg enabled:cursor-pointer enabled:hover:brightness-110 disabled:opacity-50"
        >
          {action.label}
        </button>
      )}
    </div>
  )
}
