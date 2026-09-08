import { useApp } from '@/state/app'

/** One line, bottom centre, with a tick. It says what happened, once. */
export function ToastHost() {
  const toast = useApp((s) => s.toast)
  return (
    // The region is always in the document and only its text changes. A screen
    // reader announces a mutation inside a region it was already observing;
    // inserting the region and its text in one commit - which is what a
    // conditional render does - is announced by nothing.
    <div role="status" aria-live="polite" className="sr-only">
      {toast}
    </div>
  )
}

/** The visible plate. It carries no role: the region above does the speaking. */
export function Toast() {
  const toast = useApp((s) => s.toast)
  if (!toast) return null
  return (
    <div
      aria-hidden="true"
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
