import { useCallback, useRef, useState } from 'react'

/**
 * One tab stop for a whole widget, with the arrows moving inside it.
 *
 * A grid or a listbox that gives every row `tabIndex={0}` is forty tab presses
 * to cross, and a screen reader that sees the role switches to application mode
 * and hands the arrow keys to a widget where nothing is listening - so the role
 * makes the thing *harder* to read than a plain list would be. This is the
 * other half of the pattern the roles promise.
 */
export function useRoving(count: number) {
  const [active, setActive] = useState(0)
  const box = useRef<HTMLElement | null>(null)
  // A callback ref rather than a ref object: what the widget needs is a way to
  // find its rows when a key is pressed, and handing the object out to be read
  // in a `ref=` prop is the shape the React lint rules warn about.
  const container = useCallback((el: HTMLElement | null) => {
    box.current = el
  }, [])
  const at = Math.min(active, Math.max(0, count - 1))

  const move = useCallback(
    (to: number) => {
      const next = Math.max(0, Math.min(count - 1, to))
      setActive(next)
      // Focus follows, because the tab stop it just became is only useful if
      // the person is standing on it.
      const rows = box.current?.querySelectorAll<HTMLElement>('[data-roving]')
      rows?.[next]?.focus()
    },
    [count],
  )

  const onKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      switch (e.key) {
        case 'ArrowDown':
        case 'ArrowRight':
          e.preventDefault()
          move(at + 1)
          return true
        case 'ArrowUp':
        case 'ArrowLeft':
          e.preventDefault()
          move(at - 1)
          return true
        case 'Home':
          e.preventDefault()
          move(0)
          return true
        case 'End':
          e.preventDefault()
          move(count - 1)
          return true
        default:
          return false
      }
    },
    [at, count, move],
  )

  return { container, active: at, setActive, onKeyDown }
}
