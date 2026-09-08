import type { ReactNode } from 'react'
import { FieldList } from '@/ui/FieldList'
import { ProgressBar } from '@/ui/ProgressBar'
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
  const open = useApp((s) => s.inspectorOpen)
  if (!open || !content) return null
  return (
    <aside
      aria-label="Inspector"
      className="flex-none overflow-y-auto border-l border-line bg-shell"
      style={{ width: 'var(--panel)', animation: 'aigem-sheet .16s ease-out' }}
    >
      <div className="flex items-center gap-2 border-b border-line px-3 py-[10px]">
        <span className="font-mono text-[11px] text-fg-subtle">{content.kind}</span>
        <span className="min-w-0 overflow-hidden font-mono text-[11px] text-ellipsis whitespace-nowrap text-fg-muted">
          {content.id}
        </span>
        <button
          type="button"
          onClick={() => setInspector(false)}
          aria-label="Close inspector"
          className="ml-auto grid size-5 cursor-pointer place-items-center rounded-[4px] text-[13px] text-fg-subtle hover:bg-s0 hover:text-fg"
        >
          <span aria-hidden="true">×</span>
        </button>
      </div>

      <div className="p-3">
        <h2 className="m-0 text-[13px] font-semibold tracking-[-0.01em] text-pretty">
          {content.title}
        </h2>
        {content.status && <StatusChip status={content.status} className="mt-[6px]" />}
      </div>

      <Rule />
      <div className="p-3">
        <FieldList fields={content.fields} />
      </div>

      {content.progress && (
        <div className="px-3 pb-[14px]">
          <ProgressBar used={content.progress.used} total={content.progress.total} />
        </div>
      )}

      {content.list && content.list.length > 0 && (
        <>
          <Rule />
          <div className="p-3">
            <h3 className="mb-[7px] text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
              {content.listTitle}
            </h3>
            {content.list.map((row) => (
              <div key={row.text} className="flex min-h-[26px] items-center gap-2">
                <span
                  aria-hidden="true"
                  className="text-[10px]"
                  style={{ color: row.color ?? 'var(--fg-subtle)' }}
                >
                  {row.icon}
                </span>
                <span className="overflow-hidden text-[11.5px] text-ellipsis whitespace-nowrap text-fg-muted">
                  {row.text}
                </span>
                {row.meta && (
                  <span className="ml-auto font-mono text-[10px] text-fg-subtle">{row.meta}</span>
                )}
              </div>
            ))}
          </div>
        </>
      )}

      {content.actions && content.actions.length > 0 && (
        <>
          <Rule />
          <div className="flex flex-wrap gap-[6px] p-3">
            {content.actions.map((a) => (
              <button
                key={a.label}
                type="button"
                onClick={a.onClick}
                className="h-[26px] cursor-pointer rounded-md border border-line bg-surface px-[10px] text-[11.5px] text-fg-muted hover:border-line-strong hover:text-fg"
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
