import { useApp } from '@/state/app'

/** One line, bottom centre, with a tick. It says what happened, once. */
export function ToastHost() {
  const toast = useApp((s) => s.toast)
  if (!toast) return null
  return (
    <div
      role="status"
      aria-live="polite"
      className="fixed bottom-[34px] left-1/2 z-[95] flex -translate-x-1/2 items-center gap-[9px] rounded-[7px] border border-line-strong bg-surface px-3 py-2 text-[12px] shadow-panel"
      style={{ animation: 'aigem-in .12s ease-out' }}
    >
      <span aria-hidden="true" className="text-[10px] text-success">
        ✓
      </span>
      <span className="text-fg-muted">{toast}</span>
    </div>
  )
}
