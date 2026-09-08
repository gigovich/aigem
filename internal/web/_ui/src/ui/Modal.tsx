import { useEffect, useRef } from 'react'
import type { ReactNode } from 'react'

type Props = {
  title: string
  subtitle?: string
  children: ReactNode
  onClose: () => void
  /** The optional third action the canvas puts on the left of the footer. */
  secondary?: { label: string; onClick: () => void }
  confirm?: { label: string; onClick: () => void; danger?: boolean; disabled?: boolean }
  cancelLabel?: string
  width?: number
}

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])'

/**
 * A layer with the focus inside it.
 *
 * The trap is the whole reason this is a component rather than a div: without
 * it Tab walks out of the dialog into the page behind, which for a keyboard
 * user means the dialog is still on screen and nothing they press reaches it.
 * Focus is returned to whatever opened it, or the person is left standing on
 * `<body>` with no way back into the application.
 *
 * Escape is handled here as well as in the global keymap, because a dialog must
 * close even when the keymap has been suspended by a layer above it.
 */
export function Modal({
  title,
  subtitle,
  children,
  onClose,
  secondary,
  confirm,
  cancelLabel = 'Cancel',
  width = 420,
}: Props) {
  const panel = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null
    const first = panel.current?.querySelector<HTMLElement>(FOCUSABLE)
    ;(first ?? panel.current)?.focus()
    return () => opener?.focus?.()
  }, [])

  const keydown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.stopPropagation()
      onClose()
      return
    }
    if (e.key !== 'Tab') return
    const nodes = [...(panel.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])]
    if (nodes.length === 0) return
    const first = nodes[0]
    const last = nodes[nodes.length - 1]
    if (!first || !last) return
    // Wrap by hand: the browser's own order leaves the dialog at either end.
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault()
      last.focus()
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault()
      first.focus()
    }
  }

  return (
    // The backdrop closes on a click and is marked presentational: its keyboard
    // equivalent is Escape, which the dialog itself handles, and giving the
    // backdrop a key handler of its own would put a second focusable element
    // between the person and the dialog.
    <div
      role="presentation"
      className="fixed inset-0 z-[90] grid place-items-center"
      style={{ background: 'color-mix(in oklab, var(--bg) 60%, transparent)' }}
      onClick={onClose}
    >
      {/* The Tab trap has to live on the dialog, which is where the keystroke
          arrives: without it Tab walks into the page behind and the dialog is
          on screen with nothing a keyboard reaches. */}
      {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions */}
      <div
        ref={panel}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        tabIndex={-1}
        onClick={(e) => e.stopPropagation()}
        onKeyDown={keydown}
        className="max-w-[92vw] rounded-[10px] border border-line-strong bg-surface shadow-panel outline-none"
        style={{ width: `${width}px`, animation: 'aigem-in .12s ease-out' }}
      >
        <div className="flex items-baseline gap-[10px] border-b border-line px-4 pt-[14px] pb-3">
          <span className="text-[14px] font-semibold tracking-[-0.01em]">{title}</span>
          {subtitle && <span className="font-mono text-[10.5px] text-fg-subtle">{subtitle}</span>}
          <span
            aria-hidden="true"
            className="ml-auto rounded-[3px] border border-line px-[5px] py-px font-mono text-[10px] text-fg-subtle"
          >
            esc
          </span>
        </div>

        <div className="px-4 py-[14px] text-[12.5px] text-pretty text-fg-muted">{children}</div>

        <div className="flex items-center gap-2 border-t border-line px-4 py-3">
          {secondary && (
            <button
              type="button"
              onClick={secondary.onClick}
              className="h-[28px] cursor-pointer rounded-md border border-line bg-transparent px-3 text-[12px] text-fg-muted hover:border-line-strong hover:text-fg"
            >
              {secondary.label}
            </button>
          )}
          <button
            type="button"
            onClick={onClose}
            className="ml-auto h-[28px] cursor-pointer rounded-md border border-line bg-transparent px-3 text-[12px] text-fg-muted hover:border-line-strong hover:text-fg"
          >
            {cancelLabel}
          </button>
          {confirm && (
            <button
              type="button"
              onClick={confirm.onClick}
              disabled={confirm.disabled}
              className="h-[28px] cursor-pointer rounded-md px-3 text-[12px] font-medium text-bg disabled:cursor-not-allowed disabled:opacity-50 hover:brightness-110"
              style={{
                background: confirm.danger ? 'var(--danger)' : 'var(--primary)',
                border: `1px solid ${confirm.danger ? 'var(--danger)' : 'var(--primary)'}`,
              }}
            >
              {confirm.label}
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
