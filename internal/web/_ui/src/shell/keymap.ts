/**
 * The global keyboard map, and the one predicate the whole thing turns on.
 *
 * The layers are ordered as the canvas orders them: Escape closes everything,
 * a confirm takes Enter, a modal takes Cmd/Ctrl+Enter, the palette takes the
 * arrows and Enter, and only past all of that do the screen's own single-letter
 * bindings apply.
 */

/**
 * Whether a keystroke belongs to whatever the person is typing in.
 *
 * Without this, `/` inside the message box focuses the filter instead of typing
 * a slash - which is the bug that makes an application feel hostile, and the
 * reason this is a named function with its own test rather than a condition
 * inside a handler.
 */
export function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false
  const tag = target.tagName.toLowerCase()
  if (tag === 'input' || tag === 'textarea' || tag === 'select') return true
  // contentEditable is "true", "plaintext-only" or "inherit"; only "false"
  // means the element does not take typing. Compared rather than returned: the
  // property is absent on a plain element in some engines, and an undefined
  // here would be a falsy value where the signature promises a boolean.
  return target.isContentEditable === true
}

export type Layers = {
  /** Anything on top of the application: palette, quick chat, a dialog. */
  anyOpen: boolean
  confirmOpen: boolean
  modalOpen: boolean
  paletteOpen: boolean
}

export type KeyActions = {
  closeLayers: () => void
  togglePalette: () => void
  toggleQuick: () => void
  confirm: () => void
  submitModal: () => void
  paletteMove: (delta: number) => void
  paletteRun: () => void
  focusFilter: () => void
}

/** Returns true when the keystroke was handled and must not reach the page. */
export function handleKey(e: KeyboardEvent, layers: Layers, actions: KeyActions): boolean {
  const mod = e.metaKey || e.ctrlKey

  if (mod && e.key.toLowerCase() === 'k') {
    actions.togglePalette()
    return true
  }
  if (mod && e.key.toLowerCase() === 'j') {
    actions.toggleQuick()
    return true
  }
  if (e.key === 'Escape') {
    if (!layers.anyOpen) return false
    actions.closeLayers()
    return true
  }
  if (layers.confirmOpen) {
    // Deliberately no Enter binding, against the canvas, which has one.
    //
    // A dialog opens with focus on Cancel, and a global Enter here would both
    // run the destructive action and preventDefault the Cancel button the
    // person was actually looking at - so the keystroke that reads as "dismiss
    // this" would end a session instead. Enter reaches the focused button on
    // its own, which does the safe thing; Escape closes. Nothing else passes to
    // the page behind a dialog that is asking a question.
    return false
  }
  if (layers.modalOpen) {
    if (mod && e.key === 'Enter') {
      actions.submitModal()
      return true
    }
    return false
  }
  if (layers.paletteOpen) {
    if (e.key === 'ArrowDown') {
      actions.paletteMove(1)
      return true
    }
    if (e.key === 'ArrowUp') {
      actions.paletteMove(-1)
      return true
    }
    if (e.key === 'Enter') {
      actions.paletteRun()
      return true
    }
    return false
  }
  if (isTypingTarget(e.target)) return false
  if (e.key === '/') {
    actions.focusFilter()
    return true
  }
  return false
}
