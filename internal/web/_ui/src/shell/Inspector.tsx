import type { ReactNode } from 'react'
import { FieldList } from '@/ui/FieldList'
import { ProgressBar } from '@/ui/ProgressBar'
import { Rows } from '@/ui/Rows'
import { StatusChip } from '@/ui/StatusChip'
import { setInspector, useApp } from '@/state/app'
import type { InspectorContent } from '@/state/inspector'

/**
 * The right-hand panel: what is selected, said once and in full.
 *
 * It is a panel and not a page because the thing it describes is always
 * something on the screen behind it - selecting a model must not lose the table
 * the model was picked out of.
 */
export function Inspector({ content }: { content: InspectorContent }) {
  const { open, phone } = useApp((s) => ({
    open: s.inspectorOpen,
    phone: s.phone,
  }))
  if (!open || !content) return null
  return (
    <aside
      aria-label="Inspector"
      className={`flex-none overflow-y-auto border-l border-line bg-shell ${
        phone
          ? 'fixed top-9.5 right-0 bottom-6 z-[60] w-full max-w-90 shadow-panel'
          : ''
      }`}
      style={{
        width: phone ? undefined : 'var(--panel)',
        animation: 'aigem-sheet .16s ease-out',
      }}
    >
      <div className="flex items-center gap-2 border-b border-line px-3 py-2.5">
        <span className="font-mono text-[0.75rem] text-fg-subtle">{content.kind}</span>
        <span className="min-w-0 overflow-hidden font-mono text-[0.75rem] text-ellipsis whitespace-nowrap text-fg-muted">
          {content.id}
        </span>
        <button
          type="button"
          onClick={() => setInspector(false)}
          aria-label="Close inspector"
          className="ml-auto grid size-5 place-items-center rounded-[0.25rem] text-[0.875rem] text-fg-subtle hover:bg-s0 hover:text-fg"
        >
          <span aria-hidden="true">×</span>
        </button>
      </div>

      <div className="p-3">
        <h2 className="m-0 text-[0.875rem] font-semibold tracking-[-0.01em] text-pretty">
          {content.title}
        </h2>
        {content.status && <StatusChip status={content.status} className="mt-1.5" />}
      </div>

      <Rule />
      <div className="p-3">
        <FieldList fields={content.fields} />
      </div>

      {content.progress && (
        <div className="px-3 pb-3.5">
          <ProgressBar used={content.progress.used} total={content.progress.total} />
        </div>
      )}

      {content.plan && content.plan.length > 0 && (
        <>
          <Rule />
          <Rows title="Plan" rows={content.plan} fallbackMeta="pending" />
        </>
      )}
      {content.list && content.list.length > 0 && (
        <>
          <Rule />
          <Rows title={content.listTitle} rows={content.list} />
        </>
      )}

      {content.actions && content.actions.length > 0 && (
        <>
          <Rule />
          <div className="flex flex-wrap gap-1.5 p-3">
            {content.actions.map((a) => (
              <button
                key={a.label}
                type="button"
                onClick={a.onClick}
                className="h-6.5 rounded-md border border-line bg-surface px-2.5 text-[0.78125rem] text-fg-muted hover:border-line-strong hover:text-fg"
              >
                {a.label}
              </button>
            ))}
          </div>
        </>
      )}
    </aside>
  )
}

function Rule(): ReactNode {
  return <div aria-hidden="true" className="h-px bg-line" />
}
