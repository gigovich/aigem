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
export function ApprovalCard({ approval, onDecide, disabled = false }: Props) {
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

      <div className="mt-[7px] font-mono text-[12px] break-all text-fg">
        {approval.kind === 'path' ? approval.path : approval.tool}
      </div>
      {approval.kind === 'tool' && approval.args ? (
        <pre className="mt-2 mb-0 max-h-40 overflow-auto rounded-md border border-line bg-shell p-2 font-mono text-[11px] whitespace-pre-wrap text-fg-muted">
          {JSON.stringify(approval.args, null, 2)}
        </pre>
      ) : null}

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
