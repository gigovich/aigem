import { renderHook, act } from '@testing-library/react'
import { expect, test, vi } from 'vitest'
import { createStore, useStore } from './store'

test('notifies subscribers when the snapshot changes, and not when it does not', () => {
  const store = createStore({ n: 1 })
  const listener = vi.fn()
  const stop = store.subscribe(listener)

  store.set({ n: 2 })
  expect(listener).toHaveBeenCalledTimes(1)

  // Identity, not equality: the same object is not a change.
  const same = store.get()
  store.set(same)
  expect(listener).toHaveBeenCalledTimes(1)

  stop()
  store.set({ n: 3 })
  expect(listener).toHaveBeenCalledTimes(1)
})

// A listener that unsubscribes while the set is being walked would otherwise
// mutate the collection under the iteration.
test('a subscriber may unsubscribe from inside its own notification', () => {
  const store = createStore(0)
  const stop = store.subscribe(() => stop())
  const other = vi.fn()
  store.subscribe(other)

  expect(() => store.set(1)).not.toThrow()
  expect(other).toHaveBeenCalledTimes(1)
})

test('a component re-renders only when its own slice moves', () => {
  const store = createStore({ a: 1, b: 1 })
  const renders = vi.fn()
  const { result } = renderHook(() => {
    renders()
    return useStore(store, (s) => s.a)
  })

  expect(result.current).toBe(1)
  const before = renders.mock.calls.length

  act(() => store.set((s) => ({ ...s, b: 2 })))
  // The snapshot changed but the slice did not, so React bails out of the
  // re-render itself once the selector returns the same value.
  expect(result.current).toBe(1)

  act(() => store.set((s) => ({ ...s, a: 5 })))
  expect(result.current).toBe(5)
  expect(renders.mock.calls.length).toBeGreaterThan(before)
})

// The failure this prevents is an infinite render: useSyncExternalStore compares
// snapshots by identity, so a selector that builds an object has to be cached or
// every read looks like a change.
test('a selector that builds an object returns the same one until the state moves', () => {
  const store = createStore({ a: 1, b: 2 })
  const select = (s: { a: number; b: number }) => ({ a: s.a })
  const { result, rerender } = renderHook(() => useStore(store, select))

  const first = result.current
  rerender()
  expect(result.current).toBe(first)

  act(() => store.set((s) => ({ ...s, a: 9 })))
  expect(result.current).not.toBe(first)
  expect(result.current).toEqual({ a: 9 })
})

// Caching on the state alone would ignore a selector that closed over a new
// prop: the component would go on showing the previous prop's slice until
// something unrelated happened to move the store.
test('a new selector is applied even when the state has not moved', () => {
  const items: Record<string, number> = { x: 1, y: 2 }
  const store = createStore({ items })
  const { result, rerender } = renderHook(
    ({ key }: { key: string }) => useStore(store, (s) => s.items[key]),
    { initialProps: { key: 'x' } },
  )

  expect(result.current).toBe(1)
  rerender({ key: 'y' })
  expect(result.current).toBe(2)
})
