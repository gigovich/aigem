import { readable } from '@/lib/text'
import type { Approval, Decision } from '@/lib/wire'

type Props = {
  approval: Approval
  onDecide: (decision: Decision) => void
  disabled?: boolean
}

/**
 * The one question a conversation stops on.
 *
 * The buttons are the request's own options and not a fixed pair: "Always" on a
 * write outside the working directory is a promise the sandbox refuses to keep,
 * so the daemon does not offer it there - and a front-end that drew it anyway
 * would be offering a button that does not do what it says.
 *
 * The refusal is always the last option, which is what lets Escape be bound to
 * it without knowing which question is being asked.
 */
/**
 * A tool's arguments, one per row, with strings kept as text.
 *
 * `JSON.stringify` turns a shell script into a single line with `\n` between
 * its statements, which is precisely the form nobody reads to the end of.
 */
function argLines(args: unknown): { key: string; value: string }[] {
  if (!args || typeof args !== 'object') return []
  return Object.entries(args as Record<string, unknown>).map(([key, value]) => ({
    key,
    value: typeof value === 'string' ? value : JSON.stringify(value, null, 2),
  }))
}

export function ApprovalCard({ approval, onDecide, disabled = false }: Props) {
  const args = approval.kind === 'tool' ? argLines(approval.args) : []
  const lines = args.reduce((n, a) => n + a.value.split('\n').length, 0)
  const options = approval.options
  const refuse = options[options.length - 1]
  const rest = options.slice(0, -1)
  // The first option a request offers is always the plain "yes"; anything
  // between it and the refusal is a broader grant, which is the secondary.
  const primary = rest[0]
  const middle = rest.slice(1)

  return (
    <div
      className="mt-3 rounded-r-[7px] border border-l-2 border-line border-l-attention bg-bg px-3 py-[11px]"
      role="group"
      aria-label="Approval required"
    >
      <div className="flex items-center gap-2 font-mono text-[10.5px] text-fg-subtle">
        <span aria-hidden="true" className="text-attention">
          !
        </span>
        <span>
          {approval.kind === 'path'
            ? `${approval.write ? 'write' : 'read'} outside the working directory`
            : 'tool call awaiting approval'}
        </span>
      </div>

      {/* Through `readable`: a path carrying a bidi override displays as one
          name and is another, and this is the one screen where a person makes a
          security decision on a string the model supplied. */}
      <div className="mt-[7px] font-mono text-[12px] break-all text-fg">
        {readable(approval.kind === 'path' ? (approval.path ?? '') : approval.tool)}
      </div>
      {args.length > 0 && (
        <>
          {/* Not clipped to a scroll box, and each string argument shown as the
              text it is rather than as one escaped JSON line. A long command
              with one hostile line at the end is exactly the shape an attacker
              wants approved, and a box showing nine of its sixty lines - or all
              sixty run together with \n between them - is the mechanism. */}
          {args.map((a) => (
            <div key={a.key} className="mt-2">
              <div className="font-mono text-[10px] text-fg-subtle">{a.key}</div>
              <pre className="m-0 overflow-x-auto rounded-md border border-line bg-shell p-2 font-mono text-[11px] whitespace-pre-wrap text-fg-muted">
                {readable(a.value)}
              </pre>
            </div>
          ))}
          {lines > 12 && (
            <p className="mt-1 mb-0 font-mono text-[10.5px] text-attention">
              {lines} lines — read to the end before approving
            </p>
          )}
        </>
      )}

      <div className="mt-[11px] flex flex-wrap items-center gap-2">
        <span className="font-mono text-[10.5px] text-fg-subtle">
          {approval.kind === 'path' ? approval.tool : 'answered once, by whoever gets there first'}
        </span>
        <div className="ml-auto flex gap-2">
          {refuse && (
            <button
              type="button"
              disabled={disabled}
              onClick={() => onDecide(refuse.value)}
              className="h-[27px] cursor-pointer rounded-md border border-line px-[11px] text-[11.5px] text-fg-muted hover:border-danger hover:text-danger disabled:opacity-50"
            >
              {refuse.label}
            </button>
          )}
          {middle.map((o) => (
            <button
              key={o.value}
              type="button"
              disabled={disabled}
              onClick={() => onDecide(o.value)}
              className="h-[27px] cursor-pointer rounded-md border border-line px-[11px] text-[11.5px] text-fg-muted hover:border-line-strong hover:text-fg disabled:opacity-50"
            >
              {o.label}
            </button>
          ))}
          {primary && (
            <button
              type="button"
              disabled={disabled}
              onClick={() => onDecide(primary.value)}
              className="h-[27px] cursor-pointer rounded-md border border-primary bg-primary px-[11px] text-[11.5px] font-medium text-bg hover:brightness-110 disabled:opacity-50"
            >
              {primary.label === 'Once' ? 'Approve & run' : primary.label}
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
