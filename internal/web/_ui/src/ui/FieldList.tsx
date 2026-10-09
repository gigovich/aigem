import type { ReactNode } from 'react'

export type Field = { key: string; value: ReactNode; color?: string }

/**
 * Key on the left in the interface font, value on the right in mono.
 *
 * Mono for the value is the rule the canvas applies everywhere: an id, a path,
 * a branch, a number and a time all have to line up down a column and all have
 * to survive being compared by eye against another one.
 */
export function FieldList({ fields, keyWidth = 84 }: { fields: Field[]; keyWidth?: number }) {
  return (
    <dl className="m-0">
      {fields.map((f) => (
        <div key={f.key} className="flex items-baseline gap-2.5 py-1">
          <dt
            className="flex-none text-[0.75rem] text-fg-subtle"
            style={{ width: `${keyWidth / 16}rem` }}
          >
            {f.key}
          </dt>
          <dd
            className="m-0 min-w-0 font-mono text-[0.78125rem] break-all"
            style={{ color: f.color ?? 'var(--fg-muted)' }}
          >
            {f.value}
          </dd>
        </div>
      ))}
    </dl>
  )
}
